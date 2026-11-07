package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
)

func TestContractRejectedNormalizationRunsHooks(t *testing.T) {
	skipShort(t)
	before, after := make(chan string, 1), make(chan string, 1)
	p := &plugin.Plugin{Name: "hooks", ToolExecuteBefore: &plugin.ToolBefore{Run: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
		before <- string(req.Args)
		return nil, nil
	}}, ToolExecuteAfter: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteAfterRequest) (*plugin.ToolExecuteAfterResponse, error) {
		after <- req.Output[0].Text
		return nil, nil
	}}
	call := harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "probe", Input: map[string]any{"value": "reject"}}}}}
	done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
	s, _ := stopRuntimeOptions(t, "openai", harness.Options{Tools: []harness.Tool{normalizingProbe{inputProbe{"probe"}}}, Plugins: []*plugin.Plugin{p}}, call, done)
	stopWait(t, stopPrompt(t, s, "a", "go"))
	select {
	case got := <-before:
		if !strings.Contains(got, "reject") {
			t.Errorf("before arguments = %q", got)
		}
	default:
		t.Error("before hook did not run")
	}
	select {
	case got := <-after:
		if !strings.Contains(got, "input rejected") {
			t.Errorf("after result = %q", got)
		}
	default:
		t.Error("after hook did not run")
	}
}
