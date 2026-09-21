package command

import (
	"errors"
	"strings"
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

// TestResolveArgErrorNamesTheTypedAlias pins that an argument-binding
// error names what the user actually typed, not the canonical Spec
// name it resolved to. /clear is an alias for the "new" spec; a user
// who typed /clear and is told about /new has to work out they are
// the same command.
func TestResolveArgErrorNamesTheTypedAlias(t *testing.T) {
	r := NewRegistry()

	_, err := r.Resolve("/clear extra")
	if err == nil {
		t.Fatalf("Resolve(\"/clear extra\") returned no error, want a surplus-input error")
	}
	if !strings.Contains(err.Error(), "clear") {
		t.Errorf("Resolve(\"/clear extra\") error = %q, want it to mention %q", err.Error(), "clear")
	}
	if strings.Contains(err.Error(), "/new") {
		t.Errorf("Resolve(\"/clear extra\") error = %q, want it not to mention the canonical name %q", err.Error(), "/new")
	}

	_, err = r.Resolve("/new extra")
	if err == nil {
		t.Fatalf("Resolve(\"/new extra\") returned no error, want a surplus-input error")
	}
	if !strings.Contains(err.Error(), "new") {
		t.Errorf("Resolve(\"/new extra\") error = %q, want it to mention %q", err.Error(), "new")
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

// TestResolveRejectsWhitespaceAfterSlash pins rule 4: a line must parse
// as a command without dropping any text, including whitespace right
// after the slash. "/ clear" must not silently become "/clear" and fire
// a destructive frontend command.
func TestResolveRejectsWhitespaceAfterSlash(t *testing.T) {
	r := NewRegistry()
	tests := []struct {
		name string
		in   string
	}{
		{name: "space after slash", in: "/ clear"},
		{name: "tab after slash", in: "/\tclear"},
		{name: "multiple spaces after slash", in: "/  compact 5"},
		{name: "unicode NBSP after slash", in: "/\u00A0clear"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := r.Resolve(tt.in)
			if !errors.Is(err, ErrNotCommand) {
				t.Fatalf("Resolve(%q) error = %v, want ErrNotCommand", tt.in, err)
			}
			if res.Text != tt.in {
				t.Errorf("Resolve(%q).Text = %q, want %q", tt.in, res.Text, tt.in)
			}
			if res.Spec != nil {
				t.Errorf("Resolve(%q) returned Spec %+v, want nil", tt.in, res.Spec)
			}
		})
	}
}

// TestResolveTabSeparatesNameFromArgs pins that any Unicode whitespace
// after a complete command name acts as the separator, exactly as a
// space does. Unlike whitespace BEFORE the name (rule 4), whitespace
// AFTER the name drops no content: it is a separator or trailing
// padding, not text the parse throws away.
func TestResolveTabSeparatesNameFromArgs(t *testing.T) {
	r := NewRegistry()

	res, err := r.Resolve("/clear\t")
	if err != nil {
		t.Fatalf("Resolve(\"/clear\\t\") error = %v, want nil", err)
	}
	if res.Spec == nil || res.Spec.Name != "new" {
		t.Errorf("Resolve(\"/clear\\t\").Spec = %+v, want the %q spec", res.Spec, "new")
	}

	res, err = r.Resolve("/compact\t5")
	if err != nil {
		t.Fatalf("Resolve(\"/compact\\t5\") error = %v, want nil", err)
	}
	if res.Args["keep_turns"] != 5 {
		t.Errorf("Args[%q] = %#v, want 5", "keep_turns", res.Args["keep_turns"])
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
