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

	// spliceFirstID/spliceLastID name the fold range as it actually sits in
	// LIVE history, which can include a message.ResolveOrphanToolCalls
	// synthetic repair message (see engine/store.go's LoadSession, which
	// applies that repair to live history AFTER replay). The fold RANGE is
	// correct either way; only the ID used to splice LIVE history needs the
	// exact live boundary, synthetic or not.
	spliceFirstID := history[foldStart].ID
	spliceLastID := history[foldEnd].ID

	// journaledFirstID/journaledLastID are the durable record's IDs (NEP-
	// 5292): a synthetic repair message is never itself persisted, so a
	// journal record naming one is unloadable forever afterward (see
	// message.IsSyntheticOrphanID's doc comment). Walk to the nearest real,
	// persisted message on each edge before writing anything durable. The
	// synthetic message does not exist in raw replayed history at all, so
	// folding it live while journaling the last real ID before it produces
	// identical kept-history CONTENT on both the live path and a future
	// reload — see TestCompactNeverJournalsSyntheticOrphanID.
	//
	// Content, not byte-identical messages: a synthetic that survives in the
	// KEPT range gets a different ID after a reload, because
	// ResolveOrphanToolCalls numbers afterIndex against the spliced slice on
	// reload but against full history live. That difference is cosmetic and
	// cannot reach disk — a synthetic ID is never persisted (this function
	// is what guarantees it) and never survives transcode.
	//
	// # Version skew: this is a persisted-format change, verified both ways
	//
	// The compactRecord SHAPE is unchanged (FirstID/LastID/TurnsFolded/
	// Summary, same fields, same json tags) — only the VALUE a fixed
	// Session.Compact chooses for LastID differs from an unpatched build's.
	// That value is always a real, persisted message id, so an OLD binary
	// (no heal path, plain spliceCompact) reading a log THIS fixed code
	// wrote replays it correctly with no changes on its side: the id it
	// looks for is one that was always in raw history, at exactly the same
	// index a synthetic-aware reader would land on after healing. See
	// TestCompactNewRecordReplaysIdenticallyWithoutHealPath, which asserts
	// this directly by calling spliceCompact with no heal involved at all
	// and comparing to the live result — this is what makes downgrading to
	// an old binary after this fix safe. The reverse direction (a NEW
	// binary reading an OLD log that already carries a phantom synthetic
	// LastID) is store.go's heal path (Part B), covered by
	// TestLoadSessionHealsPhantomSyntheticCompactLastID.
	journaledFirstID, ok := nonSyntheticIDForward(history, foldStart, foldEnd)
	if !ok {
		err := fmt.Errorf("engine: compact fold start at index %d has no persisted message id to journal", foldStart)
		s.emit(Event{Type: EventCompactionFailed, Text: err.Error()})
		return CompactResult{}, err
	}
	journaledLastID, ok := nonSyntheticIDBackward(history, foldStart, foldEnd)
	if !ok {
		err := fmt.Errorf("engine: compact fold end at index %d has no persisted message id to journal", foldEnd)
		s.emit(Event{Type: EventCompactionFailed, Text: err.Error()})
		return CompactResult{}, err
	}

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

	summary.Normalize()

	s.mu.Lock()
	spliced, err := spliceCompact(s.history, spliceFirstID, spliceLastID, summary)
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
	// Journal only the real, persisted boundary IDs (see journaledFirstID/
	// journaledLastID's doc comment above) — never the live splice IDs,
	// which can name a synthetic message that will never exist on replay.
	s.persistCompactLocked(journaledFirstID, journaledLastID, foldTurns, summary, usage)
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
		CompactFirstID:     journaledFirstID,
		CompactLastID:      journaledLastID,
		CompactTurnsFolded: foldTurns,
		CompactSummaryID:   summary.ID,
	})

	return CompactResult{
		TurnsFolded: foldTurns,
		FirstID:     journaledFirstID,
		LastID:      journaledLastID,
		Summary:     &summary,
	}, nil
}

