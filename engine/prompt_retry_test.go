package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

// flakyProvider fails its first failN Stream calls with err, then serves ok
// as a completed turn. It models a provider that returns a one-off HTTP
// 5xx/429/529 (a Stream dial error) and then recovers.
type flakyProvider struct {
	name  string
	failN int
	err   error
	ok    []provider.Event
	calls int
}

func (p *flakyProvider) Name() string { return p.name }

func (p *flakyProvider) Stream(_ context.Context, _ *provider.Request) (provider.Stream, error) {
	p.calls++
	if p.calls <= p.failN {
		return nil, p.err
	}
	return &scriptedStream{events: p.ok}, nil
}

// retryableServerErr builds the classified error an adapter returns for a
// transient provider-side 5xx (see provider.MarkRetryable /
// classifyStatus) — the exact class the base loop must smooth over.
func retryableServerErr() error {
	return provider.MarkRetryable(errors.New("upstream returned 500"), provider.RetryableServerError)
}

func sessionErrorCount(events []plugin.Event) int {
	n := 0
	for _, ev := range events {
		if ev.Type == plugin.EventSessionError {
			n++
		}
	}
	return n
}

// TestPromptRetriesRetryableThenSucceeds is the red-first guard for the base
// loop masking a one-off retryable provider error. Attempt 1 fails with a
// classified server_error; attempt 2 succeeds. The turn must complete and no
// session.error must reach a plugin.
//
// Red-verify: against the pre-fix runAgenticLoop (streamTurn called once, its
// error returned straight to the caller), Prompt returns the error, final is
// nil, and one session.error is emitted — every assertion below fails.
func TestPromptRetriesRetryableThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &flakyProvider{
			name:  "test",
			failN: 1,
			err:   retryableServerErr(),
			ok:    asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		}
		hooks := &fakeHooks{}
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			Hooks:         hooks,
			PromptRetries: 2,
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt err = %v, want nil (a one-off retryable error must be masked)", err)
		}
		if final == nil || final.Parts.Text() != "done" {
			t.Fatalf("final = %+v, want %q", final, "done")
		}
		if prov.calls != 2 {
			t.Errorf("Stream calls = %d, want 2 (one failure, then success)", prov.calls)
		}
		if n := sessionErrorCount(hooks.events); n != 0 {
			t.Errorf("session.error events = %d, want 0 (the retry masked the blip)", n)
		}
	})
}

