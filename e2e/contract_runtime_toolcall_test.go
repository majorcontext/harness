package e2e

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// callTool records each Call that it runs and each Call that it keys.
type callTool struct {
	mu    sync.Mutex
	ran   []harness.Call
	keyed []harness.Call
}

var _ harness.KeyedTool = (*callTool)(nil)

func (*callTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "probe", Description: "records its call", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (c *callTool) Key(call harness.Call) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keyed = append(c.keyed, call)
	return "one"
}

func (c *callTool) Run(_ context.Context, call harness.Call) (protocol.ToolResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = append(c.ran, call)
	return protocol.ToolResult{Text: "done"}, nil
}

func TestContractRuntimeToolCall(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "call_1", Name: "probe", Input: map[string]any{"n": 1}}, {ID: "call_2", Name: "probe", Input: map[string]any{"n": 2}}}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "ok"}})
	tool := &callTool{}
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Tools: []harness.Tool{tool}, Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	var turnID string
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.started" {
			var d struct {
				TurnID string `json:"turn_id"`
			}
			_ = json.Unmarshal(e.Data, &d)
			turnID = d.TurnID
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	tool.mu.Lock()
	defer tool.mu.Unlock()
	if len(tool.ran) != 2 || len(tool.keyed) < 2 {
		t.Fatalf("ran %d calls and keyed %d, want 2 and at least 2", len(tool.ran), len(tool.keyed))
	}
	for i, c := range append(append([]harness.Call{}, tool.ran...), tool.keyed...) {
		if c.Session != "s1" || c.Root != "s1" || turnID == "" || c.TurnID != turnID || c.Name != "probe" || c.ID == "" {
			t.Errorf("call %d = %+v, want session s1, root s1, turn %q", i, c, turnID)
		}
	}
}
