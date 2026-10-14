package turn_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// recorder is the turn.Turn of one turn: it needs no turn ID, because it is
// bound to its turn.
type recorder struct {
	items    []protocol.LogMessage
	statuses []string
	waits    []time.Duration
	ended    []error
	steer    [][]protocol.LogMessage
	started  int
	deltas   []string
	// folded is the history that CompactTurn returns. nil: nothing folds.
	folded []protocol.LogMessage
}

func (r *recorder) Item(m protocol.LogMessage) error             { r.items = append(r.items, m); return nil }
func (r *recorder) Delta(id string, _ turn.Delta)                { r.deltas = append(r.deltas, id) }
func (r *recorder) Announce() string                             { return r.Started() }
func (r *recorder) ItemAs(_ string, m protocol.LogMessage) error { return r.Item(m) }
func (r *recorder) Started() string {
	r.started++
	return fmt.Sprintf("item_%d", r.started)
}
func (*recorder) Attach(string, []byte) (protocol.LogPart, error) {
	return protocol.LogPart{}, nil
}
func (*recorder) Alive()                   {}
func (*recorder) Discard(string)           {}
func (*recorder) Telemetry(turn.Telemetry) {}
func (r *recorder) Steer() ([]protocol.LogMessage, error) {
	if len(r.steer) == 0 {
		return nil, nil
	}
	in := r.steer[0]
	r.steer = r.steer[1:]
	return in, nil
}
func (*recorder) Settings() (string, protocol.Settings)     { return "", protocol.Settings{} }
func (*recorder) Pin([]turn.Notice, int) []turn.Pin         { return nil }
func (*recorder) Ask(string, string, json.RawMessage) error { return nil }
func (*recorder) Asked(string) (json.RawMessage, bool)      { return nil, false }
func (*recorder) Resolution(string) (protocol.RequestResolved, bool) {
	return protocol.RequestResolved{}, false
}
func (*recorder) State(string) (turn.Snapshot, error)   { return turn.Snapshot{}, nil }
func (*recorder) SaveState(string, turn.Snapshot) error { return nil }
func (*recorder) Compacted(string) error                { return nil }
func (r *recorder) Status(f protocol.StatusFrame) {
	r.statuses = append(r.statuses, string(f.Status))
	if f.Status == protocol.StatusRetrying {
		r.waits = append(r.waits, time.Until(f.NextAt))
	}
}
func (r *recorder) CompactTurn(context.Context) ([]protocol.LogMessage, bool, error) {
	return r.folded, r.folded != nil, nil
}
func (r *recorder) Ended(err error) { r.ended = append(r.ended, err) }

// model is a native backend: one scripted reply for each model call.
type model struct {
	replies  []protocol.LogMessage
	errs     []error
	requests []turn.Request
	// streams makes each call stream one delta before it fails or answers.
	streams bool
}

func (m *model) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (m *model) Run(_ context.Context, req turn.Request, out turn.Sink) error {
	m.requests = append(m.requests, req)
	i := len(m.requests) - 1
	if m.streams {
		out.Delta("backend", turn.Delta{Type: protocol.LogPartText, Text: "t"})
	}
	if i < len(m.errs) && m.errs[i] != nil {
		return m.errs[i]
	}
	return out.Item(m.replies[i])
}

func say(text string) protocol.LogMessage {
	return protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func steerInput(text string) protocol.LogMessage {
	return protocol.LogMessage{Role: protocol.RoleUser, Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}}
}

func TestRetryBackoffDoublesFromTwoHundredMilliseconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const calls = 6
		m := &model{replies: make([]protocol.LogMessage, calls+1), errs: make([]error, calls)}
		for i := range m.errs {
			m.errs[i] = turn.ErrRetryable
		}
		m.replies[calls] = say("ok")
		r := &recorder{}
		ctx := context.Background()
		turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, r, turn.Limits{Retries: calls})
		want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond}
		if len(r.waits) != len(want) {
			t.Fatalf("waits = %v, want %v", r.waits, want)
		}
		for i := range want {
			if lo, hi := want[i]*9/10, want[i]*11/10; r.waits[i] < lo || r.waits[i] > hi {
				t.Errorf("wait %d = %v, want %v within the jitter", i+1, r.waits[i], want[i])
			}
		}
	})
}

func TestRunStartsOneItemForEachCallThatStreams(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		lim  turn.Limits
		r    *recorder
	}{
		{"a retried call", turn.ErrRetryable, turn.Limits{Retries: 1}, &recorder{}},
		{"a call that overflows the context", turn.ErrContextOverflow, turn.Limits{}, &recorder{folded: []protocol.LogMessage{steerInput("summary")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &model{replies: []protocol.LogMessage{{}, say("ok")}, errs: []error{tc.err}, streams: true}
				ctx := context.Background()
				turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, nil, tc.r, tc.lim)
				if len(tc.r.ended) != 1 || tc.r.ended[0] != nil {
					t.Fatalf("Ended calls = %v, want one with no error", tc.r.ended)
				}
				if want := []string{"item_1", "item_2"}; !slices.Equal(tc.r.deltas, want) {
					t.Fatalf("deltas named %v, want %v: the failed call abandoned item_1", tc.r.deltas, want)
				}
			})
		})
	}
}

type abortingHooks struct {
	cancel context.CancelFunc
	after  []protocol.ToolResult
}

func (h *abortingHooks) Before(_ context.Context, c protocol.ToolCall) (protocol.ToolCall, string) {
	h.cancel()
	return c, ""
}

func (h *abortingHooks) After(_ context.Context, _ protocol.ToolCall, r protocol.ToolResult) protocol.ToolResult {
	h.after = append(h.after, r)
	return r
}

type hookedSource struct {
	tool  turn.Tool
	hooks turn.Hooks
}

func (s hookedSource) Toolset(context.Context, []protocol.LogMessage, []string, string) turn.Toolset {
	return turn.Toolset{Tools: []turn.Tool{s.tool}, Hooks: s.hooks}
}

type noopTool struct{}

func (noopTool) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "noop"} }
func (noopTool) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "ran"}, nil
}

func TestRunGivesAfterTheErrorResultOfACallThatWasCanceledAfterBefore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hooks := &abortingHooks{cancel: cancel}
	call := protocol.LogMessage{Role: protocol.RoleAssistant, Parts: []protocol.LogPart{{Type: protocol.LogPartToolCall, CallID: "c1", Name: "noop", Arguments: json.RawMessage(`{}`)}}}
	m := &model{replies: []protocol.LogMessage{call, say("done")}}
	turn.Run(ctx, ctx, m, turn.Request{Model: "test/model"}, hookedSource{noopTool{}, hooks}, &recorder{}, turn.Limits{})
	if len(hooks.after) != 1 || !hooks.after[0].IsError || hooks.after[0].Text != context.Canceled.Error() {
		t.Errorf("After saw %+v, want one error result with the cancel cause", hooks.after)
	}
}