// TestPromptRetryEmitsTurnRestartBeforeReStream is the red-first guard for a
// base-loop retry re-emitting a failed attempt's partial deltas. Attempt 1
// streams "Hello wor" then dies with a retryable server_error before any tool
// call; attempt 2 re-streams "Hello wor" + "ld" and finishes. Between the two
// runs streamTurnWithRetry must emit exactly one EventTurnRestart, so an
// incremental subscriber clears the stale partial and renders the retry text
// once — never the two runs concatenated as "Hello worHello world".
//
// Red-verify the NAMED mechanism: delete the s.emit(Event{Type:
// EventTurnRestart}) line in streamTurnWithRetry and this test fails two ways
// — the restart count is 0, and the reconstructed client buffer reads the
// doubled "Hello worHello world".
func TestPromptRetryEmitsTurnRestartBeforeReStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &diesAfterToolCallProvider{
			name:   "test",
			dying:  []provider.Event{{Type: provider.EventTextDelta, Text: "Hello wor"}},
			dieErr: provider.MarkRetryable(io.ErrUnexpectedEOF, provider.RetryableServerError),
			after: [][]provider.Event{
				{
					{Type: provider.EventTextDelta, Text: "Hello wor"},
					{Type: provider.EventTextDelta, Text: "ld"},
					{
						Type:       provider.EventDone,
						Message:    &message.Message{ID: "m_ok", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "Hello world"}}},
						StopReason: provider.StopEndTurn,
					},
				},
			},
		}
		var got []Event
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
			OnEvent:       func(ev Event) { got = append(got, ev) },
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt err = %v, want nil (the retry masks the blip)", err)
		}
		if final == nil || final.Parts.Text() != "Hello world" {
			t.Fatalf("final = %+v, want %q", final, "Hello world")
		}

		// Exactly one restart, and it must sit AFTER the first partial delta
		// (so the subscriber has something to drop) and BEFORE the retry's
		// re-streamed deltas.
		restarts, firstDeltaIdx, restartIdx := 0, -1, -1
		for i, ev := range got {
			switch ev.Type {
			case EventTextDelta:
				if firstDeltaIdx == -1 {
					firstDeltaIdx = i
				}
			case EventTurnRestart:
				restarts++
				restartIdx = i
			}
		}
		if restarts != 1 {
			t.Fatalf("EventTurnRestart count = %d, want exactly 1 before the re-stream", restarts)
		}
		if firstDeltaIdx == -1 || firstDeltaIdx >= restartIdx {
			t.Fatalf("EventTurnRestart at index %d must follow the first text.delta at index %d", restartIdx, firstDeltaIdx)
		}

		// Model an incremental client: accumulate text deltas, clear the
		// in-progress buffer on a restart. With the marker it ends at the
		// single correct text; without it the buffer reads the doubled
		// "Hello worHello world".
		var buf strings.Builder
		for _, ev := range got {
			switch ev.Type {
			case EventTextDelta:
				buf.WriteString(ev.Text)
			case EventTurnRestart:
				buf.Reset()
			}
		}
		if buf.String() != "Hello world" {
			t.Fatalf("client-reconstructed text = %q, want %q (partial deltas not dropped on restart)", buf.String(), "Hello world")
		}
	})
}

// TestPromptRetriesExhaustedSurfaces proves the budget is bounded: a provider
// that fails every attempt with a retryable error still surfaces the failure
// once PromptRetries additional attempts are spent — same session.error the
// caller saw before the budget existed.
func TestPromptRetriesExhaustedSurfaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wantErr := retryableServerErr()
		prov := &flakyProvider{name: "test", failN: 100, err: wantErr}
		hooks := &fakeHooks{}
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			Hooks:         hooks,
			PromptRetries: 2,
		})

		final, err := s.Prompt(context.Background(), "go")
		if err == nil {
			t.Fatal("Prompt err = nil, want the exhausted retryable error to surface")
		}
		if !errors.Is(err, wantErr) {
			t.Errorf("Prompt err = %v, want it to wrap %v", err, wantErr)
		}
		if final != nil {
			t.Errorf("final = %+v, want nil on exhaustion", final)
		}
		// 1 initial attempt + PromptRetries (2) additional = 3 total.
		if prov.calls != 3 {
			t.Errorf("Stream calls = %d, want 3 (1 + 2 retries)", prov.calls)
		}
		if n := sessionErrorCount(hooks.events); n != 1 {
			t.Errorf("session.error events = %d, want exactly 1 (only the final failure)", n)
		}
	})
}

// TestPromptRetriesPermanentFailsFast proves a provider.AsPermanent error —
// a malformed request shape retrying can never fix — gets ZERO retries even
// with a non-zero budget, exactly like the goal loop's fail-fast branch.
func TestPromptRetriesPermanentFailsFast(t *testing.T) {
	permErr := provider.MarkPermanent(errors.New("invalid_request_error: bad tool_use"))
	prov := &flakyProvider{name: "test", failN: 100, err: permErr}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		PromptRetries: 2,
	})

	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt err = nil, want the permanent error to surface")
	}
	if prov.calls != 1 {
		t.Errorf("Stream calls = %d, want 1 (a permanent error is never retried)", prov.calls)
	}
}

