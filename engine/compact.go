// Context compaction: summarize-and-truncate. See docs/design/
// context-compaction.md for the full design; this file follows it exactly —
// where a comment here and that doc ever disagree, the doc wins.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// Compaction event types (see docs/design/context-compaction.md §4 "Live
// event surface"). EventHistoryCompacted is journaled durably (like
// session.status); EventCompactionFailed is fire-and-forget.
const (
	EventHistoryCompacted = "history.compacted"
	EventCompactionFailed = "compaction.failed"
)

// defaultCompactionThreshold is Config.CompactionThreshold's zero-fills-a-
// default value: the fraction of ContextWindowTokens at which automatic
// compaction triggers.
const defaultCompactionThreshold = 0.8

// defaultCompactionKeepTurns is Config.CompactionKeepTurns's zero-fills-a-
// default value.
const defaultCompactionKeepTurns = 2

// minCompactionKeepTurns is the hard floor on keep_turns (see CompactOptions
// and docs/design/context-compaction.md §1): the most recent turn is never
// foldable, so a session's history can never collapse to a lone summary the
// model would have to answer with zero real context.
const minCompactionKeepTurns = 1

// compactionMaxTokens bounds the summarization call's response — a concise
// summary, not another full turn.
const compactionMaxTokens = 1024

// CompactionSummaryBanner prefixes every synthesized compaction summary
// message's text, mirroring message.SyntheticOrphanResultText's spirit: a
// transcript or GET /session/{id}/message reader can never mistake it for
// something the human actually typed.
const CompactionSummaryBanner = "[compacted summary of earlier conversation]\n\n"

// compactionSystemPrompt is the dedicated system prompt for the tool-less
// summarization call (see Session.Compact): concise, information-preserving,
// never tool-call minutiae verbatim.
const compactionSystemPrompt = `You are summarizing a prefix of an ongoing agent conversation so it can be folded into one message, freeing context for future turns.

Write a concise, information-preserving summary. Preserve:
- the user's intent and goals
- decisions made and their rationale
- concrete facts a later turn depends on: file paths, commands, values, error text

Do not transcribe tool-call arguments or outputs verbatim; describe what happened and why it matters instead. Be dense; omit anything a later turn would not need.`

// CompactOptions configures one call to Session.Compact.
type CompactOptions struct {
	// KeepTurns overrides Config.CompactionKeepTurns for this call only.
	// Zero (the default) uses the config value (itself defaulting to 2
	// when zero). Whatever the source, the effective value is floored at
	// minCompactionKeepTurns.
	KeepTurns int
	// Model overrides Config.CompactionModel / the session's own current
	// model for this call only. Zero uses Config.CompactionModel, and if
	// that is also zero, the session's current model (see Session.Model).
	Model message.ModelRef
}

// CompactResult is the outcome of a successful Session.Compact call.
// TurnsFolded is 0 (not an error) when there was nothing worth folding —
// fewer than the keep-turns floor's worth of complete turns exist yet.
type CompactResult struct {
	TurnsFolded int
	FirstID     string
	LastID      string
	Summary     *message.Message
}

// effectiveKeepTurns resolves CompactOptions.KeepTurns/Config.
// CompactionKeepTurns down to one concrete, floored value.
func (s *Session) effectiveKeepTurns(optKeepTurns int) int {
	keep := optKeepTurns
	if keep <= 0 {
		keep = s.cfg.CompactionKeepTurns
	}
	if keep <= 0 {
		keep = defaultCompactionKeepTurns
	}
	if keep < minCompactionKeepTurns {
		keep = minCompactionKeepTurns
	}
	return keep
}

// turnBoundaries returns the indices within history of every message that
// starts a turn: a RoleUser message. A turn runs from one such index up to
// (not including) the next, or end of history — see docs/design/
// context-compaction.md §2.
func turnBoundaries(history []message.Message) []int {
	var starts []int
	for i, m := range history {
		if m.Role == message.RoleUser {
			starts = append(starts, i)
		}
	}
	return starts
}

