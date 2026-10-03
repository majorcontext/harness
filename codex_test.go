package harness_test

import (
	"errors"
	"net/http"
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

// codexRuntime runs turns on s with no retries. The key is in the
// environment, or, with injected set, only in the ModelTransport.
func codexRuntime(t *testing.T, s *harnesstest.OpenAI, websocket, injected bool) (*harness.Runtime, harness.Store, *transport) {
	t.Helper()
	envKey, key := "k", ""
	if injected {
		envKey, key = "", "k"
	}
	t.Setenv("HARNESS_TEST_CODEX_KEY", envKey)
	st, rec, retries := harness.NewMemStore(), &transport{}, 0
	r, err := harness.New(harness.Options{
		Store: st,
		Config: config.Config{PromptRetries: &retries, Providers: map[string]config.Provider{"codex": {
			Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY",
			BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses",
			OmitResponseParams: []string{"max_output_tokens"}, UseWebSocketTransport: websocket,
		}}},
		ModelTransport: func(provider string) http.RoundTripper { return tagged{rec, provider, key} },
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
	ended := 0
	for i, text := range texts {
		after := s.View().HeadSeq
		if _, err := s.Submit(bg, protocol.Input{ID: string(rune('a' + i)), Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}}); err != nil {
			t.Fatal(err)
		}
		for e, err := range s.Events(bg, after) {
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind == "turn.ended" {
				ended++
				break
			}
		}
	}
	if ended != len(texts) {
		t.Fatalf("%d turns ended, want %d", ended, len(texts))
	}
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
		want:   []string{"input.admitted a", "turn.started a", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost}},
	{name: "a tool call item is recorded and the next turn sees it",
		steps: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash"}}}},
			{Name: "after", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}}},
		inputs: []string{"run", "again"},
		want: []string{"input.admitted a", "turn.started a", "item.completed assistant call_1", "item.completed tool call_1 " + cutOff, "turn.ended completed",
			"input.admitted b", "turn.started b", "item.completed assistant ok", "turn.ended completed"},
		calls: []string{codexPost, codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			var got []string
			for _, m := range s.Requests()[1].Messages {
				for _, p := range m.Parts {
					got = append(got, m.Role+" "+p.Kind+" "+p.ToolName+":"+p.Text)
				}
			}
			want := []string{"user text :run", "assistant tool_use bash:", "user tool_result bash:[tool error] " + cutOff, "user text :again"}
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
		want:   []string{"input.admitted a", "turn.started a", "turn.ended failed turn: retryable backend error: openai: the response has no output"},
		calls:  []string{codexPost}},
	{name: "the reasoning item of a tool call turn is replayed",
		opts: harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"call": {Reasoning: []string{"plan"}}}},
		steps: []harnesstest.Step{
			{Name: "call", Match: harnesstest.LastUserText("run"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash"}}}},
			{Name: "after", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}}},
		inputs: []string{"run", "again"},
		want: []string{"input.admitted a", "turn.started a", "item.completed assistant plan call_1", "item.completed tool call_1 " + cutOff, "turn.ended completed",
			"input.admitted b", "turn.started b", "item.completed assistant ok", "turn.ended completed"},
		calls: []string{codexPost, codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			if got := s.WireEvents()[1].ReasoningItems; got != 1 {
				t.Errorf("reasoning items in the second request = %d, want 1", got)
			}
		}},
	{name: "a reasoning-only response fails the turn as retryable",
		opts:   harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"empty": {Reasoning: []string{"plan"}}}},
		steps:  []harnesstest.Step{{Name: "empty", Match: harnesstest.LastUserText("hi")}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "turn.ended failed turn: retryable backend error: openai: the response has no output"},
		calls:  []string{codexPost}},
	{name: "the ModelTransport alone can supply the credentials", injected: true,
		opts:   harnesstest.OpenAIOptions{APIKey: "k"},
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost}},
	{name: "the session settings reach the request", effort: "high", tier: "priority",
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{codexPost},
		check: func(t *testing.T, s *harnesstest.OpenAI) {
			if e, tier := s.WireEvents()[0].ReasoningEffort, s.Requests()[0].ServiceTier; e != "high" || tier != "priority" {
				t.Errorf("effort, service tier = %q, %q, want high, priority", e, tier)
			}
		}},
	{name: "the websocket dial goes through the transport", websocket: true,
		steps:  []harnesstest.Step{{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}}},
		inputs: []string{"hi"},
		want:   []string{"input.admitted a", "turn.started a", "item.completed assistant hello", "turn.ended completed"},
		calls:  []string{"codex GET /backend-api/codex/responses"}},
}

func TestCodexTurn(t *testing.T) {
	for _, tc := range codexTurns {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.NewOpenAI(t, tc.opts, tc.steps...)
			r, st, rec := codexRuntime(t, s, tc.websocket, tc.injected)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI}, "openai": {}}}
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeRuntime(t, r) })
			if _, err := r.Create(bg, protocol.CreateSession{Model: tc.model}); !errors.Is(err, tc.want) {
				t.Errorf("Create(%q) = %v, want %v", tc.model, err, tc.want)
			}
		})
	}
}