// TestPromptRetriesInterruptedNotRetried proves the idempotency guard: an
// interruptedTurnError (a stream that emitted tool_call blocks before dying)
// is never retried, even though it WRAPS a retryable class — runAgenticLoop
// appends its partial message plus synthetic results, so a silent re-issue
// would duplicate the model's tool intent. This red-verifies the NAMED
// mechanism: the interruptedTurnError guard, not the retryable guard.
func TestPromptRetriesInterruptedNotRetried(t *testing.T) {
	// The stream emits one complete tool_call block, then dies with a
	// retryable-classed error before EventDone: streamTurn wraps this as an
	// interruptedTurnError that wraps the retryable error. Without the
	// interrupted guard the retryable guard alone would retry it.
	orphaned := toolCall("tc_orphan", "bash", `{"command":"echo hi"}`)
	prov := &diesAfterToolCallProvider{
		name:   "test",
		dying:  []provider.Event{{Type: provider.EventToolCall, ToolCall: orphaned}},
		dieErr: provider.MarkRetryable(io.ErrUnexpectedEOF, provider.RetryableServerError),
		after: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "recovered"}),
		},
	}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		PromptRetries: 2,
	})

	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt err = nil, want the interrupted turn's error to surface")
	}
	if prov.calls != 1 {
		t.Errorf("Stream calls = %d, want 1 (an interrupted turn is never retried)", prov.calls)
	}
}

// TestPromptRetriesZeroDisables proves PromptRetries == 0 keeps the exact
// pre-fix behavior: the first failure surfaces with no retry.
func TestPromptRetriesZeroDisables(t *testing.T) {
	prov := &flakyProvider{name: "test", failN: 1, err: retryableServerErr()}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		PromptRetries: 0,
	})

	if _, err := s.Prompt(context.Background(), "go"); err == nil {
		t.Fatal("Prompt err = nil, want the first failure to surface with retries disabled")
	}
	if prov.calls != 1 {
		t.Errorf("Stream calls = %d, want 1 (PromptRetries=0 disables retry)", prov.calls)
	}
}

// emptyMaxTokensTurn builds the provider.Event a completed-but-empty turn
// reports: production incident box fx-context-limits (session
// ses_01m0ga6v25f1h902fnmx98zhn3, 2026-08-20) had sonnet-5's thinking
// consume the entire max_tokens ceiling — output_tokens exactly 8192 (4096
// EffortLow budget_tokens + 4096 thinkingCompletionMargin) — before emitting
// any text or tool call, so the assistant message carried only a Reasoning
// part and the provider reported stop_reason "max_tokens". This is a clean
// EventDone, not a Stream error: streamTurn returns (asst, stop, usage, nil)
// exactly like any other completed turn.
func emptyMaxTokensTurn() []provider.Event {
	return []provider.Event{{
		Type:       provider.EventDone,
		Message:    &message.Message{ID: "msg_empty", Role: message.RoleAssistant, Parts: message.Parts{&message.Reasoning{Text: "thinking really hard about it"}}},
		StopReason: provider.StopMaxTokens,
		Usage:      provider.Usage{OutputTokens: 8192},
	}}
}

// TestEmptyTurnRetriesThenSucceeds is the red-first guard for the production
// defect: a completed turn with no actionable content (no Text, no
// ToolCall — here, Reasoning only) must never be journaled as success.
// Attempt 1 reports the empty max_tokens shape above; attempt 2 is a normal
// text turn. streamTurnWithRetry must treat attempt 1 as a retryable
// failure — same bounded budget and EventTurnRestart signal as a transport
// error — and the turn must complete with attempt 2's text.
//
// Red-verify: before the fix, runAgenticLoop's `if stop != StopToolUse {
// return asst, nil }` (engine.go) treats attempt 1 as turn-complete with no
// content validation. Prompt returns attempt 1's Reasoning-only message as
// final (Parts.Text() == "", not "done"), the provider is called exactly
// once, and no EventTurnRestart is emitted — every assertion below fails.
func TestEmptyTurnRetriesThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			emptyMaxTokensTurn(),
			asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		}}
		var got []Event
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
			OnEvent:       func(ev Event) { got = append(got, ev) },
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt err = %v, want nil (the empty turn must be retried and succeed)", err)
		}
		if final == nil || final.Parts.Text() != "done" {
			t.Fatalf("final = %+v, want a message with text %q", final, "done")
		}
		if prov.call != 2 {
			t.Errorf("Stream calls = %d, want 2 (attempt 1 empty, attempt 2 succeeds)", prov.call)
		}
		restarts := 0
		for _, ev := range got {
			if ev.Type == EventTurnRestart {
				restarts++
			}
		}
		if restarts != 1 {
			t.Errorf("EventTurnRestart count = %d, want exactly 1 between the empty attempt and the retry", restarts)
		}
	})
}

