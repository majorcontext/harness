package harness_test

import (
	"net/http"
	"regexp"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

var bannerText = regexp.MustCompile(`(?s)<harness-engine-context>\n\[engine: harness 9\.9\.9 · session_sync=fsync · engine started \d{4}-\d\d-\d\dT[^\]]+Z\]\n</harness-engine-context>`)

func bannerRuntime(t *testing.T, s *harnesstest.OpenAI, version string, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	return bannerRuntimeOn(t, harness.NewMemStore(), s, version, tools...)
}

func bannerRuntimeOn(t *testing.T, st harness.Store, s *harnesstest.OpenAI, version string, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: s.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: st, Version: version, Tools: tools,
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

func TestEngineBannerHoldsItsPlaceWhenATurnCompactsInTheMiddle(t *testing.T) {
	type step = harnesstest.Step
	type reply = harnesstest.Reply
	answer := func(in string) step {
		return step{Name: in, Match: harnesstest.LastUserText(in), Reply: reply{Text: "re " + in}}
	}
	summarized := func(r harnesstest.Request) bool {
		return r.Messages[0].Parts[0].Text == turn.SummaryBanner+"sum" && harnesstest.LastUserText("charlie")(r)
	}
	s := harnesstest.New(t,
		answer("alpha"), answer("bravo"),
		step{Name: "overflow", Match: harnesstest.LastUserText("charlie"), Reply: reply{HTTPStatus: 400, ErrorMessage: harnesstest.ContextOverflowMessage}},
		step{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: reply{Text: "sum"}},
		step{Name: "call", Match: summarized, Reply: reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "echo", Input: map[string]any{"x": 1}}}}},
		step{Name: "done", Match: harnesstest.LastToolResult("echo"), Reply: reply{Text: "done"}})
	st := harness.NewMemStore()
	first := anthropicBannerRuntime(t, st, s)
	sess, err := first.Create(bg, protocol.CreateSession{ID: "s1", Model: "anthropic/claude-fable-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "alpha", "bravo")
	closeRuntime(t, first)
	woken := anthropicBannerRuntime(t, st, s, newProbe("echo", false))
	if sess, err = woken.Open(bg, "s1"); err != nil {
		t.Fatal(err)
	}
	watch(t, sess, sess.View().HeadSeq-1, sess.View().HeadSeq, text("z", "charlie"), false)
	reqs := s.Requests()
	call, done := transcript(reqs[len(reqs)-2]), transcript(reqs[len(reqs)-1])
	if len(done) < len(call) || !slices.Equal(done[:len(call)], call) {
		t.Errorf("the request after the tool call =\n%q\nwant it to extend the request before it,\n%q", done, call)
	}
}

func anthropicBannerRuntime(t *testing.T, st harness.Store, s *harnesstest.Server, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	t.Setenv("HARNESS_TEST_KEY", "k")
	retries := 0
	r, err := harness.New(harness.Options{Store: st, Version: "9.9.9", Tools: tools, Config: config.Config{PromptRetries: &retries,
		Providers: map[string]config.Provider{"anthropic": {APIKeyEnv: "HARNESS_TEST_KEY", BaseURL: s.URL()}}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
