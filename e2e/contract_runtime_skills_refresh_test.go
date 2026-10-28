package e2e

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

const skillUpdateMarker = "The embedded skills changed after the system prompt was written."

// editableSkills is a SkillSource that a test changes between turns.
type editableSkills struct {
	mu    sync.Mutex
	infos []harness.SkillInfo
	err   error
}

func (e *editableSkills) set(err error, infos ...harness.SkillInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.infos, e.err = infos, err
}

func (e *editableSkills) List(context.Context, string) ([]harness.SkillInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]harness.SkillInfo(nil), e.infos...), e.err
}

func (e *editableSkills) Read(_ context.Context, _, name, _ string) ([]byte, error) {
	return []byte("# " + name + "\nbody of " + name + "\n"), nil
}

var (
	skillRefunds = harness.SkillInfo{Name: "refunds", Description: "Process refund requests"}
	skillInvoice = harness.SkillInfo{Name: "invoices", Description: "Explain an invoice"}
)

func skillsAnthropicRuntime(t *testing.T, src harness.SkillSource, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.NewAnthropic(t, steps...)
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Skills: src, Getenv: func(string) string { return "k" }, Config: config.Config{
		ContextWindowTokens: 100000,
		Providers:           map[string]config.Provider{"anthropic": {BaseURL: fake.URL(), APIKeyEnv: "SKILLS_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, fake
}

func anthropicSkillSession(t *testing.T, r *harness.Runtime) *harness.Session {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "anthropic/claude-sonnet-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func okReplies(n int) []harnesstest.Step {
	var out []harnesstest.Step
	for range n {
		out = append(out, harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "ok"}})
	}
	return out
}

// updates counts the skill update segments in the messages of r.
func updates(r harnesstest.Request) int {
	n := 0
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			n += strings.Count(p.Text, skillUpdateMarker)
		}
	}
	return n
}

// stableCache fails when request i does not keep the cached prefix of request i-1: the system prompt, the tool
// specs, every message of the request before, and the cache markers of the system prompt and the newest message.
func stableCache(t *testing.T, reqs []harnesstest.Request) {
	t.Helper()
	for i, req := range reqs {
		last := len(req.Messages) - 1
		want := []harnesstest.CacheBreakpoint{{Message: -1}, {Message: last, Block: len(req.Messages[last].Parts) - 1}}
		if !reflect.DeepEqual(req.CacheBreakpoints, want) {
			t.Errorf("request %d cache breakpoints = %+v, want %+v", i+1, req.CacheBreakpoints, want)
		}
		if i == 0 {
			continue
		}
		prev := reqs[i-1]
		if req.System != reqs[0].System || !reflect.DeepEqual(req.Tools, reqs[0].Tools) || !reflect.DeepEqual(req.ToolSpecs, reqs[0].ToolSpecs) {
			t.Errorf("request %d changed the system prompt or the tools of request 1", i+1)
		}
		if len(req.Messages) < len(prev.Messages) || !reflect.DeepEqual(req.Messages[:len(prev.Messages)], prev.Messages) {
			t.Errorf("request %d does not begin with the messages of request %d:\n%+v\n---\n%+v", i+1, i, req.Messages, prev.Messages)
		}
	}
}

func TestContractRuntimeSkillsRefresh(t *testing.T) {
	skipShort(t)
	t.Run("a_skill_added_mid_session_is_announced_once_after_the_history_and_loads", rowASkillAddedMidSessionIsAnnouncedOnceAfterTheHistoryAndLoads)
	t.Run("an_unchanged_skill_list_adds_no_segment", rowAnUnchangedSkillListAddsNoSegment)
	t.Run("a_removed_skill_is_named_once_and_load_skill_fails_for_it", rowARemovedSkillIsNamedOnceAndLoadSkillFailsForIt)
	t.Run("a_failed_list_keeps_the_last_skills_and_adds_no_segment", rowAFailedListKeepsTheLastSkillsAndAddsNoSegment)
	t.Run("a_skill_added_after_a_reopen_is_pinned_after_the_history", rowASkillAddedAfterAReopenIsPinnedAfterTheHistory)
	t.Run("a_reopened_session_lists_the_changed_skills_in_its_prompt_and_announces_nothing", rowAReopenedSessionListsTheChangedSkillsInItsPromptAndAnnouncesNothing)
}

func rowASkillAddedMidSessionIsAnnouncedOnceAfterTheHistoryAndLoads(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	steps := append(okReplies(3), harnesstest.Step{Name: "load", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "load_skill", Input: map[string]any{"name": "invoices"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}})
	r, fake := skillsAnthropicRuntime(t, src, steps...)
	s := anthropicSkillSession(t, r)
	sessionMCPTurn(t, s, "a", "one")
	src.set(nil, skillRefunds, skillInvoice)
	sessionMCPTurn(t, s, "b", "two")
	sessionMCPTurn(t, s, "c", "three")
	sessionMCPTurn(t, s, "d", "load it")
	reqs := fake.Requests()
	if len(reqs) != 5 {
		t.Fatalf("model requests = %d, want 5", len(reqs))
	}
	stableCache(t, reqs)
	for i, want := range []int{0, 1, 1, 1, 1} {
		if got := updates(reqs[i]); got != want {
			t.Errorf("request %d holds %d skill updates, want %d", i+1, got, want)
		}
	}
	if strings.Contains(reqs[0].System, "invoices") {
		t.Errorf("the first system prompt lists the new skill: %s", reqs[0].System)
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "invoices — Explain an invoice (load with load_skill)") {
		t.Errorf("request 2 does not list the new skill: %s", got)
	}
	if got := requestText(reqs[4]); !strings.Contains(got, "body of invoices") {
		t.Errorf("request 5 does not hold the loaded skill: %s", got)
	}
}

