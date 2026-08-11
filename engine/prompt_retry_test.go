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
