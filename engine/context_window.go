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
)

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
func resolveContextWindow(explicitTokens int, model message.ModelRef) (tokens int, source string) {
	if explicitTokens > 0 {
		return explicitTokens, contextWindowSourceConfig
	}
	got, ok := modelContextWindowLookup(model)
	if !ok {
		return 0, contextWindowSourceDisabled
	}
	if got < minAutoContextWindowTokens {
		slog.Warn("engine: ignoring implausible model-derived context window",
			"model", model.String(), "tokens", got, "floor", minAutoContextWindowTokens)
		return 0, contextWindowSourceDisabled
	}
	return got, contextWindowSourceModelDerived
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
