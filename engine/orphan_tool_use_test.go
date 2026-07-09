package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// dyingStream emits a fixed sequence of events (typically ending with one
// or more provider.EventToolCall entries — a complete tool_use/tool_call
// block the provider has finished emitting) and then, once exhausted,
// returns err instead of io.EOF: the shape of an SSE connection dropping
// (or the wire otherwise erroring) after a tool_use block has closed but
// before message_stop/EventDone ever arrives. It never emits EventDone.
type dyingStream struct {
	events []provider.Event
	err    error
	i      int
}

func (s *dyingStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, s.err
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *dyingStream) Close() error { return nil }

// diesAfterToolCallProvider models the mechanism behind production
// incident ses_01kx48z4rqfkpbwmzfdv1jzeg6: its first Stream call returns a
// dyingStream (one or more tool_call blocks, then a transport-style
// error, never EventDone); every subsequent Stream call serves the
// pre-scripted turns in after, exactly like scriptedProvider, so a test can
// observe what the NEXT request build looks like once the interrupted turn
// has been recorded.
type diesAfterToolCallProvider struct {
	name     string
	dying    []provider.Event
	dieErr   error
	after    [][]provider.Event
	calls    int
	requests []*provider.Request
}

func (p *diesAfterToolCallProvider) Name() string { return p.name }

func (p *diesAfterToolCallProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	p.calls++
	if p.calls == 1 {
		return &dyingStream{events: p.dying, err: p.dieErr}, nil
	}
	idx := p.calls - 2
	if idx >= len(p.after) {
		return nil, io.ErrUnexpectedEOF
	}
	return &scriptedStream{events: p.after[idx]}, nil
}

var errTransportDropped = errors.New("engine: simulated transport drop mid-turn")

// TestOrphanedToolCallAppendsSyntheticResult reproduces incident
// ses_01kx48z4rqfkpbwmzfdv1jzeg6 red-first: a provider stream emits one
// complete tool_call block (provider.EventToolCall — the shape
// provider/anthropic/anthropic.go's content_block_stop handler and
// provider/openaicompat/openaicompat.go's emitToolCalls both produce) and
// then dies before EventDone ever arrives, so the engine never gets a
// chance to execute it.
//
// Before the fix: Prompt's error path discarded the assembled partial
// content entirely — nothing entered history, so nothing looked
// "poisoned" in this session's own history yet, but the model's tool_call
// was lost with no record and no result. Worse, the NEXT prompt in this
// same test proves the point that matters operationally: with the fix, a
// self-consistent history (ToolCall immediately followed by its
// ToolResult) means the subsequent turn's request build succeeds and the
// session recovers instead of the production shape (three identical
// retries, all killed by the same orphaned tool_use).
func TestOrphanedToolCallAppendsSyntheticResult(t *testing.T) {
	orphaned := toolCall("orphan1", "bash", `{"command":"echo hi"}`)
	prov := &diesAfterToolCallProvider{
		name:   "test",
		dying:  []provider.Event{{Type: provider.EventToolCall, ToolCall: orphaned}},
		dieErr: errTransportDropped,
		after: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "recovered"}),
		},
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	_, err := s.Prompt(context.Background(), "go")
	if err == nil {
		t.Fatal("Prompt = nil error, want the underlying transport error surfaced")
	}
	if !errors.Is(err, errTransportDropped) {
		t.Errorf("Prompt error = %v, want it to wrap %v (errors.Is)", err, errTransportDropped)
	}

	// History: user, then the interrupted assistant message (its
	// ToolCall preserved), then a synthetic tool-role result — never a
	// bare ToolCall with nothing after it.
	h := s.History()
	if len(h) != 3 {
		t.Fatalf("history len = %d, want 3 (user, interrupted assistant, synthetic tool result): %+v", len(h), h)
	}
	if h[0].Role != message.RoleUser {
		t.Fatalf("h[0].Role = %s, want user", h[0].Role)
	}
	if h[1].Role != message.RoleAssistant {
		t.Fatalf("h[1].Role = %s, want assistant", h[1].Role)
	}
	var gotTC *message.ToolCall
	for _, p := range h[1].Parts {
		if tc, ok := p.(*message.ToolCall); ok {
			gotTC = tc
		}
	}
	if gotTC == nil || gotTC.CallID != "orphan1" || gotTC.Name != "bash" {
		t.Fatalf("interrupted assistant message lost its ToolCall: %+v", h[1])
	}
	if h[2].Role != message.RoleTool {
		t.Fatalf("h[2].Role = %s, want tool (the synthetic result)", h[2].Role)
	}
	if len(h[2].Parts) != 1 {
		t.Fatalf("synthetic tool message parts = %d, want 1", len(h[2].Parts))
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want *message.ToolResult", h[2].Parts[0])
	}
	if tr.CallID != "orphan1" {
		t.Errorf("synthetic ToolResult.CallID = %q, want %q", tr.CallID, "orphan1")
	}
	if !tr.IsError {
		t.Error("synthetic ToolResult.IsError = false, want true")
	}
	if tr.Content.Text() != interruptedTurnErrorText {
		t.Errorf("synthetic ToolResult.Content = %q, want %q", tr.Content.Text(), interruptedTurnErrorText)
	}

	// The whole point: this history must itself be marshalable (the
	// GET /message shape) ...
	if _, err := json.Marshal(h); err != nil {
		t.Fatalf("json.Marshal(History()) = %v, want success", err)
	}

	// ... and, the actual production failure mode, the NEXT request build
	// must succeed and pair the tool_use with its result — a subsequent
	// worker turn recovers instead of dying identically on every retry.
	final, err := s.Prompt(context.Background(), "continue")
	if err != nil {
		t.Fatalf("second Prompt (subsequent worker turn) = %v, want success", err)
	}
	if final.Parts.Text() != "recovered" {
		t.Errorf("second Prompt final = %q, want %q", final.Parts.Text(), "recovered")
	}
	if len(prov.requests) < 2 {
		t.Fatalf("provider recorded %d requests, want at least 2", len(prov.requests))
	}
	secondReqMessages := prov.requests[1].Messages
	if len(secondReqMessages) != 4 {
		t.Fatalf("second request history = %d messages, want 4 (user, interrupted assistant, synthetic tool result, continue)", len(secondReqMessages))
	}
	if _, err := json.Marshal(secondReqMessages); err != nil {
		t.Fatalf("json.Marshal(second request's Messages) = %v, want success", err)
	}

	// toolExecCount must NOT have moved: the orphaned call never executed,
	// so a goal-loop retry of this same attempt would be safe (see
	// promptTurnWithRetry's non-idempotency doc comment in goal.go).
	if got := s.toolExecutions(); got != 0 {
		t.Errorf("toolExecutions() = %d, want 0 (interrupted call must never be executed)", got)
	}
}

