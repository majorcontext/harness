package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestRunModeOpsAreDeclared pins the support matrix in both directions:
// every Op the run dispatcher claims must be a real control Op, and every
// control Op must be either supported or explicitly refused. A new Op
// that nobody handles fails here instead of going silent at runtime.
func TestRunModeOpsAreDeclared(t *testing.T) {
	registry := command.NewRegistry()
	control := map[command.Op]bool{}
	for _, s := range registry.All() {
		if s.Kind == command.KindControl {
			control[s.Op] = true
		}
	}
	for op := range runModeOps {
		if !control[op] {
			t.Errorf("runModeOps names %q, which is not a control Op", op)
		}
	}
	for op := range control {
		if _, ok := runModeOps[op]; !ok {
			t.Errorf("control Op %q is neither supported nor refused by run mode", op)
		}
	}
}

// TestUnsupportedOpReportsWhy pins that an unsupported Op fails loudly.
// Silence is the failure to avoid: a user who types /abort in a one-shot
// run must learn that run mode cannot do it.
func TestUnsupportedOpReportsWhy(t *testing.T) {
	res := command.Resolution{Kind: command.KindControl, Op: command.OpAbort}
	err := dispatchCommand(t.Context(), nil, res)
	if err == nil {
		t.Fatal("dispatchCommand returned nil for an unsupported Op")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %q, want it to say the op is not available in this mode", err)
	}
}

// TestFrontendOpIsRefused pins that run mode does not pretend to own the
// session pointer: /new and /clear belong to whoever holds it.
func TestFrontendOpIsRefused(t *testing.T) {
	spec, ok := command.NewRegistry().Lookup("clear")
	if !ok {
		t.Fatal("clear not in registry")
	}
	err := dispatchCommand(t.Context(), nil, command.Resolution{Kind: command.KindFrontend, Spec: spec})
	if err == nil {
		t.Fatal("dispatchCommand returned nil for a frontend command")
	}
}

// TestPromptTextPassesThrough pins that ordinary text is untouched: the
// command layer must not change what a non-command prompt sends.
func TestPromptTextPassesThrough(t *testing.T) {
	r := command.NewRegistry()
	const in = "summarize the diff"
	res, err := r.Resolve(in)
	if !errors.Is(err, command.ErrNotCommand) {
		t.Fatalf("Resolve error = %v, want ErrNotCommand", err)
	}
	if res.Text != in {
		t.Errorf("Text = %q, want %q", res.Text, in)
	}
}

// TestUnknownCommandDoesNotBecomeAPrompt pins the spec's §3 rule: an
// unknown /name is an error, never literal text sent to the model.
func TestUnknownCommandDoesNotBecomeAPrompt(t *testing.T) {
	_, err := command.NewRegistry().Resolve("/nope")
	if err == nil || errors.Is(err, command.ErrNotCommand) {
		t.Fatalf("Resolve(\"/nope\") error = %v, want a command error", err)
	}
}

// TestCompactSkipIsAnError pins spec §5's "silence is the failure to
// avoid": a fresh run-mode session has no prior history to fold, so
// Session.Compact returns a nil error with SkipReasonNotEnoughTurns
// (success, not failure, from the engine's point of view). Discarding
// that result lets `harness run -p "/compact"` print nothing and exit
// 0 even though the user's destructive command did nothing. dispatchCommand
// must turn a non-empty SkipReason into an error naming why.
func TestCompactSkipIsAnError(t *testing.T) {
	sess := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:      t.TempDir(),
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	spec, ok := command.NewRegistry().Lookup("compact")
	if !ok {
		t.Fatal("compact not in registry")
	}
	res := command.Resolution{Kind: command.KindControl, Spec: spec, Op: command.OpCompact, Args: map[string]any{}}

	err := dispatchCommand(t.Context(), sess, res)
	if err == nil {
		t.Fatal("dispatchCommand returned nil for a compact that folded nothing; want an error naming the skip reason")
	}
	if !strings.Contains(err.Error(), "enough turns") {
		t.Errorf("error = %q, want it to name why nothing was compacted", err)
	}
}
