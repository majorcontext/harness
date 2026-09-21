package command

import (
	"errors"
	"testing"
)

// TestResolveRejectsSurplusInput is the named-failure test for parse rule
// 1. Claude Code matched /clear, passed "is just an alias for new" as
// arguments, ran the command, and discarded the sentence. A command that
// takes no arguments must reject trailing text instead.
func TestResolveRejectsSurplusInput(t *testing.T) {
	r := NewRegistry()
	res, err := r.Resolve("/clear is just an alias for new")
	if err == nil {
		t.Fatalf("Resolve returned no error; got %+v, want a surplus-input error", res)
	}
	if errors.Is(err, ErrNotCommand) {
		t.Fatalf("Resolve error = %v, want a surplus-input error, not ErrNotCommand", err)
	}
	if res.Op != "" || res.Spec != nil {
		t.Errorf("Resolve returned a dispatchable resolution %+v; nothing must run", res)
	}
}

// TestResolveEscape pins parse rule 3: a user must be able to write about
// a command. //clear is the literal text /clear.
func TestResolveEscape(t *testing.T) {
	r := NewRegistry()
	res, err := r.Resolve("//clear")
	if !errors.Is(err, ErrNotCommand) {
		t.Fatalf("Resolve(\"//clear\") error = %v, want ErrNotCommand", err)
	}
	if res.Text != "/clear" {
		t.Errorf("Resolve(\"//clear\").Text = %q, want %q", res.Text, "/clear")
	}
}

// TestResolveMultiLineIsNotACommand pins parse rule 2.
func TestResolveMultiLineIsNotACommand(t *testing.T) {
	r := NewRegistry()
	const in = "/compact\nand then explain what you did"
	res, err := r.Resolve(in)
	if !errors.Is(err, ErrNotCommand) {
		t.Fatalf("Resolve error = %v, want ErrNotCommand", err)
	}
	if res.Text != in {
		t.Errorf("Resolve().Text = %q, want the line unchanged", res.Text)
	}
}

func TestResolveTable(t *testing.T) {
	r := NewRegistry()
	tests := []struct {
		name     string
		in       string
		wantOp   Op
		wantArgs map[string]any
		wantErr  bool
		wantText string
	}{
		{name: "int arg is an int", in: "/compact 5", wantOp: OpCompact,
			wantArgs: map[string]any{"keep_turns": 5}},
		{name: "optional arg may be absent", in: "/compact", wantOp: OpCompact,
			wantArgs: map[string]any{}},
		{name: "non-numeric int arg", in: "/compact abc", wantErr: true},
		{name: "required arg missing", in: "/model", wantErr: true},
		{name: "string arg", in: "/model anthropic/claude-sonnet-5",
			wantOp: OpSetModel, wantArgs: map[string]any{"model": "anthropic/claude-sonnet-5"}},
		{name: "rest arg takes the remainder", in: "/goal the tests pass and CI is green",
			wantOp: OpSetGoal, wantArgs: map[string]any{"condition": "the tests pass and CI is green"}},
		{name: "unknown command", in: "/nope", wantErr: true},
		{name: "plain text", in: "review this", wantText: "review this"},
		{name: "leading space is text", in: " /compact", wantText: " /compact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := r.Resolve(tt.in)
			switch {
			case tt.wantErr:
				if err == nil || errors.Is(err, ErrNotCommand) {
					t.Fatalf("Resolve(%q) error = %v, want a command error", tt.in, err)
				}
				return
			case tt.wantText != "":
				if !errors.Is(err, ErrNotCommand) {
					t.Fatalf("Resolve(%q) error = %v, want ErrNotCommand", tt.in, err)
				}
				if res.Text != tt.wantText {
					t.Errorf("Text = %q, want %q", res.Text, tt.wantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) error = %v, want nil", tt.in, err)
			}
			if res.Op != tt.wantOp {
				t.Errorf("Op = %q, want %q", res.Op, tt.wantOp)
			}
			if len(res.Args) != len(tt.wantArgs) {
				t.Fatalf("Args = %#v, want %#v", res.Args, tt.wantArgs)
			}
			for k, want := range tt.wantArgs {
				got, ok := res.Args[k]
				if !ok {
					t.Errorf("Args missing %q", k)
					continue
				}
				if got != want {
					t.Errorf("Args[%q] = %#v (%T), want %#v (%T)", k, got, got, want, want)
				}
			}
		})
	}
}

// TestResolveUnknownIsNotText pins that an unknown /name never reaches the
// model as literal text.
func TestResolveUnknownIsNotText(t *testing.T) {
	r := NewRegistry()
	var unknown *UnknownCommandError
	_, err := r.Resolve("/nope")
	if !errors.As(err, &unknown) {
		t.Fatalf("Resolve(\"/nope\") error = %v, want *UnknownCommandError", err)
	}
	if unknown.Name != "nope" {
		t.Errorf("UnknownCommandError.Name = %q, want %q", unknown.Name, "nope")
	}
}
