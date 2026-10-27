package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
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

// hostBash is an embedder tool with the name of the built-in bash tool.
type hostBash struct{}

func (hostBash) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "bash", Description: "host shell", InputSchema: json.RawMessage(`{"type":"object","properties":{"host_only":{"type":"string"}}}`)}
}

func (hostBash) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "host ran"}, nil
}

func hostBashRuntime(t *testing.T, fake *harnesstest.Server) *harness.Runtime {
	t.Helper()
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Tools: []harness.Tool{hostBash{}}, WorkDir: t.TempDir(), Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}}})
	if err != nil {
		t.Fatalf("New with an embedder tool named bash = %v, want nil", err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

func TestContractRuntimeEmbedderToolReplacesAnUnlistedBuiltinOfTheSameName(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{}}}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ok"}})
	r := hostBashRuntime(t, fake)
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test", AllowedTools: []string{"read_file", "bash"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	if n := strings.Count(strings.Join(reqs[0].Tools, " ")+" ", "bash "); n != 1 {
		t.Errorf("tools of the first request = %v, want one bash", reqs[0].Tools)
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "host ran") {
		t.Errorf("the second request does not hold the result of the embedder tool: %s", got)
	}
}

func TestContractRuntimeEmbedderToolWithABuiltinNameFailsCreateWhenEveryToolIsAllowed(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	r := hostBashRuntime(t, harnesstest.NewChat(t))
	_, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if !errors.Is(err, harness.ErrInvalidRequest) || !strings.Contains(err.Error(), `"bash"`) {
		t.Errorf("Create = %v, want ErrInvalidRequest that names bash", err)
	}
}

func TestContractRuntimeEmbedderToolWithABuiltinNameFailsOpenOfASessionThatAllowsEveryTool(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	fake := harnesstest.NewChat(t)
	st, dir := store.NewMemStore(), t.TempDir()
	opts := func(tools ...harness.Tool) harness.Options {
		return harness.Options{Store: st, Tools: tools, WorkDir: dir, Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000,
			Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}}}
	}
	first, err := harness.New(opts())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := harness.New(opts(hostBash{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	_, err = second.Open(t.Context(), "s1")
	if !errors.Is(err, harness.ErrInvalidRequest) || !strings.Contains(err.Error(), `"bash"`) {
		t.Errorf("Open = %v, want ErrInvalidRequest that names bash", err)
	}
}

func TestContractRuntimeSessionInfoWithoutWorkDir(t *testing.T) {
	skipShort(t)
	t.Run("a_runtime_with_no_work_directory_offers_session_info_and_a_call_returns_a_report", func(t *testing.T) {
		t.Parallel()
		call := harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "session_info", Input: map[string]any{}}}}}
		r, fake := noWorkDirRuntime(t, call, harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
		s := sessionInfoCreate(t, r, nil)
		sessionMCPTurn(t, s, "a", "go")
		reqs := fake.Requests()
		if !requestHas(reqs[0], "session_info") {
			t.Errorf("first request tools = %v, want session_info", reqs[0].Tools)
		}
		if got := requestText(reqs[1]); !strings.Contains(got, `"session_id": "s1"`) {
			t.Errorf("second request does not hold the report: %s", got)
		}
	})
	t.Run("an_allowed_tools_list_without_session_info_hides_it", func(t *testing.T) {
		t.Parallel()
		r, fake := noWorkDirRuntime(t, harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
		s := sessionInfoCreate(t, r, []string{"read_file"})
		sessionMCPTurn(t, s, "a", "go")
		if reqs := fake.Requests(); requestHas(reqs[0], "session_info") {
			t.Errorf("tools = %v, want no session_info", reqs[0].Tools)
		}
	})
}

func noWorkDirRuntime(t *testing.T, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.NewChat(t, steps...)
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: func(string) string { return "k" }, Config: config.Config{
		ContextWindowTokens: 100000,
		Providers:           map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "SESSION_INFO_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, fake
}

func sessionInfoCreate(t *testing.T, r *harness.Runtime, allowed []string) *harness.Session {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test", AllowedTools: allowed})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
