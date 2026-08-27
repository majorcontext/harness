package engine

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// isBatchingSegment reports whether seg is the tool-batching system
// segment. The other segment-layout tests in this package use it so a
// wording change to toolBatchingSegment does not have to touch every one
// of them; only the assertions in this file pin the actual text.
func isBatchingSegment(seg string) bool {
	return strings.HasPrefix(seg, "If you intend to call multiple tools")
}

// batchingSession runs one prompt and returns the assembled system prompt.
func batchingSystem(t *testing.T, cfg Config) []string {
	t.Helper()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg.Providers = provider.Registry{"test": prov}
	cfg.Model = message.ModelRef{Provider: "test", Model: "m1"}
	if cfg.System == nil {
		cfg.System = []string{"base"}
	}
	if cfg.Instructions == nil {
		cfg.Instructions = &InstructionsConfig{Disabled: true}
	}
	if cfg.SkillsDirs == nil {
		cfg.SkillsDirs = []string{}
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	return prov.requests[0].System
}

// TestToolBatchingSegmentPresentByDefault pins the segment's position and
// its content. It sits immediately after the caller's base prompt, ahead of
// project instructions, because it describes how the engine executes tools
// rather than anything about the project.
func TestToolBatchingSegmentPresentByDefault(t *testing.T) {
	sys := batchingSystem(t, Config{})
	if len(sys) != 2 {
		t.Fatalf("system = %v, want [base, tool-batching]", sys)
	}
	if sys[0] != "base" {
		t.Errorf("sys[0] = %q, want the caller's base prompt first", sys[0])
	}
	seg := sys[1]
	if !isBatchingSegment(seg) {
		t.Fatalf("sys[1] = %q, want the tool-batching segment", seg)
	}
	// Both halves must survive any future edit. The second is as
	// load-bearing as the first: without it a model batches calls whose
	// arguments depend on an earlier call's result.
	if !strings.Contains(seg, "no dependencies between the calls") {
		t.Errorf("segment lost the independence condition: %q", seg)
	}
	if !strings.Contains(seg, "same message") {
		t.Errorf("segment does not say where to put the calls: %q", seg)
	}
	if !strings.Contains(seg, "MUST wait for previous calls to finish") {
		t.Errorf("segment lost the dependency caveat: %q", seg)
	}
	if !strings.Contains(seg, "up to 8 at a time") {
		t.Errorf("segment should name the default cap: %q", seg)
	}
}

// TestToolBatchingSegmentRendersTheRealCap proves the number the model
// reads is the number the executor enforces, not a hardcoded 8.
func TestToolBatchingSegmentRendersTheRealCap(t *testing.T) {
	for _, cap := range []int{2, 4, 16} {
		sys := batchingSystem(t, Config{ToolConcurrency: cap})
		if len(sys) != 2 {
			t.Fatalf("cap %d: system = %v, want the segment present", cap, sys)
		}
		want := "up to " + strconv.Itoa(cap) + " at a time"
		if !strings.Contains(sys[1], want) {
			t.Errorf("cap %d: segment does not say %q: %q", cap, want, sys[1])
		}
	}
}

// TestToolBatchingSegmentAbsentWhenSequential is the important negative.
// A session that runs tools one at a time — an operator who set the kill
// switch, or an embedder who set ToolConcurrency 1 — must not be told its
// calls run concurrently, because for that session they do not.
func TestToolBatchingSegmentAbsentWhenSequential(t *testing.T) {
	for _, cap := range []int{1, -1} {
		sys := batchingSystem(t, Config{ToolConcurrency: cap})
		if len(sys) != 1 || sys[0] != "base" {
			t.Errorf("ToolConcurrency %d: system = %v, want only [base] — a sequential session must not be told its calls run concurrently", cap, sys)
		}
	}
}

// TestToolBatchingSegmentIsStableAcrossTurns keeps the segment usable as a
// prompt-cache prefix: it must not vary from one request to the next.
func TestToolBatchingSegmentIsStableAcrossTurns(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "one"}),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "two"}),
	}}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		System:       []string{"base"},
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	for _, p := range []string{"first", "second"} {
		if _, err := s.Prompt(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(prov.requests))
	}
	if a, b := prov.requests[0].System[1], prov.requests[1].System[1]; a != b {
		t.Errorf("segment changed between turns:\n%q\n%q", a, b)
	}
}
