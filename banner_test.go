package harness_test

import (
	"net/http"
	"regexp"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

var bannerText = regexp.MustCompile(`(?s)<harness-engine-context>\n\[engine: harness 9\.9\.9 · session_sync=fsync · engine started \d{4}-\d\d-\d\dT[^\]]+Z\]\n</harness-engine-context>`)

func bannerRuntime(t *testing.T, s *harnesstest.OpenAI, version string, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Version: version, Tools: tools,
		Config:         config.Config{PromptRetries: &retries, Providers: map[string]config.Provider{"codex": p}},
		ModelTransport: func(string) http.RoundTripper { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	return r
}

func bannerSteps() []harnesstest.Step {
	return []harnesstest.Step{callStep("echo"),
		{Name: "done", Match: harnesstest.LastToolResult("echo"), Reply: harnesstest.Reply{Text: "done"}},
		{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}}}
}

func TestEngineBannerStaysAtItsFirstPosition(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, bannerSteps()...)
	r := bannerRuntime(t, s, "9.9.9", newProbe("echo", false))
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "run", "again")
	want := [][]string{
		{"user text :run", "user text :<banner>"},
		{"user text :run", "user text :<banner>", "assistant tool_use echo:", "user tool_result echo:echo ran {\"x\":1}"},
		{"user text :run", "user text :<banner>", "assistant tool_use echo:", "user tool_result echo:echo ran {\"x\":1}",
			"assistant text :done", "user text :again"},
	}
	reqs := s.Requests()
	if len(reqs) != len(want) {
		t.Fatalf("model requests = %d, want %d", len(reqs), len(want))
	}
	for i, req := range reqs {
		var got []string
		for _, line := range transcript(req) {
			got = append(got, bannerText.ReplaceAllString(line, "<banner>"))
		}
		if !slices.Equal(got, want[i]) {
			t.Errorf("request %d =\n%q\nwant\n%q", i, got, want[i])
		}
	}
}

func TestNoVersionSendsNoEngineBanner(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, bannerSteps()[2])
	r := bannerRuntime(t, s, "")
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "again")
	if got := transcript(s.Requests()[0]); !slices.Equal(got, []string{"user text :again"}) {
		t.Errorf("request = %q, want the input alone", got)
	}
}
