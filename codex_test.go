package harness_test

import (
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
	opts      harnesstest.OpenAIOptions
	calls     []string
}{
	{name: "the ModelTransport alone can supply the credentials",
		opts:  harnesstest.OpenAIOptions{APIKey: "k"},
		calls: []string{codexPost}},
	{name: "the websocket dial goes through the transport", websocket: true,
		calls: []string{"codex GET /backend-api/codex/responses"}},
}

func TestCodexTurn(t *testing.T) {
	for _, tc := range codexTurns {
		t.Run(tc.name, func(t *testing.T) {
			s := harnesstest.NewOpenAI(t, tc.opts, harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
			r, st, rec := codexRuntime(t, s, tc.websocket, true, "")
			sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
			if err != nil {
				t.Fatal(err)
			}
			converse(t, sess, "hi")
			wantLog(t, st, 2, "input.admitted a", "turn.started a", "context.measured", "item.completed assistant hello", "turn.ended completed")
			if got := rec.Calls(); !reflect.DeepEqual(got, tc.calls) {
				t.Errorf("transport calls = %q, want %q", got, tc.calls)
			}
			if u := sess.View().Usage; u.InputTokens == 0 || u.OutputTokens == 0 {
				t.Errorf("usage = %+v, want the reported tokens", u)
			}
		})
	}
}

// TestEachWireRunsATurn runs a turn on each wire with the key only in the
// ModelTransport.
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
		t.Run(tc.model, func(t *testing.T) {
			t.Setenv("HARNESS_TEST_KEY", "")
			s := tc.serve(t, hi)
			tc.p.BaseURL = s.URL() + tc.path
			name, model, _ := strings.Cut(tc.model, "/")
			st, rec := harness.NewMemStore(), &transport{}
			r, err := harness.New(harness.Options{Store: st, Config: config.Config{Providers: map[string]config.Provider{name: tc.p}},
				ModelTransport: func(provider string) http.RoundTripper { return tagged{rec, provider, "k"} }})
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
