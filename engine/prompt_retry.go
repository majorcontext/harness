package engine

import (
	"context"
	"errors"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// basePromptRetryBackoffBase and basePromptRetryBackoffCap define the base
// interactive Prompt loop's retry backoff (see streamTurnWithRetry): attempt
// 1 waits basePromptRetryBackoffBase (1s), each later attempt doubles it,
// capped at basePromptRetryBackoffCap. With the default PromptRetries (2)
// only 1s then 2s are ever used; the cap bounds any future increase so a
// single wait can never grow unboundedly.
//
// This schedule is deliberately far shorter than the goal loop's tiers
// (goalRetryDelay's 1s/4s and goalRetryableBackoff's 5s→5min weather
// schedule, goal.go): an interactive user waits on the turn, so the base loop
// smooths a one-off blip in a second or two rather than riding a long outage.
const (
	basePromptRetryBackoffBase = 1 * time.Second
	basePromptRetryBackoffCap  = 8 * time.Second
)

// basePromptRetryDelay returns the backoff to wait after the given 1-indexed
// attempt has failed, before the next attempt runs.
func basePromptRetryDelay(attempt int) time.Duration {
	d := basePromptRetryBackoffBase
	for i := 1; i < attempt; i++ {
		if d >= basePromptRetryBackoffCap {
			return basePromptRetryBackoffCap
		}
		d *= 2
	}
	if d > basePromptRetryBackoffCap {
		d = basePromptRetryBackoffCap
	}
	return d
}

// waitBasePromptRetryBackoff blocks for basePromptRetryDelay(attempt), or
// until ctx is done, whichever comes first — a deliberate abort ends the wait
// immediately. It uses time.NewTimer with an explicit Stop (never
// time.After) so the timer is released promptly when ctx fires first.
func waitBasePromptRetryBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(basePromptRetryDelay(attempt))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// streamTurnWithRetry wraps streamTurn with a small, bounded retry budget for
// the base interactive Prompt loop, so a one-off retryable provider error (a
// momentary HTTP 5xx/429/529 or a truncated stream) never surfaces to an
// interactive user. It has the exact same signature as streamTurn and is a
// drop-in for the single call site in runAgenticLoop, so that loop's
// structure — its interruptedTurnError handling, emitSessionError, usage
// accounting, and queue drain — is unchanged.
//
// A retry runs only when the error is classified retryable through
// provider.AsRetryable (server_error/overloaded/rate_limited/stream_truncated
// — never by matching error text) AND the budget (s.cfg.PromptRetries
// additional attempts) has one left. Every other error returns on the first
// attempt with ZERO retries:
//
//   - context.Canceled is a deliberate abort, never provider weather.
//   - An *interruptedTurnError carries a partial assistant message the caller
//     must still append to keep history protocol-valid; a silent re-issue
//     would duplicate the model's already-emitted tool intent, so it is never
//     retried here (this is why the interrupted check precedes the retryable
//     one — an interruptedTurnError wraps whatever class the stream died
//     with, which may itself be retryable).
//   - A provider.AsPermanent malformed-request shape, or any deterministic
//     failure, is not retryable, so !retryable returns it immediately —
//     matching the goal loop's fail-fast branch (promptTurnWithRetry).
//
// Retrying streamTurn is idempotent for history and tool side effects:
// streamTurn makes ONE model call and never executes a tool (runAgenticLoop
// runs tools only AFTER streamTurn returns a StopToolUse message), so a
// failed attempt ran no side effect to redo — even a later model call within
// the same turn re-issues against unchanged history. The one shape that DID
// emit tool intent before failing arrives as *interruptedTurnError and is
// excluded above.
//
// The EMIT stream is not idempotent, so this closes the gap explicitly. A
// failed attempt may have already emitted EventTextDelta/EventReasoningDelta
// for partial content before its stream died (a mid-text stream_truncated or
// server_error), and the next attempt re-streams that content from scratch.
// This emits one EventTurnRestart before each retry, so a subscriber that
// renders deltas incrementally drops the stale partial and rebuilds it from
// the retry rather than rendering the two runs concatenated — see
// EventTurnRestart. History still reconciles through the turn's final
// EventMessage regardless.
//
// A COMPLETED turn (streamTurn's err is nil) with no actionable content —
// per turnHasActionableContent, no non-empty Text and no ToolCall part,
// alongside a stop reason other than StopToolUse — is folded into the same
// bounded retry budget via a synthesized *emptyTurnError, rather than
// returned as a success. See emptyTurnError's doc comment (engine.go) for
// the production incident this guards: a provider stream can reach
// EventDone cleanly while reporting nothing the caller can act on (e.g.
// thinking alone consumed the entire max_tokens ceiling), and that must
// never be journaled as a completed turn. This is why the check runs before
// the `err == nil` early return below, not after it.
func (s *Session) streamTurnWithRetry(ctx context.Context) (*message.Message, provider.StopReason, provider.Usage, error) {
	for attempt := 1; ; attempt++ {
		asst, stop, usage, err := s.streamTurn(ctx)
		if err == nil {
			if stop != provider.StopToolUse && !turnHasActionableContent(asst) {
				err = &emptyTurnError{stop: stop, outputTokens: usage.OutputTokens}
			} else {
				return asst, stop, usage, nil
			}
		}
		if errors.Is(err, context.Canceled) {
			return nil, "", provider.Usage{}, err
		}
		var interrupted *interruptedTurnError
		if errors.As(err, &interrupted) {
			return nil, "", provider.Usage{}, err
		}
		// An emptyTurnError is synthesized above, never provider-classified,
		// so it is always eligible for the bounded retry budget below —
		// provider.AsRetryable only applies to a genuine provider/transport
		// error.
		var empty *emptyTurnError
		if !errors.As(err, &empty) {
			if _, retryable := provider.AsRetryable(err); !retryable {
				return nil, "", provider.Usage{}, err
			}
		}
		if attempt > s.cfg.PromptRetries {
			return nil, "", provider.Usage{}, err
		}
		// Signal subscribers to drop any partial deltas the failed attempt
		// emitted before the retry re-streams the same content. A reset on an
		// attempt that emitted nothing is a harmless no-op — the subscriber's
		// in-progress buffer is already empty. See EventTurnRestart.
		s.emit(Event{Type: EventTurnRestart})
		if werr := waitBasePromptRetryBackoff(ctx, attempt); werr != nil {
			// ctx was cancelled during the backoff wait: surface the
			// cancellation rather than retrying against a dead context.
			return nil, "", provider.Usage{}, werr
		}
	}
}
