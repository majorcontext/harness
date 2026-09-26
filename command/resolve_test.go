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

// TestResolveTabSeparatesNameFromArgs pins the asymmetry in rule 4:
// whitespace BEFORE the name makes the line text, because dropping it
// would change what parses; whitespace AFTER a complete name is a
// separator and drops no content.
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
		{name: "plain text", in: "review this", wantText: "review this"},
		{name: "leading space is text", in: " /compact", wantText: " /compact"},
		{name: "rule 3: // is a literal slash", in: "//clear", wantText: "/clear"},
		{name: "rule 2: multi-line is text", in: "/compact\nand explain", wantText: "/compact\nand explain"},
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

// TestBindArgsSplitsOnUnicodeWhitespace pins that a positional argument
// splits on any Unicode whitespace, so a folded non-ASCII space cannot
// smuggle a second token into a single-value field. /thinking takes one
// argument, so the second token must be rejected as surplus.
func TestBindArgsSplitsOnUnicodeWhitespace(t *testing.T) {
	r := NewRegistry()
	tests := []struct {
		name string
		in   string
	}{
		{name: "NBSP", in: "/thinking high\u00A0junk"},
		{name: "ideographic space", in: "/thinking high\u3000junk"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := r.Resolve(tt.in)
			if err == nil {
				t.Fatalf("Resolve(%q) = %+v, want a surplus-input error", tt.in, res)
			}
			if errors.Is(err, ErrNotCommand) {
				t.Fatalf("Resolve(%q) error = %v, want a surplus-input error, not ErrNotCommand", tt.in, err)
			}
			if res.Op != "" || res.Spec != nil {
				t.Errorf("Resolve(%q) returned a dispatchable resolution %+v; nothing must run", tt.in, res)
			}
		})
	}
}

// TestResolveArgRestKeepsInternalWhitespace pins that ArgRest binds the
// remainder verbatim: the per-field whitespace split must not reach it.
func TestResolveArgRestKeepsInternalWhitespace(t *testing.T) {
	r := NewRegistry()
	const in = "/goal a b  c"
	res, err := r.Resolve(in)
	if err != nil {
		t.Fatalf("Resolve(%q) error = %v, want nil", in, err)
	}
	want := "a b  c"
	if got := res.Args["condition"]; got != want {
		t.Errorf("Resolve(%q) Args[condition] = %q, want %q", in, got, want)
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

func TestResolveArgErrorNamesSpec(t *testing.T) {
	r := NewRegistry()
	for _, line := range []string{"/compact abc", "/status now", "/model", "/model a b"} {
		_, err := r.Resolve(line)
		var ae *ArgsError
		if !errors.As(err, &ae) || ae.Spec == nil {
			t.Fatalf("%q: err %v is not *ArgsError", line, err)
		}
	}
	_, err := r.Resolve("/compact abc")
	if want := `command: /compact keep_turns must be a number, got "abc"`; err.Error() != want {
		t.Fatalf("text changed: %q", err)
	}
}