// nonSyntheticIDForward walks forward from index i (within [start, end]
// inclusive) and returns the ID of the first message that is not a
// message.ResolveOrphanToolCalls synthetic (see message.IsSyntheticOrphanID).
// foldStart is always a turn boundary — a RoleUser message — so it should
// never itself be synthetic; this walk is defensive only. ok is false when
// every message in [start, end] is synthetic, which should never happen
// (start itself is a turn boundary) but is guarded rather than assumed.
func nonSyntheticIDForward(history []message.Message, start, end int) (id string, ok bool) {
	for i := start; i <= end; i++ {
		if !message.IsSyntheticOrphanID(history[i].ID) {
			return history[i].ID, true
		}
	}
	return "", false
}

// nonSyntheticIDBackward walks backward from index end down to start
// (inclusive) and returns the ID of the first message that is not a
// message.ResolveOrphanToolCalls synthetic (see message.IsSyntheticOrphanID)
// — the nearest real, persisted message at or before the fold's end. ok is
// false when every message in [start, end] is synthetic (the walk-back-
// crosses-before-foldStart guard): that range can then never be journaled,
// and the caller must fail loudly rather than journal a phantom ID anyway.
func nonSyntheticIDBackward(history []message.Message, start, end int) (id string, ok bool) {
	for i := end; i >= start; i-- {
		if !message.IsSyntheticOrphanID(history[i].ID) {
			return history[i].ID, true
		}
	}
	return "", false
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
	// The summarizer's stream gets the same idle watchdog worker turns get
	// (see armIdleWatchdog): maybeAutoCompact runs at the top of every
	// Prompt, so a permanently silent summarizer stream would wedge the
	// very turn it was trying to protect.
	ctx, watch, release := s.armIdleWatchdog(ctx)
	defer release()
	stream, err := prov.Stream(ctx, req)
	if err != nil {
		return "", provider.Usage{}, watch.explain(err)
	}
	defer stream.Close()

	var deltas strings.Builder
	var doneText string
	var usage provider.Usage
	for {
		ev, err := stream.Next()
		watch.kick()
		// Identity comparison, deliberately not errors.Is: a truncated
		// stream's classified error wraps an underlying io.EOF, and
		// folding real history into a summary the model never finished is
		// silent data loss — see goal.go's evaluateGoal loop for the same
		// rule and TestCompactTruncatedSummaryNeverFolds.
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", provider.Usage{}, watch.explain(err)
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

// indexOfMessageID returns the index of the first message in history whose
// ID equals id, and whether one was found. Used by LoadSession's recCompact
// replay (store.go) to decide, BEFORE calling spliceCompact, whether a
// record's LastID needs NEP-5292's heal path below.
func indexOfMessageID(history []message.Message, id string) (int, bool) {
	for i, m := range history {
		if m.ID == id {
			return i, true
		}
	}
	return 0, false
}

// healCompactFoldEnd re-derives a compact record's fold-end message ID when
// the recorded LastID cannot be found verbatim in replayed history
// (NEP-5292, candidate fix 3): an unpatched build could journal
// message.ResolveOrphanToolCalls's synthetic repair-message ID as LastID,
// but that message is minted fresh on every LoadSession, AFTER the scan
// loop that calls this runs — it was never itself persisted, so this replay
// can never see it. Re-derives from firstID's position within history plus
// the record's own turnsFolded count (using turnBoundaries, the same turn-
// boundary logic Session.Compact itself uses to compute a fold range) —
// deliberately NOT by parsing call IDs out of the synthetic ID string: that
// format joins call IDs with "-", which is ambiguous whenever a call ID
// itself contains a "-".
//
// Returns an error — never a silent guess — when firstID itself cannot be
// found (still corruption, unhealable), when firstID is not itself a turn
// boundary, or when turnsFolded does not name a KEPT turn boundary that
// actually exists after it.
func healCompactFoldEnd(history []message.Message, firstID string, turnsFolded int) (string, error) {
	firstIdx, found := indexOfMessageID(history, firstID)
	if !found {
		return "", fmt.Errorf("first_id %q not found in history", firstID)
	}
	starts := turnBoundaries(history)
	startPos := -1
	for i, idx := range starts {
		if idx == firstIdx {
			startPos = i
			break
		}
	}
	if startPos == -1 {
		return "", fmt.Errorf("first_id %q at index %d is not a turn boundary", firstID, firstIdx)
	}
	if turnsFolded <= 0 || startPos+turnsFolded >= len(starts) {
		return "", fmt.Errorf("turns_folded %d out of range for %d turn boundaries after first_id %q", turnsFolded, len(starts), firstID)
	}
	foldEndExclusive := starts[startPos+turnsFolded]
	foldEnd := foldEndExclusive - 1
	if foldEnd < firstIdx {
		return "", fmt.Errorf("re-derived fold end %d precedes first_id %q at index %d", foldEnd, firstID, firstIdx)
	}
	return history[foldEnd].ID, nil
}

// bytesPerTokenEstimate is the standard ~4-bytes-per-token heuristic used by
// estimatePromptTokensFromHistory below when a provider's own usage
// accounting is unavailable.
const bytesPerTokenEstimate = 4

// estimatePromptTokensFromHistory is maybeAutoCompact's fallback for the
// 2026-08-06 nimble-pizza incident: a Bedrock-via-gateway route reported
// InputTokens=0, CacheReadTokens=0, CacheWriteTokens=0 on EVERY turn of a
// 631-message session (OutputTokens was correct throughout, so this was a
// prompt-accounting gap on that route, not a dead provider). maybeAutoCompact's
// threshold check sums exactly those three fields; permanently zero meant
// `over` could never become true, so automatic compaction could never fire on
// that route no matter how large history actually grew — the session ran to
// a hard context overflow instead, which (per the goal loop's design) CLEARS
// an active goal rather than parking it: an unrecoverable dead end that
// existed only because the safety net's own trigger signal was silently
// broken.
//
// This walks the actual session history and sums the byte length of every
// part that contributes real content to a future request — Text, ToolCall
// arguments, ToolResult content, Blob payloads/URLs, and Reasoning text —
// then divides by bytesPerTokenEstimate. It is deliberately crude: the goal
// is not an accurate token count (the real transcoder + provider tokenizer
// already do that job when accounting works) but a signal that survives a
// provider reporting nothing at all, so the overflow-prevention layer keeps
// functioning instead of going permanently dark.
func estimatePromptTokensFromHistory(history []message.Message) int {
	var bytes int
	for _, m := range history {
		bytes += estimatePartsBytes(m.Parts)
	}
	return bytes / bytesPerTokenEstimate
}

// estimatePartsBytes sums the content bytes of parts for
// estimatePromptTokensFromHistory, recursing once into ToolResult.Content
// (itself Text/Blob parts only, per ToolResult's doc comment).
func estimatePartsBytes(parts message.Parts) int {
	var bytes int
	for _, p := range parts {
		switch v := p.(type) {
		case *message.Text:
			bytes += len(v.Text)
		case *message.ToolCall:
			bytes += len(v.Name) + len(v.Arguments)
		case *message.ToolResult:
			bytes += len(v.CallID) + estimatePartsBytes(v.Content)
		case *message.Blob:
			bytes += len(v.Data) + len(v.URL)
		case *message.Reasoning:
			bytes += len(v.Text)
		}
	}
	return bytes
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
	// A provider that reports SOME input usage — even a small amount, e.g. a
	// short warm-cache turn — is trusted as-is: promptTokens > 0 here is real
	// accounting and must never be second-guessed. All-zero across every
	// input component on a turn that DID complete (haveLastUsage is true) is
	// a different case entirely: it is missing data, not evidence of a cheap
	// prompt, and treating it as "0 tokens, never over" is exactly the
	// nimble-pizza failure mode (see estimatePromptTokensFromHistory's doc
	// comment). Falling back to the size-derived estimate here keeps this
	// overflow-prevention layer alive on a route with broken input-usage
	// accounting; it is used for this threshold comparison ONLY and is never
	// written into s.usage/lastUsage — real accounting stays untouched (see
	// the "cumulative-only accounting" comment in Compact above).
	if promptTokens == 0 {
		promptTokens = estimatePromptTokensFromHistory(s.History())
	}
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