// TestEmptyTurnRetriesExhaustedSurfacesError proves the budget is bounded
// and the terminal shape is a real failure, not a silently "completed"
// turn: this reproduces the SECOND, session-terminal occurrence of the
// production defect, where every attempt came back empty and the task died
// silently. Every attempt reports the empty max_tokens shape; Prompt must
// return a non-nil error unwrapping to *emptyTurnError, and history must
// never contain a zero-actionable-parts assistant message.
//
// Red-verify: before the fix, attempt 1 alone is treated as turn-complete
// (see TestEmptyTurnRetriesThenSucceeds's red-verify note) — Prompt returns
// nil error, final is the empty message, only 1 Stream call happens, and
// the empty message IS in history — every assertion below fails.
func TestEmptyTurnRetriesExhaustedSurfacesError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			emptyMaxTokensTurn(),
			emptyMaxTokensTurn(),
			emptyMaxTokensTurn(),
		}}
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
		})

		final, err := s.Prompt(context.Background(), "go")
		if err == nil {
			t.Fatal("Prompt err = nil, want the exhausted empty-turn failure to surface")
		}
		var emptyErr *emptyTurnError
		if !errors.As(err, &emptyErr) {
			t.Fatalf("Prompt err = %v, want it to unwrap to *emptyTurnError", err)
		}
		if final != nil {
			t.Errorf("final = %+v, want nil on exhaustion", final)
		}
		// 1 initial attempt + PromptRetries (2) additional = 3 total.
		if prov.call != 3 {
			t.Errorf("Stream calls = %d, want 3 (1 + 2 retries)", prov.call)
		}
		for _, m := range s.History() {
			if m.Role != message.RoleAssistant {
				continue
			}
			if !turnHasActionableContent(&m) {
				t.Errorf("session history contains a zero-actionable-parts assistant message that must never have been appended: %+v", m)
			}
		}
	})
}

// TestNormalTurnNoEmptyTurnRetry is the existing-behavior guard: an ordinary
// completed turn with real text and StopEndTurn must complete with zero
// extra provider calls — the empty-turn check must never fire for a turn
// that already has actionable content.
func TestNormalTurnNoEmptyTurnRetry(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:     provider.Registry{"test": prov},
		Model:         message.ModelRef{Provider: "test", Model: "m1"},
		PromptRetries: 2,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatalf("Prompt err = %v, want nil", err)
	}
	if final == nil || final.Parts.Text() != "done" {
		t.Fatalf("final = %+v, want a message with text %q", final, "done")
	}
	if prov.call != 1 {
		t.Errorf("Stream calls = %d, want 1 (a normal turn needs no empty-turn retry)", prov.call)
	}
}

