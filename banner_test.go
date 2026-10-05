package harness_test

import (
	"net/http"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestNoVersionSendsNoEngineBanner(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{},
		harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}})
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(),
		Config:         config.Config{PromptRetries: &retries, Providers: map[string]config.Provider{"codex": p}},
		ModelTransport: func(string) http.RoundTripper { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "again")
	if got := transcript(s.Requests()[0]); !slices.Equal(got, []string{"user text :again"}) {
		t.Errorf("request = %q, want the input alone", got)
	}
}