// Compact folds a contiguous prefix of whole turns into one synthetic
// summary message, durably, in place. It is the single entry point both the
// automatic trigger (maybeAutoCompact) and the explicit POST
// /session/{id}/compact endpoint funnel through — see docs/design/
// context-compaction.md §1.
//
// It runs the slow, network-bound summarization call WITHOUT holding s.mu
// (same pattern streamTurn uses via History()), then re-acquires s.mu once
// to splice s.history and persist the compact record in one critical
// section — a concurrent reader of History/Usage/LastUsage sees the pre- or
// post-compaction state, never a half-spliced one.
//
// A result with TurnsFolded == 0 is not an error: fewer than the effective
// keep-turns floor's worth of complete turns exist yet, so there is nothing
// to gain by folding (see §2's minimum-fold rule). Any other failure (the
// summarization call itself errors, or — defense in depth — the computed
// range cannot be found) aborts cleanly: no journal write, no history
// mutation, and an emitted EventCompactionFailed.
func (s *Session) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error) {
	history := s.History()
	keepTurns := s.effectiveKeepTurns(opts.KeepTurns)

	starts := turnBoundaries(history)
	if len(starts) <= keepTurns {
		return CompactResult{}, nil
	}
	foldTurns := len(starts) - keepTurns
	foldStart := starts[0]
	foldEndExclusive := starts[foldTurns] // first KEPT turn's leading RoleUser message
	foldEnd := foldEndExclusive - 1

	firstID := history[foldStart].ID
	lastID := history[foldEnd].ID

	model := opts.Model
	if model.IsZero() {
		model = s.cfg.CompactionModel
	}
	if model.IsZero() {
		model = s.Model()
	}

	summaryText, usage, err := s.runCompactionSummary(ctx, model, history[foldStart:foldEnd+1])
	if err != nil {
		s.emit(Event{Type: EventCompactionFailed, Text: err.Error()})
		return CompactResult{}, err
	}

	summary := message.Message{
		ID:        newID("msg"),
		Role:      message.RoleUser,
		Parts:     message.Parts{&message.Text{Text: CompactionSummaryBanner + summaryText}},
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	spliced, err := spliceCompact(s.history, firstID, lastID, summary)
	if err != nil {
		s.mu.Unlock()
		s.emit(Event{Type: EventCompactionFailed, Text: err.Error()})
		return CompactResult{}, err
	}
	s.history = spliced
	// Cumulative usage only (see docs/design/context-compaction.md's "Usage
	// accounting"): NEVER touch lastUsage/haveLastUsage here — the
	// automatic trigger reads LastUsage as "how large is the next worker
	// request", and this small summarization call would mask the very
	// pressure that triggered compaction.
	s.usage.InputTokens += usage.InputTokens
	s.usage.OutputTokens += usage.OutputTokens
	s.usage.CacheReadTokens += usage.CacheReadTokens
	s.usage.CacheWriteTokens += usage.CacheWriteTokens
	s.compactCount++
	s.lastCompactedAt = summary.CreatedAt
	s.persistCompactLocked(firstID, lastID, foldTurns, summary, usage)
	s.mu.Unlock()

	// Live event surface (§4): the summary flows through the ordinary
	// message-event path FIRST, so an events.jsonl tailer receives the
	// summary content before it ever sees history.compacted — the durable
	// compact record carries the summary inline rather than as a
	// recMessage, so without this emission a tailer would hold a dangling
	// id for a message it never received.
	s.emit(Event{Type: EventMessage, Message: &summary})
	s.emit(Event{
		Type:               EventHistoryCompacted,
		CompactFirstID:     firstID,
		CompactLastID:      lastID,
		CompactTurnsFolded: foldTurns,
		CompactSummaryID:   summary.ID,
	})

	return CompactResult{
		TurnsFolded: foldTurns,
		FirstID:     firstID,
		LastID:      lastID,
		Summary:     &summary,
	}, nil
}

// runCompactionSummary issues the tool-less summarization call: a request
// built from exactly the folded range's messages (independently
// transcodable — a whole-turns range never has a dangling tool call at
// either edge) plus the dedicated compaction system prompt. Mirrors the
// evaluator shape goal.go's runEvaluator already establishes, but sends the
// folded messages directly rather than a rendered transcript, since (unlike
// the evaluator's cross-cutting judge call) this range is always
// transcodable as-is.
func (s *Session) runCompactionSummary(ctx context.Context, model message.ModelRef, folded []message.Message) (string, provider.Usage, error) {
	prov, err := s.cfg.Providers.For(model)
	if err != nil {
		return "", provider.Usage{}, err
	}
	req := &provider.Request{
		Model:     model,
		System:    []string{compactionSystemPrompt},
		Messages:  append([]message.Message(nil), folded...),
		MaxTokens: compactionMaxTokens,
	}
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return "", provider.Usage{}, err
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	var usage provider.Usage
	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", provider.Usage{}, err
		}
		switch ev.Type {
		case provider.EventTextDelta:
			deltas.WriteString(ev.Text)
		case provider.EventDone:
			usage = ev.Usage
			if ev.Message != nil {
				doneText = ev.Message.Parts.Text()
			}
		}
	}
	text := doneText
	if text == "" {
		text = deltas.String()
	}
	if strings.TrimSpace(text) == "" {
		return "", provider.Usage{}, errors.New("engine: compaction summary was empty")
	}
	return text, usage, nil
}