func rowAnUnchangedSkillListAddsNoSegment(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	r, fake := skillsAnthropicRuntime(t, src, okReplies(3)...)
	s := anthropicSkillSession(t, r)
	for _, id := range []string{"a", "b", "c"} {
		sessionMCPTurn(t, s, id, "go")
	}
	reqs := fake.Requests()
	stableCache(t, reqs)
	for i, req := range reqs {
		if updates(req) != 0 || len(req.Messages) != 2*i+1 {
			t.Errorf("request %d: %d skill updates and %d messages, want none and %d", i+1, updates(req), len(req.Messages), 2*i+1)
		}
		for _, m := range req.Messages {
			if len(m.Parts) != 1 {
				t.Errorf("request %d: a message has %d parts, want 1", i+1, len(m.Parts))
			}
		}
	}
}

func rowARemovedSkillIsNamedOnceAndLoadSkillFailsForIt(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds, skillInvoice)
	steps := append(okReplies(1), harnesstest.Step{Name: "load", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "load_skill", Input: map[string]any{"name": "invoices"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}})
	r, fake := skillsAnthropicRuntime(t, src, steps...)
	s := anthropicSkillSession(t, r)
	sessionMCPTurn(t, s, "a", "one")
	src.set(nil, skillRefunds)
	sessionMCPTurn(t, s, "b", "load it")
	reqs := fake.Requests()
	stableCache(t, reqs)
	if updates(reqs[1]) != 1 || updates(reqs[2]) != 1 {
		t.Errorf("skill updates = %d and %d, want 1 and 1", updates(reqs[1]), updates(reqs[2]))
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "Removed, so load_skill fails for them: invoices.") {
		t.Errorf("request 2 does not name the removed skill: %s", got)
	}
	if got := requestText(reqs[2]); !strings.Contains(got, "unknown skill invoices") {
		t.Errorf("request 3 does not hold the load error: %s", got)
	}
}

func rowAFailedListKeepsTheLastSkillsAndAddsNoSegment(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	steps := append(okReplies(1), harnesstest.Step{Name: "load", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "load_skill", Input: map[string]any{"name": "refunds"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}})
	r, fake := skillsAnthropicRuntime(t, src, steps...)
	s := anthropicSkillSession(t, r)
	sessionMCPTurn(t, s, "a", "one")
	src.set(errors.New("database down"))
	sessionMCPTurn(t, s, "b", "load it")
	reqs := fake.Requests()
	stableCache(t, reqs)
	for i, req := range reqs {
		if updates(req) != 0 {
			t.Errorf("request %d holds a skill update", i+1)
		}
	}
	if got := requestText(reqs[2]); !strings.Contains(got, "body of refunds") {
		t.Errorf("request 3 does not hold the loaded skill: %s", got)
	}
}

func rowAReopenedSessionListsTheChangedSkillsInItsPromptAndAnnouncesNothing(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	r, fake := skillsAnthropicRuntime(t, src, okReplies(3)...)
	s := anthropicSkillSession(t, r)
	sessionMCPTurn(t, s, "a", "one")
	src.set(nil, skillRefunds, skillInvoice)
	sessionMCPTurn(t, s, "b", "two")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := r.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "c", "three")
	reqs := fake.Requests()
	if updates(reqs[1]) != 1 || updates(reqs[2]) != 0 {
		t.Errorf("skill updates = %d, then %d after the reopen, want 1, then 0", updates(reqs[1]), updates(reqs[2]))
	}
	if !strings.Contains(reqs[2].System, "invoices — Explain an invoice (load with load_skill)") {
		t.Errorf("the system prompt after the reopen lacks the new skill: %s", reqs[2].System)
	}
}

func rowASkillAddedAfterAReopenIsPinnedAfterTheHistory(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	r, fake := skillsAnthropicRuntime(t, src, okReplies(3)...)
	s := anthropicSkillSession(t, r)
	sessionMCPTurn(t, s, "a", "one")
	sessionMCPTurn(t, s, "b", "two")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := r.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	src.set(nil, skillRefunds, skillInvoice)
	sessionMCPTurn(t, s, "c", "three")
	reqs := fake.Requests()
	stableCache(t, reqs)
	if updates(reqs[2]) != 1 {
		t.Errorf("request 3 holds %d skill updates, want 1", updates(reqs[2]))
	}
}
