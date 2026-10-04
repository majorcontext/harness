package harness_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/prompt"
	"github.com/majorcontext/harness/protocol"
)

func TestTheSystemPromptIsReadWhenTheSessionStarts(t *testing.T) {
	wd, st := t.TempDir(), harness.NewMemStore()
	rules := func(body string) error { return os.WriteFile(filepath.Join(wd, "AGENTS.md"), []byte(body), 0o644) }
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, harnesstest.Step{Name: "any", Repeat: true, Reply: harnesstest.Reply{Text: "ok"}})
	t.Setenv("HARNESS_TEST_KEY", "k")
	cfg := config.Config{AppendSystemPrompt: []string{"PLATFORM"}, Providers: map[string]config.Provider{"codex": {Type: config.TypeOpenAI,
		APIKeyEnv: "HARNESS_TEST_KEY", BaseURL: s.URL() + "/backend-api/codex", ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}}}
	r, err := harness.New(harness.Options{Store: st, WorkDir: wd, Config: cfg})
	if err = errors.Join(err, rules("first rule")); err != nil {
		t.Fatal(err)
	}
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err = errors.Join(err, rules("second rule")); err != nil {
		t.Fatal(err)
	}
	converse(t, sess, "a", "b")
	closeRuntime(t, r)
	if r, err = harness.New(harness.Options{Store: st, WorkDir: wd, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	if sess, err = r.Open(bg, "s1"); err != nil {
		t.Fatal(err)
	}
	watch(t, sess, sess.View().HeadSeq-1, sess.View().HeadSeq, text("c", "c"), false)
	var got []string
	for _, req := range s.Requests() {
		got = append(got, strings.TrimPrefix(req.System, prompt.Base(wd)+"\n\nPLATFORM\n\nProject instructions from AGENTS.md:\n\n"))
	}
	if !slices.Equal(got, []string{"first rule", "first rule", "second rule"}) {
		t.Errorf("AGENTS.md text after the base prompt and PLATFORM, per request = %q, want first, first, second", got)
	}
}
