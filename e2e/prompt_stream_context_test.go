package e2e

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/httpapi"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var promptStreamContextRows = map[string]runtimeRow{
	"prompt_stream_context_cancels_a_custom_tool_after_close": runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_context_withdraws_its_queued_input_only":   runtimeOnly("embedded-subscriber-held"),
}

func TestContractPromptStreamContext(t *testing.T) {
	tool := &streamContextTool{entered: make(chan struct{}), exited: make(chan struct{})}
	runScenarios(t, []scenario{
		{
			name: "prompt_stream_context_cancels_a_custom_tool_after_close",
			driver: func(t *testing.T, h host, modelURL string) driver {
				d := h.newDriver(t, modelURL, nil).(*runtimeDriver)
				d.stop(t, context.Background())
				rt, err := newWired(t, harness.Options{Store: store.NewDiskStore(d.store), Config: d.cfg, WorkDir: d.workDir,
					Version: runtimeVersion, Getenv: d.getenv, Tools: []harness.Tool{tool}})
				if err != nil {
					t.Fatal(err)
				}
				d.rt, d.srv = rt, httptest.NewServer(httpapi.Handler(rt))
				return d
			},
			model:   []harnesstest.Step{{Name: "tool", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "toolu_1", Name: "wait_context"}}}}},
			actions: []action{create{as: "a"}, promptStreamToolCanceled{tool: tool}},
		},
		{
			name:    "prompt_stream_context_withdraws_its_queued_input_only",
			model:   []harnesstest.Step{{Name: "prior", Reply: harnesstest.Reply{Text: "prior", Block: true}}},
			actions: []action{create{as: "a"}, watchItems{as: "a"}, submit{as: "a", text: "prior"}, watchItems{as: "a", late: true}, promptStreamQueuedCanceled{}},
		},
	})
}

type streamContextTool struct{ entered, exited chan struct{} }

func (*streamContextTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "wait_context", Description: "Wait for cancellation", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (t *streamContextTool) Run(ctx context.Context, _ harness.Call) (protocol.ToolResult, error) {
	close(t.entered)
	<-ctx.Done()
	close(t.exited)
	return protocol.ToolResult{}, ctx.Err()
}

type promptStreamToolCanceled struct{ tool *streamContextTool }

func (a promptStreamToolCanceled) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := s.PromptStream(ctx, protocol.Input{ID: "tool", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	<-a.tool.entered
	r.drv.(*runtimeDriver).awaitToolCall(t, s.ID(), "toolu_1")
	stream.Close()
	cancel()
	<-a.tool.exited
	result, err := stream.Wait(t.Context())
	if err != nil || result.StopReason != protocol.StopInterrupted {
		t.Fatalf("tool cancellation = %+v, %v", result, err)
	}
}

type promptStreamQueuedCanceled struct{}

func (promptStreamQueuedCanceled) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	s, err := d.rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := s.PromptStream(ctx, protocol.Input{ID: "queued", Parts: []protocol.Part{{Type: protocol.PartText, Text: "never"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	cancel()
	result, err := stream.Wait(t.Context())
	if err != nil || !result.Withdrawn {
		t.Fatalf("queued cancellation = %+v, %v", result, err)
	}
	if s.Snapshot().Status != protocol.StatusRunning {
		t.Fatal("queued context interrupted the prior turn")
	}
	r.fake.Release("prior")
	d.WaitIdle(t, s.ID())
	if len(r.fake.Requests()) != 1 {
		t.Fatal("withdrawn input ran a model call")
	}
}
