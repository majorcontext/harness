// Automatic compaction's context window: derived from the session's MODEL
// when the operator hasn't set one explicitly. See docs/design/
// context-compaction.md for the compaction mechanism itself (sound and
// unchanged by this file) and the jumpy-pizza incident this file exists to
// close: Config.ContextWindowTokens was opt-in, the boxes platform set it
// nowhere, so compaction never armed on any box and a session died with
// "context exhausted: prompt 1136916 tokens > limit 1000000" instead of
// compacting first.
package engine

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
)

// Context-window sources — both the Session's internal bookkeeping and the
// "source" field on the session-start INFO log line (logContextWindowArmed).
const (
	// contextWindowSourceConfig: the operator set Config.ContextWindowTokens
	// explicitly. Wins over any model metadata, permanently for the life of
	// the session (a later model switch never overrides an explicit value —
	// see resolveContextWindow's precedence).
	contextWindowSourceConfig = "config"
	// contextWindowSourceModelDerived: Config.ContextWindowTokens was zero,
	// and the active model's advertised context window (modelmeta) was found
	// and passed the sanity floor.
	contextWindowSourceModelDerived = "model-derived"
	// contextWindowSourceDisabled: Config.ContextWindowTokens was zero AND
	// no usable model metadata was found — automatic compaction is disarmed,
	// identical to today's behavior before this file existed.
	contextWindowSourceDisabled = "disabled"
	// contextWindowSourceOptOut: the operator DELIBERATELY disabled
	// automatic compaction with a negative Config.ContextWindowTokens. It
	// is told apart from contextWindowSourceDisabled on purpose: "disabled"
	// can mean the registry did not recognize the model, which
	// Config.RequireContextWindow turns into a hard refusal, while this is
	// a stated choice that never refuses. See resolveContextWindow.
	contextWindowSourceOptOut = "disabled-by-config"
)

// ErrUnknownContextWindow marks a model ref the context-window registry
// (package modelmeta) does not recognize, so no context window can be
// derived for it.
//
// It exists because the old answer to that lookup was SILENCE: source
// "disabled", compaction_armed=false, and a session that ran anyway with
// no context management at all — until it died with "context exhausted"
// instead of compacting. An unrecognized model is not a state to degrade
// into, it is a configuration the operator has to fix, and
// Config.RequireContextWindow turns it into a loud refusal at the earliest
// point of use. Errors returned for it wrap this sentinel, and their text
// always names the offending ref.
var ErrUnknownContextWindow = errors.New("engine: no context window configured for model")

// unknownContextWindowError builds the operator-facing refusal for ref. The
// text names the ref, says plainly what is missing, and names both ways out
// — an explicit window, or turning the requirement off — because an error
// that only reports a problem makes an operator go read source to act on
// it.
func unknownContextWindowError(ref message.ModelRef) error {
	return fmt.Errorf("%w: unknown model %q: refusing to run without a context window "+
		"(set context_window_tokens for this model, or context_window_required=false to allow it)",
		ErrUnknownContextWindow, ref.String())
}

// minAutoContextWindowTokens is the sanity floor a model-derived context
// window must clear to arm automatic compaction. A metadata value below
// this is far more likely a bug — a zeroed, truncated, or misparsed field
// somewhere upstream — than a real model's actual window, and arming
// compaction on a bogus tiny window would make it fire on nearly every
// turn, folding a session's history into an unusable stream of summaries.
// No entry in modelmeta's table is anywhere close to this floor (the
// smallest is gpt-4's 8_192, itself below the floor and therefore never
// auto-armed either) — 16k is deliberately conservative, not tuned to the
// current table's actual minimum.
const minAutoContextWindowTokens = 16_000

// modelContextWindowLookup is modelmeta.ContextWindow, indirected so tests
// can substitute a fake table without depending on the real one.
var modelContextWindowLookup = modelmeta.ContextWindow