// TestEmptyTurnRetriesOnStopToolUseWithNoToolCall is the red-first guard for
// the StopToolUse variant of the same defect: provider/openaicompat's
// mapFinishReason maps the wire finish_reason "tool_calls" to StopToolUse
// unconditionally (openaicompat.go), so a proxied provider that reports
// "tool_calls" with an empty or dropped tool_calls array (observed on the
// bifrost→Fireworks path) produces a StopToolUse turn whose assistant
// message carries no ToolCall part at all — here, Reasoning only, same as
// the max_tokens shape but under the ONE stop reason a naive
// `stop != provider.StopToolUse` gate would treat as automatically safe.
// Attempt 1 reports this shape; attempt 2 is a normal text turn.
// streamTurnWithRetry must retry attempt 1, not treat it as success.
//
// Red-verify the NAMED mechanism: with the guard restored to
// `stop != provider.StopToolUse && !turnHasActionableContent(asst)`
// (the pre-review shape), attempt 1's stop reason IS StopToolUse, so the
// condition is false regardless of content and streamTurnWithRetry returns
// it as success on the first call — runAgenticLoop then finds
// runToolCalls(ctx, asst) returns zero results (no ToolCall parts) and hits
// its own defensive `len(results) == 0` branch (engine.go), which also
// returns asst, nil. Prompt returns attempt 1's Reasoning-only message as
// final (Parts.Text() == "", not "done"), the provider is called exactly
// once, and no EventTurnRestart is emitted — every assertion below fails.
func TestEmptyTurnRetriesOnStopToolUseWithNoToolCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		emptyToolUseTurn := []provider.Event{{
			Type:       provider.EventDone,
			Message:    &message.Message{ID: "msg_empty_tu", Role: message.RoleAssistant, Parts: message.Parts{&message.Reasoning{Text: "deciding what to call"}}},
			StopReason: provider.StopToolUse,
			Usage:      provider.Usage{OutputTokens: 8192},
		}}
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			emptyToolUseTurn,
			asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
		}}
		var got []Event
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
			OnEvent:       func(ev Event) { got = append(got, ev) },
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt err = %v, want nil (the empty StopToolUse turn must be retried and succeed)", err)
		}
		if final == nil || final.Parts.Text() != "done" {
			t.Fatalf("final = %+v, want a message with text %q", final, "done")
		}
		if prov.call != 2 {
			t.Errorf("Stream calls = %d, want 2 (attempt 1 empty under StopToolUse, attempt 2 succeeds)", prov.call)
		}
		restarts := 0
		for _, ev := range got {
			if ev.Type == EventTurnRestart {
				restarts++
			}
		}
		if restarts != 1 {
			t.Errorf("EventTurnRestart count = %d, want exactly 1 between the empty attempt and the retry", restarts)
		}
	})
}

// emptyMaxTokensTurnUsage builds the same empty (Reasoning-only,
// StopMaxTokens) turn shape emptyMaxTokensTurn does, but with an explicit
// usage: a discarded empty attempt bills a full input prefill plus the
// max_tokens output ceiling, not always the same fixed numbers, so a
// usage-accounting test needs distinct per-attempt usage to prove real
// accumulation rather than a coincidental match.
func emptyMaxTokensTurnUsage(usage provider.Usage) []provider.Event {
	return []provider.Event{{
		Type:       provider.EventDone,
		Message:    &message.Message{ID: "msg_empty", Role: message.RoleAssistant, Parts: message.Parts{&message.Reasoning{Text: "thinking really hard about it"}}},
		StopReason: provider.StopMaxTokens,
		Usage:      usage,
	}}
}