// spliceCompact replaces history[start..end] (the messages named firstID and
// lastID, inclusive) with summary, returning a fresh slice that never
// aliases history's backing array. Shared by the live Compact path above and
// LoadSession's recCompact replay (see store.go) so the two can never drift
// apart. firstID/lastID not found (in order) within history is corruption —
// an explicit error, never a silent best-effort guess.
func spliceCompact(history []message.Message, firstID, lastID string, summary message.Message) ([]message.Message, error) {
	start, end := -1, -1
	for i, m := range history {
		if start == -1 && m.ID == firstID {
			start = i
		}
		if start != -1 && m.ID == lastID {
			end = i
			break
		}
	}
	if start == -1 || end == -1 {
		return nil, fmt.Errorf("engine: compact record range [%s, %s] not found in history", firstID, lastID)
	}
	out := make([]message.Message, 0, len(history)-(end-start+1)+1)
	out = append(out, history[:start]...)
	out = append(out, summary)
	out = append(out, history[end+1:]...)
	return out, nil
}

// maybeAutoCompact is Prompt's automatic-trigger check (see docs/design/
// context-compaction.md §1): a no-op unless Config.ContextWindowTokens is
// positive (opt-in) and at least one turn has completed. Best-effort: a
// failed or skipped compaction never blocks the caller's real turn — the
// turn simply proceeds uncompacted, at the same risk layer 1's
// context-overflow classification already handles if it actually overflows.
func (s *Session) maybeAutoCompact(ctx context.Context) {
	s.mu.Lock()
	windowTokens := s.cfg.ContextWindowTokens
	threshold := s.cfg.CompactionThreshold
	lastUsage := s.lastUsage
	haveLastUsage := s.haveLastUsage
	onCooldown := s.compactHysteresis
	s.mu.Unlock()

	if windowTokens <= 0 || !haveLastUsage {
		return
	}
	if threshold <= 0 {
		threshold = defaultCompactionThreshold
	}
	// The prompt occupies the context window as the SUM of all three
	// input components. Harness injects cache_control by default, so on a
	// warm session the Anthropic adapter reports most of the prompt in
	// CacheReadTokens (new prefix growth in CacheWriteTokens) while
	// InputTokens is only the uncached tail — counting InputTokens alone
	// meant auto-compaction never fired in exactly the long-cached-session
	// shape it exists for.
	promptTokens := lastUsage.InputTokens + lastUsage.CacheReadTokens + lastUsage.CacheWriteTokens
	over := float64(promptTokens) >= threshold*float64(windowTokens)
	if !over {
		// Churn-guard reset: LastUsage has dipped below the threshold at
		// least once since the last automatic compaction, so a future
		// crossing is allowed to trigger again.
		if onCooldown {
			s.mu.Lock()
			s.compactHysteresis = false
			s.mu.Unlock()
		}
		return
	}
	if onCooldown {
		// Churn guard (§2): still over threshold since the last automatic
		// compaction. The pressure must live in the kept region (a single
		// giant tool result) — folding the prefix again cannot relieve it,
		// so do not re-fire every turn.
		return
	}

	res, err := s.Compact(ctx, CompactOptions{})
	if err != nil {
		// Best-effort: EventCompactionFailed already emitted inside
		// Compact. The turn proceeds uncompacted.
		return
	}
	if res.TurnsFolded > 0 {
		s.mu.Lock()
		s.compactHysteresis = true
		s.mu.Unlock()
	}
}
