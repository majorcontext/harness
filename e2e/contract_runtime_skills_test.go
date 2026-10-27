package e2e

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

type readCall struct{ session, name, file string }

type fakeSkills struct {
	mu    sync.Mutex
	reads []readCall
}

func (f *fakeSkills) List(_ context.Context, session string) ([]harness.SkillInfo, error) {
	return []harness.SkillInfo{{Name: "refunds", Description: "Process refund requests"}}, nil
}

func (f *fakeSkills) Read(_ context.Context, session, name, file string) ([]byte, error) {
	f.mu.Lock()
	f.reads = append(f.reads, readCall{session, name, file})
	f.mu.Unlock()
	return []byte("# Refunds\nRefund within 30 days.\n"), nil
}

func (f *fakeSkills) calls() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]readCall(nil), f.reads...)
}

func skillsRuntime(t *testing.T, src harness.SkillSource, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.NewChat(t, steps...)
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Skills: src, Getenv: func(string) string { return "k" }, Config: config.Config{
		ContextWindowTokens: 100000,
		Providers:           map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "SKILLS_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, fake
}

func loadSkillStep(input map[string]any) harnesstest.Step {
	return harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "load_skill", Input: input}}}}
}

func TestContractRuntimeSkills(t *testing.T) {
	skipShort(t)
	t.Run("an_embedded_skill_is_listed_and_load_skill_reads_it_from_the_source", func(t *testing.T) {
		t.Parallel()
		src := &fakeSkills{}
		r, fake := skillsRuntime(t, src, loadSkillStep(map[string]any{"name": "refunds"}), harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
		s := skillsCreate(t, r)
		sessionMCPTurn(t, s, "a", "refund")
		reqs := fake.Requests()
		if len(reqs) != 2 {
			t.Fatalf("model requests = %d, want 2", len(reqs))
		}
		if !strings.Contains(reqs[0].System, "refunds — Process refund requests (load with load_skill)") || !requestHas(reqs[0], "load_skill") {
			t.Errorf("first request: system %q, tools %v, want the embedded skill line and load_skill", reqs[0].System, reqs[0].Tools)
		}
		if got := requestText(reqs[1]); !strings.Contains(got, "Refund within 30 days.") {
			t.Errorf("second request does not hold the SKILL.md text: %s", got)
		}
		if got, want := src.calls(), []readCall{{"s1", "refunds", "SKILL.md"}}; len(got) != 1 || got[0] != want[0] {
			t.Errorf("Read calls = %v, want %v", got, want)
		}
	})
	t.Run("load_skill_of_an_unlisted_name_fails_without_a_read", func(t *testing.T) {
		t.Parallel()
		src := &fakeSkills{}
		r, fake := skillsRuntime(t, src, loadSkillStep(map[string]any{"name": "nope"}), harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
		s := skillsCreate(t, r)
		sessionMCPTurn(t, s, "a", "go")
		if got := requestText(fake.Requests()[1]); !strings.Contains(got, "unknown skill nope") {
			t.Errorf("second request does not hold the error: %s", got)
		}
		if got := src.calls(); len(got) != 0 {
			t.Errorf("Read calls = %v, want none", got)
		}
	})
	t.Run("an_embedder_tool_may_not_take_the_name_load_skill_with_a_skill_source", func(t *testing.T) {
		t.Parallel()
		_, err := harness.New(harness.Options{Store: store.NewMemStore(), Skills: &fakeSkills{}, Tools: []harness.Tool{namedTool("load_skill")}})
		if err == nil {
			t.Fatal("New = nil, want an error for the repeated tool name")
		}
	})
}

func skillsCreate(t *testing.T, r *harness.Runtime) *harness.Session {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
