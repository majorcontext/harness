package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// gateTool returns a Tool named "gate" whose Run blocks: it closes entered
// the instant execution starts (so a test can observe the tool is genuinely
// in flight) and then parks on release until the test lets it finish. This
// is the channel-gate a real subprocess-backed tool (bash) cannot give a
// test deterministically without a sleep-and-poll loop.
func gateTool(entered, release chan struct{}) Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "gate",
			Description: "test-only tool that blocks until released",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return message.Parts{&message.Text{Text: "gate done"}}, nil
		},
	}
}

// TestMidTurnInjectionAtToolBoundary is the headline test for the tool-call-
// boundary injection amendment (docs/plans/2026-07-19-prompt-queue.md's
// addendum): a prompt enqueued WHILE a tool call is still executing must be
// delivered into the very next provider request of that SAME turn — not
// wait for the turn (or a goal boundary) to end.
//
// The turn is scripted as: assistant call 1 makes a tool call to "gate"
// (StopToolUse); the gate tool blocks; the test enqueues a prompt while it
// is genuinely in flight, then releases it; assistant call 2 (immediately
// after the tool result is appended) must see the operator block trailing
// its request history.
func TestMidTurnInjectionAtToolBoundary(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "gate", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "final"}),
	}}
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		System:     []string{"base"},
		SessionDir: dir,
		Tools:      []Tool{gateTool(entered, release)},
	})

	type outcome struct {
		msg *message.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		m, err := s.Prompt(context.Background(), "please run gate")
		done <- outcome{m, err}
	}()

	<-entered // the tool call is genuinely executing

	if _, err := s.EnqueuePrompt("operator says hi mid-turn"); err != nil {
		t.Fatalf("EnqueuePrompt = %v", err)
	}

	// The queue must be drained (and journaled) the instant the tool result
	// lands, which is entirely before the release below lets the tool
	// return — but there is no in-process signal for "the tool result was
	// appended and the queue drained" available to the test other than
	// letting the turn finish and inspecting requests/log afterward, so
	// release now and assert the ordering post-hoc from the durable log.
	close(release)

	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.msg.Parts.Text() != "final" {
		t.Errorf("final = %q", out.msg.Parts.Text())
	}

	if len(prov.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(prov.requests))
	}
	second := prov.requests[1]
	last := second.Messages[len(second.Messages)-1]
	if last.Role != message.RoleUser {
		t.Fatalf("second request's trailing message role = %s, want user (the injected operator block)", last.Role)
	}
	text := last.Parts.Text()
	if !strings.Contains(text, "OPERATOR MESSAGES") {
		t.Errorf("second request's trailing message = %q, want a labeled operator block", text)
	}
	// This is engine.go's tool-call-boundary drain, not a goal loop, so the
	// header's trailing clause must say "task", never "goal" (see
	// operatorMessagesBlock, queue.go).
	if !strings.Contains(text, "continue the task") {
		t.Errorf("second request's trailing message = %q, want plain-turn wording (continue the task)", text)
	}
	if strings.Contains(text, "continue the goal") {
		t.Errorf("second request's trailing message = %q, must not reference a goal in a plain turn", text)
	}
	if !strings.Contains(text, "operator says hi mid-turn") {
		t.Errorf("second request's trailing message = %q, want the queued text", text)
	}

	if pending := s.QueuedPrompts(); len(pending) != 0 {
		t.Fatalf("QueuedPrompts after mid-turn injection = %+v, want empty", pending)
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	dequeueLine, messageLine := -1, -1
	for i, line := range lines {
		if strings.Contains(line, `"type":"prompt.dequeued"`) && strings.Contains(line, "operator says hi mid-turn") {
			dequeueLine = i
		}
		if strings.Contains(line, `"type":"message"`) && strings.Contains(line, "operator says hi mid-turn") {
			messageLine = i
		}
	}
	if dequeueLine == -1 {
		t.Fatalf("log missing the injected prompt.dequeued record: %s", log)
	}
	if messageLine == -1 {
		t.Fatalf("log missing the delivered message record carrying the injected text: %s", log)
	}
	if dequeueLine >= messageLine {
		t.Fatalf("prompt.dequeued(injected) record (line %d) must be journaled BEFORE the message record (line %d) that delivers it", dequeueLine, messageLine)
	}
	if !strings.Contains(log, `"reason":"injected"`) {
		t.Fatalf("log missing reason:injected on the dequeue record: %s", log)
	}
}

// TestNoToolCallTurnLeavesQueueForTail covers the other half of the tool-
// call-boundary contract: a turn that ends WITHOUT any tool call never
// reaches the new mid-turn drain point at all (both early returns in
// Session.Prompt precede it), so a prompt sitting in the queue when such a
// turn runs must come out untouched — left for the server's ordinary tail
// drain, exactly as before this change.
func TestNoToolCallTurnLeavesQueueForTail(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done, no tools"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
	})

	if _, err := s.EnqueuePrompt("still waiting"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	pending := s.QueuedPrompts()
	if len(pending) != 1 || pending[0].Text != "still waiting" {
		t.Fatalf("QueuedPrompts after a no-tool-call turn = %+v, want still 1 pending (left for the server's tail drain)", pending)
	}

	for _, m := range s.History() {
		if strings.Contains(m.Parts.Text(), "still waiting") {
			t.Errorf("queued text must not be injected when the turn made no tool calls: %+v", m)
		}
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1 (no tool loop, so no second call)", len(prov.requests))
	}
}
