package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// reqSnapshot captures the parts of a request an OnRequest observer cares about,
// copied out so a later assertion is not fooled by the shared *provider.Request.
type reqSnapshot struct {
	turn     int
	system   []string
	tools    []string
	messages int
}

func snapshotRequest(turn int, req *provider.Request) reqSnapshot {
	var tools []string
	for _, td := range req.Tools {
		tools = append(tools, td.Name)
	}
	return reqSnapshot{
		turn:     turn,
		system:   append([]string(nil), req.System...),
		tools:    tools,
		messages: len(req.Messages),
	}
}

// TestOnRequestFiresPerTurnWithAssembledSystem verifies OnRequest sees the exact
// final system prompt (base, instructions, skills, hook segments — in that
// order) once per model call, with turns counted from 1.
func TestOnRequestFiresPerTurnWithAssembledSystem(t *testing.T) {
	work := t.TempDir()
	writeInstr(t, filepath.Join(work, "AGENTS.md"), "instr body")
	skills := filepath.Join(work, "skills")
	writeSkill(t, skills, "one", "Skill one")
	hooks := &fakeHooks{segments: []string{"hook seg"}}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	var seen []reqSnapshot
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		System:     []string{"base"},
		WorkDir:    work,
		SkillsDirs: []string{skills},
		Hooks:      hooks,
		OnRequest:  func(turn int, req *provider.Request) { seen = append(seen, snapshotRequest(turn, req)) },
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("OnRequest fired %d times, want 2 (tool-use turn then final turn)", len(seen))
	}
	if seen[0].turn != 1 || seen[1].turn != 2 {
		t.Errorf("turn numbers = %d,%d, want 1,2", seen[0].turn, seen[1].turn)
	}

	sys := seen[0].system
	if len(sys) != 4 {
		t.Fatalf("system = %v, want [base, instructions, skills, hook seg]", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want base", sys[0])
	}
	if !strings.Contains(sys[1], "instr body") {
		t.Errorf("sys[1] = %q, want instructions", sys[1])
	}
	if !strings.Contains(sys[2], "one — Skill one") {
		t.Errorf("sys[2] = %q, want skills", sys[2])
	}
	if sys[3] != "hook seg" {
		t.Errorf("sys[3] = %q, want hook seg", sys[3])
	}
}

// TestOnRequestIncludesToolsAndHistory verifies the observed request carries the
// tool defs and the full running history (which grows across the tool loop).
func TestOnRequestIncludesToolsAndHistory(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo hi"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	var seen []reqSnapshot
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		OnRequest: func(turn int, req *provider.Request) { seen = append(seen, snapshotRequest(turn, req)) },
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 {
		t.Fatalf("OnRequest fired %d times, want 2", len(seen))
	}
	// First turn: only the user message.
	if seen[0].messages != 1 {
		t.Errorf("turn 1 history = %d messages, want 1 (user)", seen[0].messages)
	}
	// Second turn: user, assistant(tool call), tool(result).
	if seen[1].messages != 3 {
		t.Errorf("turn 2 history = %d messages, want 3", seen[1].messages)
	}
	if !containsStr(seen[0].tools, "bash") {
		t.Errorf("tools = %v, want to include bash", seen[0].tools)
	}
}

// TestOnRequestNilNoop verifies a nil OnRequest is a no-op: the prompt still runs.
func TestOnRequestNilNoop(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		// OnRequest deliberately nil.
	})
	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q, want done", final.Parts.Text())
	}
	if len(prov.requests) != 1 {
		t.Errorf("provider called %d times, want 1", len(prov.requests))
	}
}

// TestOnRequestTurnCountsAcrossPrompts verifies the turn counter is per-session,
// continuing to climb across separate Prompt calls.
func TestOnRequestTurnCountsAcrossPrompts(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	var turns []int
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		OnRequest: func(turn int, _ *provider.Request) { turns = append(turns, turn) },
	})
	if _, err := s.Prompt(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0] != 1 || turns[1] != 2 {
		t.Errorf("turns = %v, want [1 2]", turns)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
