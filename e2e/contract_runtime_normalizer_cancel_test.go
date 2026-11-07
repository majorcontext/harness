package e2e

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/store"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

type heldInputNormalizer struct {
	inputProbe
	started  chan struct{}
	release  chan struct{}
	returned chan struct{}
	ran      chan struct{}
}

func (h heldInputNormalizer) Run(_ context.Context, _ harness.Call) (protocol.ToolResult, error) {
	close(h.ran)
	return protocol.ToolResult{}, nil
}

func (h heldInputNormalizer) NormalizeInput(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	close(h.started)
	<-h.release
	defer close(h.returned)
	return input, nil
}

func TestContractRuntimeNormalizerCancellation(t *testing.T) {
	skipShort(t)
	tool := heldInputNormalizer{inputProbe: inputProbe{"probe"}, started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{}), ran: make(chan struct{})}
	call := harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "probe", Input: map[string]any{}}}}}
	s, _ := stopRuntime(t, "openai", []harness.Tool{tool}, nil, call)
	var once sync.Once
	release := func() { once.Do(func() { close(tool.release) }) }
	t.Cleanup(release)
	turn := stopPrompt(t, s, "a", "go")
	startCtx, startCancel := context.WithTimeout(t.Context(), waitBound)
	defer startCancel()
	select {
	case <-tool.started:
	case <-startCtx.Done():
		t.Fatal("normalizer did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	if err := turn.Cancel(ctx); err != nil {
		t.Fatalf("cancellation waited for normalizer: %v", err)
	}
	result, err := turn.Wait(ctx)
	if err != nil || result.Cause != protocol.CauseStopped {
		t.Fatalf("turn = %+v, %v; want stopped", result, err)
	}
	release()
	select {
	case <-tool.returned:
	case <-ctx.Done():
		t.Fatal("normalizer did not return")
	}
	select {
	case <-tool.ran:
		t.Fatal("tool ran after cancellation")
	default:
	}
}

func TestContractRuntimeIdleInterruptsNormalizer(t *testing.T) {
	skipShort(t)
	tool := heldInputNormalizer{inputProbe: inputProbe{"probe"}, started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{}), ran: make(chan struct{})}
	fake := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, stopCall("probe")).Server
	r, err := harness.New(harness.Options{Tools: []harness.Tool{tool}, Store: store.NewMemStore(), Getenv: func(string) string { return "k" }, Config: config.Config{StreamIdleTimeoutS: 1, StreamMaxRetries: new(int), ContextWindowTokens: 100000, Providers: map[string]config.Provider{"openai": {Type: config.TypeOpenAI, BaseURL: fake.URL(), ResponsesPath: "/backend-api/codex/responses", APIKeyEnv: "KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	t.Cleanup(func() { close(tool.release) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "openai/test-model"})
	if err != nil {
		t.Fatal(err)
	}
	turn := stopPrompt(t, s, "a", "go")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal("normalizer did not start")
	}
	_, err = turn.Wait(ctx)
	if err != nil {
		t.Fatalf("idle watchdog did not interrupt normalizer: %v", err)
	}
	select {
	case <-tool.ran:
		t.Fatal("tool ran after idle timeout")
	default:
	}
}

type retryInputNormalizer struct {
	inputProbe
	calls   atomic.Int32
	release chan struct{}
	ran     chan struct{}
}

func (h *retryInputNormalizer) NormalizeInput(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	if h.calls.Add(1) == 1 {
		<-h.release
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return input, nil
}

func (h *retryInputNormalizer) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	close(h.ran)
	return protocol.ToolResult{Text: "ok"}, nil
}

func TestContractRuntimeIdleRetriesNormalizer(t *testing.T) {
	skipShort(t)
	tool := &retryInputNormalizer{inputProbe: inputProbe{"probe"}, release: make(chan struct{}), ran: make(chan struct{})}
	done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "done"}}
	fake := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, stopCall("probe"), stopCall("probe"), done).Server
	one := 1
	r, err := harness.New(harness.Options{Tools: []harness.Tool{tool}, Store: store.NewMemStore(), Getenv: func(string) string { return "k" }, Config: config.Config{StreamIdleTimeoutS: 1, StreamMaxRetries: &one, ContextWindowTokens: 100000, Providers: map[string]config.Provider{"openai": {Type: config.TypeOpenAI, BaseURL: fake.URL(), ResponsesPath: "/backend-api/codex/responses", APIKeyEnv: "KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	t.Cleanup(func() { close(tool.release) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "openai/test-model"})
	if err != nil {
		t.Fatal(err)
	}
	turn := stopPrompt(t, s, "a", "go")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := turn.Wait(ctx)
	if err != nil || result.StopReason != protocol.StopCompleted {
		t.Fatalf("retry turn = %+v, %v; want completed", result, err)
	}
	if got := tool.calls.Load(); got != 2 {
		t.Fatalf("normalizer calls = %d; want 2", got)
	}
	select {
	case <-tool.ran:
	default:
		t.Fatal("retry tool did not run")
	}
}