// TestOrphanedToolCallMultipleCalls covers a turn that recorded more than
// one complete tool_call before dying: every one of them must get its own
// synthetic result, in emission order, none silently dropped.
func TestOrphanedToolCallMultipleCalls(t *testing.T) {
	tc1 := toolCall("tc1", "bash", `{"command":"echo a"}`)
	tc2 := toolCall("tc2", "read_file", `{"path":"x"}`)
	prov := &diesAfterToolCallProvider{
		name: "test",
		dying: []provider.Event{
			{Type: provider.EventToolCall, ToolCall: tc1},
			{Type: provider.EventToolCall, ToolCall: tc2},
		},
		dieErr: errTransportDropped,
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	if _, err := s.Prompt(context.Background(), "go"); !errors.Is(err, errTransportDropped) {
		t.Fatalf("Prompt error = %v, want it to wrap errTransportDropped", err)
	}

	h := s.History()
	if len(h) != 3 || h[2].Role != message.RoleTool {
		t.Fatalf("history = %+v, want [user, assistant, tool]", h)
	}
	if len(h[2].Parts) != 2 {
		t.Fatalf("synthetic tool message parts = %d, want 2 (one per orphaned call)", len(h[2].Parts))
	}
	gotIDs := map[string]bool{}
	for _, p := range h[2].Parts {
		tr, ok := p.(*message.ToolResult)
		if !ok {
			t.Fatalf("part = %T, want *message.ToolResult", p)
		}
		if !tr.IsError {
			t.Errorf("ToolResult for %s: IsError = false, want true", tr.CallID)
		}
		gotIDs[tr.CallID] = true
	}
	if !gotIDs["tc1"] || !gotIDs["tc2"] {
		t.Errorf("synthetic result call IDs = %v, want both tc1 and tc2", gotIDs)
	}
}

// TestStreamErrorWithoutToolCallIsUnaffected proves the fix is scoped: a
// turn that errors having recorded NO tool call at all (the ordinary
// provider-failure case every other engine test already covers, e.g.
// TestSessionErrorEvent) behaves exactly as before — nothing is appended
// to history, the bare error is returned unwrapped.
func TestStreamErrorWithoutToolCallIsUnaffected(t *testing.T) {
	prov := &diesAfterToolCallProvider{
		name:   "test",
		dying:  []provider.Event{{Type: provider.EventTextDelta, Text: "thinking..."}},
		dieErr: errTransportDropped,
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	_, err := s.Prompt(context.Background(), "go")
	if !errors.Is(err, errTransportDropped) {
		t.Fatalf("Prompt error = %v, want errTransportDropped", err)
	}
	var interrupted *interruptedTurnError
	if errors.As(err, &interrupted) {
		t.Fatalf("error wrapped as interruptedTurnError with no tool call ever recorded: %+v", interrupted)
	}
	h := s.History()
	if len(h) != 1 || h[0].Role != message.RoleUser {
		t.Fatalf("history = %+v, want only the user message (turn never entered history)", h)
	}
}