// TestEmptyTurnDiscardedAttemptsAccumulateUsage is the red-first guard for
// the discarded-usage defect raised on review: a discarded empty attempt is
// not a provider failure — the call ran to completion and billed real
// tokens (a full input prefill plus the max_tokens output ceiling) — so
// those tokens must still land in cumulative Session.Usage(), exactly like
// the discarded-empty-attempt contract in docs/engine-request-cycle.md (the
// call's real usage still accumulates because it was billed). Dropping it silently
// would undercount GET /session by the full cost of every discarded
// attempt.
//
// Attempts 1 and 2 are empty with distinct usage (so a coincidental match
// can't hide a bug); attempt 3 succeeds. Session.Usage() after Prompt must
// equal the SUM of all three attempts' usage. LastUsage() must equal ONLY
// attempt 3's usage — see accumulateDiscardedTurnUsage's doc comment
// (engine.go) for why a discarded attempt must never update it.
//
// Red-verify: before the fix, streamTurnWithRetry discards attempts 1 and 2
// without ever touching s.usage — Session.Usage() after Prompt reflects
// only attempt 3's usage, missing the ~16k billed-but-empty tokens.
func TestEmptyTurnDiscardedAttemptsAccumulateUsage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempt1Usage := provider.Usage{InputTokens: 1000, OutputTokens: 8192}
		attempt2Usage := provider.Usage{InputTokens: 1010, OutputTokens: 8192}
		attempt3Usage := provider.Usage{InputTokens: 1050, OutputTokens: 12}
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			emptyMaxTokensTurnUsage(attempt1Usage),
			emptyMaxTokensTurnUsage(attempt2Usage),
			{{
				Type:       provider.EventDone,
				Message:    &message.Message{ID: "msg_ok", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
				StopReason: provider.StopEndTurn,
				Usage:      attempt3Usage,
			}},
		}}
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
		})

		final, err := s.Prompt(context.Background(), "go")
		if err != nil {
			t.Fatalf("Prompt err = %v, want nil", err)
		}
		if final == nil || final.Parts.Text() != "done" {
			t.Fatalf("final = %+v, want a message with text %q", final, "done")
		}

		wantUsage := provider.Usage{
			InputTokens:  attempt1Usage.InputTokens + attempt2Usage.InputTokens + attempt3Usage.InputTokens,
			OutputTokens: attempt1Usage.OutputTokens + attempt2Usage.OutputTokens + attempt3Usage.OutputTokens,
		}
		if got := s.Usage(); got != wantUsage {
			t.Errorf("Usage() = %+v, want %+v (all three billed attempts, including the two discarded empty ones)", got, wantUsage)
		}

		last, ok := s.LastUsage()
		if !ok {
			t.Fatal("LastUsage not ok")
		}
		if last != attempt3Usage {
			t.Errorf("LastUsage() = %+v, want %+v (only the real successful attempt — a discarded empty attempt must never set it)", last, attempt3Usage)
		}
	})
}

// TestEmptyTurnExhaustedUsageStillAccumulates proves the accumulation
// happens even when every attempt is empty and Prompt ultimately returns an
// error: none of the tokens billed across the exhausted retry budget go
// unaccounted just because the turn never succeeded.
//
// Red-verify: before the fix, none of the three discarded attempts ever
// touch s.usage — Session.Usage() after Prompt is the zero value even
// though ~24k output tokens (3 * 8192) were actually billed.
func TestEmptyTurnExhaustedUsageStillAccumulates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u1 := provider.Usage{InputTokens: 1000, OutputTokens: 8192}
		u2 := provider.Usage{InputTokens: 1010, OutputTokens: 8192}
		u3 := provider.Usage{InputTokens: 1020, OutputTokens: 8192}
		prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
			emptyMaxTokensTurnUsage(u1),
			emptyMaxTokensTurnUsage(u2),
			emptyMaxTokensTurnUsage(u3),
		}}
		s := NewSession(Config{
			Providers:     provider.Registry{"test": prov},
			Model:         message.ModelRef{Provider: "test", Model: "m1"},
			PromptRetries: 2,
		})

		if _, err := s.Prompt(context.Background(), "go"); err == nil {
			t.Fatal("Prompt err = nil, want the exhausted empty-turn failure to surface")
		}

		wantUsage := provider.Usage{
			InputTokens:  u1.InputTokens + u2.InputTokens + u3.InputTokens,
			OutputTokens: u1.OutputTokens + u2.OutputTokens + u3.OutputTokens,
		}
		if got := s.Usage(); got != wantUsage {
			t.Errorf("Usage() = %+v, want %+v (all three billed attempts present even though Prompt returned an error)", got, wantUsage)
		}
		if _, ok := s.LastUsage(); ok {
			t.Error("LastUsage ok = true, want false (no turn ever completed successfully)")
		}
	})
}