// resolveContextWindow implements Config.ContextWindowTokens's precedence:
// explicit config > model-derived > disabled. explicitTokens is
// Config.ContextWindowTokens exactly as the operator set it (0 when unset —
// never the already-resolved value from a previous call); model is the ref
// to derive from when explicitTokens is 0. Returns the effective window (0
// when disabled) and which source produced it.
// A registry MISS is reported through miss (wrapping
// ErrUnknownContextWindow) INSTEAD of being folded into a silent
// "disabled" answer. resolveContextWindow does not decide what to do about
// it: the caller does, from Config.RequireContextWindow, so the definition
// of a miss lives in exactly one place and the policy lives with the
// session that has to honor it. miss is nil for every legitimate way to
// end up without a window — an explicit operator window, an explicit
// opt-out, no model at all, or a model the registry knows whose window is
// simply below the auto-arm floor.
func resolveContextWindow(explicitTokens int, model message.ModelRef) (tokens int, source string, miss error) {
	if explicitTokens > 0 {
		return explicitTokens, contextWindowSourceConfig, nil
	}
	if explicitTokens < 0 {
		// A stated choice, not a gap: the operator asked for no automatic
		// compaction. Never a miss, whatever the model is.
		return 0, contextWindowSourceOptOut, nil
	}
	if model.IsZero() {
		// No model to look up. An embedder that has not chosen one yet is
		// not running anything against it either, so there is nothing to
		// refuse — the refusal belongs to whatever later names a model.
		return 0, contextWindowSourceDisabled, nil
	}
	got, ok := modelContextWindowLookup(model)
	if !ok {
		return 0, contextWindowSourceDisabled, unknownContextWindowError(model)
	}
	if got < minAutoContextWindowTokens {
		// INFO, not WARN: the table legitimately keeps some genuinely
		// small windows (gpt-4's true 8_192), so a below-floor value from
		// the static snapshot is a known-good model that is simply too
		// small to auto-arm — not evidence of corruption. Warning here
		// would page a false "metadata bug" alarm on every session that
		// starts on or switches to such a model.
		slog.Info("engine: model-derived context window below auto-compaction floor; compaction disabled",
			"model", model.String(), "tokens", got, "floor", minAutoContextWindowTokens)
		return 0, contextWindowSourceDisabled, nil
	}
	return got, contextWindowSourceModelDerived, nil
}

// requiredContextWindowErr turns a resolveContextWindow miss into this
// session's refusal, or into nothing at all.
//
// It is the ONE place Config.RequireContextWindow is consulted, so the
// policy cannot drift between session start, a model switch, and a resume.
// The ERROR log line fires here rather than at each call site, for the same
// reason: an operator gets the same message with the same fields however
// the miss was reached, and gets it even if the caller ignores the returned
// error entirely. reason names which of those the caller was, mirroring
// logContextWindowArmed's own reason field.
//
// A miss with the requirement OFF is not silent either — it is the state
// logContextWindowArmed already reports as source=disabled,
// compaction_armed=false — so nothing is logged here for it.
func requiredContextWindowErr(cfg Config, ref message.ModelRef, miss error, reason string) error {
	if miss == nil || !cfg.RequireContextWindow {
		return nil
	}
	slog.Error("engine: refusing to run: model has no known context window",
		"model", ref.String(),
		"reason", reason,
		"error", miss.Error(),
	)
	return miss
}

// logContextWindowArmed emits the one operator-facing INFO line stating
// whether automatic compaction is armed for this session and why — the
// signal an operator needs to answer "why didn't compaction fire" without
// reading source. Called once at session start (NewSession, LoadSession)
// with reason "start", and again from SetModel with reason "model_switch"
// whenever a switch actually changes the effective window (never on a
// no-op switch, and never at all when the window is config-pinned — see
// SetModel).
func logContextWindowArmed(sessionID string, model message.ModelRef, tokens int, source, reason string) {
	slog.Info("engine: context window",
		"session", sessionID,
		"model", model.String(),
		"context_window_tokens", tokens,
		"source", source,
		"compaction_armed", tokens > 0,
		"reason", reason,
	)
}
