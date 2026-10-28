package e2e

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
)

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

func TestContractRuntimeSkillsRefresh(t *testing.T) {
	skipShort(t)
	t.Run("a_skill_added_mid_session_loads", rowASkillAddedMidSessionLoads)
	t.Run("a_removed_skill_fails_to_load", rowARemovedSkillFailsToLoad)
	t.Run("a_failed_list_keeps_the_last_skills", rowAFailedListKeepsTheLastSkills)
}

// loadAfter runs a turn that calls load_skill for name after change, and returns the text of the last request.
func loadAfter(t *testing.T, src *editableSkills, name string, change func()) string {
	t.Helper()
	r, fake := skillsRuntime(t, src, harnesstest.Step{Name: "one", Reply: harnesstest.Reply{Text: "ok"}}, loadSkillStep(map[string]any{"name": name}),
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}})
	s := skillsCreate(t, r)
	sessionMCPTurn(t, s, "a", "one")
	change()
	sessionMCPTurn(t, s, "b", "load it")
	reqs := fake.Requests()
	return requestText(reqs[len(reqs)-1])
}

func rowASkillAddedMidSessionLoads(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	got := loadAfter(t, src, "invoices", func() { src.set(nil, skillRefunds, skillInvoice) })
	if !strings.Contains(got, "body of invoices") {
		t.Errorf("last request does not hold the loaded skill: %s", got)
	}
}

func rowARemovedSkillFailsToLoad(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds, skillInvoice)
	got := loadAfter(t, src, "invoices", func() { src.set(nil, skillRefunds) })
	if !strings.Contains(got, "unknown skill invoices") {
		t.Errorf("last request does not hold the load error: %s", got)
	}
}

func rowAFailedListKeepsTheLastSkills(t *testing.T) {
	t.Parallel()
	src := &editableSkills{}
	src.set(nil, skillRefunds)
	got := loadAfter(t, src, "refunds", func() { src.set(errors.New("database down")) })
	if !strings.Contains(got, "body of refunds") {
		t.Errorf("last request does not hold the loaded skill: %s", got)
	}
}
