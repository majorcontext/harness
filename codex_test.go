package harness_test

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// transport records each request that a provider sends through ModelTransport.
type transport struct {
	mu    sync.Mutex
	calls []string
}

// tagged sets the bearer token key on each request when key is set.
type tagged struct {
	t        *transport
	provider string
	key      string
}

func (g tagged) RoundTrip(req *http.Request) (*http.Response, error) {
	g.t.mu.Lock()
	g.t.calls = append(g.t.calls, g.provider+" "+req.Method+" "+req.URL.Path)
	g.t.mu.Unlock()
	if g.key != "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (t *transport) Calls() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.calls)
}

// codexRuntime runs turns on s, as provider codex and as provider openai,
// with no retries, and also configures provider claude-code. workDir is
// Options.WorkDir. The key is in the
// environment, or, with injected set, only in the ModelTransport.
func codexRuntime(t *testing.T, s *harnesstest.OpenAI, websocket, injected bool, workDir string, tools ...harness.Tool) (*harness.Runtime, harness.Store, *transport) {
	t.Helper()
	envKey, key := "k", ""
	if injected {
		envKey, key = "", "k"
	}
	t.Setenv("HARNESS_TEST_CODEX_KEY", envKey)
	st, rec, retries := harness.NewMemStore(), &transport{}, 0
	p := config.Provider{
		Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
		BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses",
		OmitResponseParams: []string{"max_output_tokens"}, UseWebSocketTransport: websocket,
	}
	r, err := harness.New(harness.Options{
		Store:          st,
		WorkDir:        workDir,
		Config:         config.Config{PromptRetries: &retries, Providers: map[string]config.Provider{"codex": p, "openai": p, "claude-code": {Type: config.TypeClaudeCodeCLI}}},
		ModelTransport: func(provider string) http.RoundTripper { return tagged{rec, provider, key} },
		Tools:          tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r, st, rec
}

// converse submits each text as an input and waits for the turn it starts to end.
func converse(t *testing.T, s *harness.Session, texts ...string) {
	t.Helper()
	for i, txt := range texts {
		watch(t, s, s.View().HeadSeq-1, s.View().HeadSeq, text(string(rune('a'+i)), txt), false)
	}
}

// transcript prints each part of the messages of req as one line.
func transcript(req harnesstest.Request) []string {
	var out []string
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			out = append(out, m.Role+" "+p.Kind+" "+p.ToolName+":"+p.Text)
		}
	}
	return out
}

const codexPost = "codex POST /backend-api/codex/responses"

var codexTurns = []struct {
	name      string
	websocket bool
	injected  bool
	effort    string
	tier      string
	opts      harnesstest.OpenAIOptions
	steps     []harnesstest.Step
	inputs    []string
	want      []string
	calls     []string
	check     func(t *testing.T, s *harnesstest.OpenAI)
}{
	{name: "a text turn records the assistant item",
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost}},
	{name: "a call to an unknown tool gets an error result and the next turn sees it",
		steps: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash"}}}},
			{Name: "refused", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "noted"}},
			{Name: "after", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}}},
		inputs: []string{"run", "again"},
		want: []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant call_1", "item.completed tool call_1 " + noTool + "bash",
			"context.measured", "item.completed assistant noted", "turn.ended completed",
			"input.admitted b", "turn.started b", "context.measured", "item.completed assistant ok", "turn.ended completed"},
		calls: []string{codexPost, codexPost, codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			got := transcript(s.Requests()[2])
			want := []string{"user text :run", "assistant tool_use bash:", "user tool_result bash:[tool error] " + noTool + "bash",
				"assistant text :noted", "user text :again"}
			if !slices.Equal(got, want) {
				t.Errorf("second request = %q, want %q", got, want)
			}
		}},
	{name: "a dropped stream fails the turn as retryable",
		opts:   harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"drop": {Drop: true}}},
		steps:  []harnesstest.Step{{Name: "drop", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "partial"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "turn.ended failed turn: retryable backend error: [retryable:stream_truncated] provider stream ended before completion: unexpected EOF"},
		calls:  []string{codexPost}},
	{name: "a response with no output fails the turn as retryable",
		steps:  []harnesstest.Step{{Name: "empty", Match: harnesstest.LastUserText("hi")}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "turn.ended failed turn: retryable backend error: modelapi: the response has no output"},
		calls:  []string{codexPost}},
	{name: "the reasoning item of a tool call turn is replayed",
		opts: harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"call": {Reasoning: []string{"plan"}}}},
		steps: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash"}}}},
			{Name: "refused", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "noted"}},
			{Name: "after", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}}},
		inputs: []string{"run", "again"},
		want: []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant plan call_1", "item.completed tool call_1 " + noTool + "bash",
			"context.measured", "item.completed assistant noted", "turn.ended completed",
			"input.admitted b", "turn.started b", "context.measured", "item.completed assistant ok", "turn.ended completed"},
		calls: []string{codexPost, codexPost, codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			if got := s.WireEvents()[1].ReasoningItems; got != 1 {
				t.Errorf("reasoning items in the second request = %d, want 1", got)
			}
		}},
	{name: "a reasoning-only response fails the turn as retryable",
		opts:   harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"empty": {Reasoning: []string{"plan"}}}},
		steps:  []harnesstest.Step{{Name: "empty", Match: harnesstest.LastUserText("hi")}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "turn.ended failed turn: retryable backend error: modelapi: the response has no output"},
		calls:  []string{codexPost}},
	{name: "the ModelTransport alone can supply the credentials", injected: true,
		opts:   harnesstest.OpenAIOptions{APIKey: "k"},
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost}},
	{name: "the session settings reach the request", effort: "high", tier: "priority",
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			if e, tier := s.WireEvents()[0].ReasoningEffort, s.Requests()[0].ServiceTier; e != "high" || tier != "priority" {
				t.Errorf("effort, service tier = %q, %q, want high, priority", e, tier)
			}
		}},
	{name: "the websocket dial goes through the transport", websocket: true,
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{"codex GET /backend-api/codex/responses"}},
}

