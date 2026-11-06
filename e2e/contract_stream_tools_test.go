package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
)

func TestContractRuntimeStartsAToolCallBeforeTheModelStreamEnds(t *testing.T) {
	skipShort(t)
	started, release := make(chan string, 2), make(chan struct{})
	tools := []harness.Tool{stopTool{name: "react", started: started, release: release, readOnly: true}}
	call := stopCall("react")
	call.Reply.EndBlock = true
	done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("react"), Reply: harnesstest.Reply{Text: "ok"}}
	s, fake := stopRuntime(t, "anthropic", tools, nil, call, done)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	turn := stopPrompt(t, s, "a", "go")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the tool did not start while the model stream was still open")
	}
	if !fake.AwaitBlocked("call:end", time.Second) {
		t.Fatal("the model stream ended before the tool started")
	}
	close(release)
	fake.Release("call:end")
	stopWait(t, turn)
	select {
	case name := <-started:
		t.Fatalf("%s started a second time", name)
	default:
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || !strings.Contains(requestText(reqs[1]), "react result") {
		t.Fatalf("requests = %d; the second must carry the tool result", len(reqs))
	}
}