func TestCodexTurn(t *testing.T) {
	for _, tc := range codexTurns {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.NewOpenAI(t, tc.opts, tc.steps...)
			r, st, rec := codexRuntime(t, s, tc.websocket, tc.injected, "")
			sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5", Effort: tc.effort, ServiceTier: tc.tier})
			if err != nil {
				t.Fatal(err)
			}
			converse(t, sess, tc.inputs...)
			wantLog(t, st, 2, tc.want...)
			if got := rec.Calls(); !reflect.DeepEqual(got, tc.calls) {
				t.Errorf("transport calls = %q, want %q", got, tc.calls)
			}
			if tc.check != nil {
				tc.check(t, s)
			}
			if u := sess.View().Usage; strings.HasSuffix(tc.want[len(tc.want)-1], "completed") && (u.InputTokens == 0 || u.OutputTokens == 0) {
				t.Errorf("usage = %+v, want the reported tokens", u)
			}
		})
	}
}

// TestEachWireRunsATurn runs a turn on each wire with the key in the
// environment, and with the key only in the ModelTransport.
func TestEachWireRunsATurn(t *testing.T) {
	hi := harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}
	for _, tc := range []struct {
		model, path string
		serve       func(testing.TB, ...harnesstest.Step) *harnesstest.Server
		p           config.Provider
	}{
		{"anthropic/claude-opus-5", "", harnesstest.New, config.Provider{APIKeyEnv: "HARNESS_TEST_KEY"}},
		{"bifrost/fireworks/accounts/fireworks/routers/firerouter", "/v1", harnesstest.NewChat,
			config.Provider{Type: config.TypeOpenAICompat, APIKeyEnv: "HARNESS_TEST_KEY"}},
	} {
		for _, injected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s injected=%t", tc.model, injected), func(t *testing.T) {
				envKey, key := "k", ""
				if injected {
					envKey, key = "", "k"
				}
				t.Setenv("HARNESS_TEST_KEY", envKey)
				s := tc.serve(t, hi)
				tc.p.BaseURL = s.URL() + tc.path
				name, model, _ := strings.Cut(tc.model, "/")
				st, rec := harness.NewMemStore(), &transport{}
				r, err := harness.New(harness.Options{Store: st, Config: config.Config{Providers: map[string]config.Provider{name: tc.p}},
					ModelTransport: func(provider string) http.RoundTripper { return tagged{rec, provider, key} }})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { closeRuntime(t, r) })
				sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: tc.model})
				if err != nil {
					t.Fatal(err)
				}
				converse(t, sess, "hi")
				wantLog(t, st, 2, "input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed")
				if got := s.Requests()[0]; got.Model != model || got.MaxTokens < 1 {
					t.Errorf("request model, max tokens = %q, %d, want %q and a cap", got.Model, got.MaxTokens, model)
				}
				if got := rec.Calls(); len(got) != 1 || !strings.HasPrefix(got[0], name+" POST ") {
					t.Errorf("transport calls = %q, want one %s POST", got, name)
				}
			})
		}
	}
}

func TestCreateChecksTheModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		cfg   func(*config.Config)
		want  error
	}{
		{name: "a known model of a configured provider", model: "codex/gpt-5"},
		{name: "a native openai entry with no type", model: "openai/gpt-5"},
		{name: "a provider with no entry", model: "nope/gpt-5", want: harness.ErrModelUnavailable},
		{name: "a ref with no provider", model: "gpt-5", want: harness.ErrInvalidRequest},
		{name: "a model that modelmeta does not know", model: "codex/no-such-model", want: harness.ErrModelUnavailable},
		{name: "an unknown model with a configured context window", model: "codex/no-such-model",
			cfg: func(c *config.Config) { c.ContextWindowTokens = 1000 }},
		{name: "an unknown model when the context window is not required", model: "codex/no-such-model",
			cfg: func(c *config.Config) { c.ContextWindowRequired = new(false) }},
		{name: "a native anthropic entry with no type", model: "anthropic/claude-opus-5"},
		{name: "an openai-compat entry", model: "bifrost/fireworks/accounts/fireworks/routers/firerouter"},
		{name: "an openrouter entry that names only its key", model: "openrouter/vendor/model",
			cfg: func(c *config.Config) {
				c.ContextWindowTokens = 1000
				c.Providers["openrouter"] = config.Provider{APIKeyEnv: "OPENROUTER_TEST_KEY"}
			}},
		{name: "a provider entry of an unknown type fails New", model: "codex/gpt-5", want: harness.ErrInvalidRequest,
			cfg: func(c *config.Config) { c.Providers["bad"] = config.Provider{Type: "bogus"} }},
		{name: "an empty model takes the configured model", model: "", want: harness.ErrModelUnavailable,
			cfg: func(c *config.Config) { c.Model = "nope/gpt-5" }},
		{name: "an empty model with no configured model takes the default model", model: ""},
		{name: "an empty model resolves a configured alias", model: "", want: harness.ErrModelUnavailable,
			cfg: func(c *config.Config) { c.Model, c.Aliases = "fast", map[string]string{"fast": "nope/gpt-5"} }},
		{name: "an explicit model gets no alias lookup", model: "fast", want: harness.ErrInvalidRequest,
			cfg: func(c *config.Config) { c.Aliases = map[string]string{"fast": "codex/gpt-5"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI, BaseURL: "https://codex.test"}, "openai": {},
				"anthropic": {}, "bifrost": {Type: config.TypeOpenAICompat, BaseURL: "https://bifrost.test"}}}
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: cfg})
			if err == nil {
				t.Cleanup(func() { closeRuntime(t, r) })
				_, err = r.Create(bg, protocol.CreateSession{Model: tc.model})
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Create(%q) = %v, want %v", tc.model, err, tc.want)
			}
		})
	}
}

func TestModelsListsTheConfiguredProviders(t *testing.T) {
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: config.Config{Providers: map[string]config.Provider{
		"codex":       {Type: config.TypeOpenAI, BaseURL: "https://codex.test"},
		"claude-code": {Type: config.TypeClaudeCodeCLI},
		"work":        {Type: config.TypeOpenAI, BaseURL: "https://work.test"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	var got []string
	for _, m := range r.Models() {
		got = append(got, fmt.Sprintf("%s %s %d", m.ID, m.Provider, m.ContextWindow))
		if _, err := r.Create(bg, protocol.CreateSession{Model: m.ID}); err != nil {
			t.Errorf("Create(%s): %v", m.ID, err)
		}
	}
	want := []string{"claude-code/fable claude-code 0", "claude-code/haiku claude-code 0", "claude-code/opus claude-code 0",
		"claude-code/sonnet claude-code 0", "codex/gpt-6-astra codex 1050000", "codex/gpt-6-luna codex 1050000", "codex/gpt-6-sol codex 1050000"}
	if !slices.Equal(got, want) {
		t.Errorf("Models =\n%q\nwant\n%q", got, want)
	}
}

func TestTheSystemPromptIsReadWhenTheSessionStarts(t *testing.T) {
	wd := t.TempDir()
	rules := func(body string) {
		if err := os.WriteFile(wd+"/AGENTS.md", []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, harnesstest.Step{Name: "any", Repeat: true, Reply: harnesstest.Reply{Text: "ok"}})
	r, _, _ := codexRuntime(t, srv, false, false, wd)
	for _, id := range []string{"s1", "s2"} {
		rules(id)
		sess, err := r.Create(bg, protocol.CreateSession{ID: id, Model: "codex/gpt-5"})
		if err != nil {
			t.Fatal(err)
		}
		rules("late")
		converse(t, sess, "a", "b")
	}
	got := ""
	for _, req := range srv.Requests() {
		got += req.System[strings.LastIndex(req.System, "\n")+1:] + ","
	}
	if got != "s1,s1,s2,s2," {
		t.Errorf("last line of the system prompt per request = %q, want the file as it was at session start", got)
	}
}
