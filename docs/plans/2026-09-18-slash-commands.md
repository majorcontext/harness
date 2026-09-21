# Slash Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give harness one command registry that resolves a typed `/name`
line to either a session control operation or a frontend action, and
expose it to clients over `GET /commands` and to `harness run` directly.

**Architecture:** A new pure `command` package owns the vocabulary: kinds,
ops, specs, and a strict parser. It imports neither `engine` nor `server`
and performs no I/O. `server` maps each `Op` to the route that already
implements it and serves the registry as JSON. `cmd/harness` maps the same
`Op` set to method calls on the one session a run holds. The engine learns
no control verb.

**Tech Stack:** Go 1.27.1, standard library only. `net/http`'s
`ServeMux`, `encoding/json`, `testing` with `-race`, `net/http/httptest`.

**Spec:** `docs/design/slash-commands.md`

## Global Constraints

Copied from `AGENTS.md`. Every task's requirements include this section.

- Go 1.27.1. Standard library only. No new dependency. No cgo.
- Run every Go test with `-race`.
- No `time.Sleep` in tests. No guessed `time.After` deadline around an
  in-process wait.
- No `init()` side effects. No disk read before first output. No network
  call and no subprocess before a command needs one.
- `gofmt` and `go vet ./...` must be clean.
- Package boundaries are one-way. `command` must not import `engine`,
  `server`, or `cmd/harness`.
- Model references use `provider/model`. Use `message.ParseModelRef`.
- Prose in `docs/` uses ASD-STE100 Simplified Technical English.
- Conventional Commit subjects: `type(scope): description`, lowercase, no
  final period, about 72 characters or less. No AI-attribution footer.
- Scope is stages 1 through 3 of the spec's §11. Stage 5 (prompt commands
  from `.agents/commands/*.md`) is OUT of scope and gets its own spec and
  plan.

## File Structure

| File | Responsibility |
|---|---|
| `command/command.go` | `Kind`, `Op`, `ArgType`, `ArgSpec`, `Category`, `Spec`. Types only. |
| `command/registry.go` | `Registry`, the builtin table, `Lookup`, `All`. |
| `command/resolve.go` | `Resolve` and the four parse rules. |
| `command/command_test.go` | Registry invariants. |
| `command/resolve_test.go` | Parse rules, arity, argument typing. |
| `server/commands.go` | `opRoutes` map, `handleCommands`. |
| `server/commands_test.go` | `GET /commands` contract. |
| `server/server.go:1011` | One `mux.HandleFunc` line in `routes()`. |
| `server/openapi.yaml` | The `/commands` path entry. |
| `cmd/harness/command.go` | `dispatchCommand`: `Op` to a session method. |
| `cmd/harness/command_test.go` | Run-mode dispatch and rejection. |

`command/` splits by responsibility, not by layer: types, the table, and
the parser each change for different reasons. `server/commands.go` is new
rather than an addition to the 4000-line `server/handlers.go`.

## Spec deviations

Three, each deliberate. Raise them in the pull request body.

1. **`/goal clear` and `/queue clear` become `/goal-clear` and
   `/queue-clear`.** The spec's §4 table writes them as a `clear`
   subcommand. A subcommand is indistinguishable from a goal condition
   literally named `clear`, and the spec's own parse rule 4 forbids
   guessing between two readings. Two names cost nothing and stay
   unambiguous.
2. **`/help` is not in the v1 table.** The spec lists it with "none —
   reads the registry", which would be the one `KindControl` entry with no
   route, breaking the invariant Task 4 asserts. `GET /commands` already
   IS the help text. The spec's deferred question about `/help` stays
   open.
3. **`/status`, `/processes`, and `/mcp` ship as control entries with
   routes but no run-mode support.** See Task 4's support matrix.

---

### Task 1: The command types and registry

**Files:**
- Create: `command/command.go`
- Create: `command/registry.go`
- Test: `command/command_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `command.Kind` (`KindControl`, `KindFrontend`, `KindPrompt`),
  `command.Op` (`OpCompact`, `OpSetModel`, `OpSetThinking`,
  `OpSetServiceTier`, `OpAbort`, `OpSetGoal`, `OpClearGoal`,
  `OpQueueList`, `OpQueueClear`, `OpStatus`, `OpProcessList`, `OpMCP`),
  `command.ArgType` (`ArgString`, `ArgInt`), `command.ArgSpec`,
  `command.Category`, `command.Spec`, `command.Registry`,
  `func NewRegistry() *Registry`,
  `func (*Registry) Lookup(name string) (*Spec, bool)`,
  `func (*Registry) All() []*Spec`.

- [ ] **Step 1: Write the failing test**

Create `command/command_test.go`:

```go
package command

import (
	"sort"
	"testing"
)

// TestRegistryKindInvariants pins the spec's §1 rule: a control command
// names an Op, a frontend command names none. A violation means a client
// cannot tell from GET /commands whether the server or the client owns
// the action.
func TestRegistryKindInvariants(t *testing.T) {
	r := NewRegistry()
	for _, s := range r.All() {
		switch s.Kind {
		case KindControl:
			if s.Op == "" {
				t.Errorf("control command %q has no Op", s.Name)
			}
		case KindFrontend:
			if s.Op != "" {
				t.Errorf("frontend command %q has Op %q, want none", s.Name, s.Op)
			}
		default:
			t.Errorf("command %q has kind %q, want control or frontend", s.Name, s.Kind)
		}
	}
}

// TestRegistrySortedAndUnique pins that All() is menu-ready: sorted, and
// with no name or alias claimed twice. A duplicate would make Lookup's
// answer depend on table order.
func TestRegistrySortedAndUnique(t *testing.T) {
	r := NewRegistry()
	all := r.All()
	names := make([]string, len(all))
	for i, s := range all {
		names[i] = s.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("All() not sorted: %v", names)
	}
	seen := map[string]bool{}
	for _, s := range all {
		for _, n := range append([]string{s.Name}, s.Aliases...) {
			if seen[n] {
				t.Errorf("name or alias %q claimed twice", n)
			}
			seen[n] = true
		}
	}
}

// TestAliasResolvesToSameSpec pins that /clear is the same entry as /new,
// not a copy that can drift.
func TestAliasResolvesToSameSpec(t *testing.T) {
	r := NewRegistry()
	byName, ok := r.Lookup("new")
	if !ok {
		t.Fatal(`Lookup("new") not found`)
	}
	byAlias, ok := r.Lookup("clear")
	if !ok {
		t.Fatal(`Lookup("clear") not found`)
	}
	if byName != byAlias {
		t.Errorf("Lookup(\"clear\") = %p, want the same *Spec as \"new\" (%p)", byAlias, byName)
	}
}

// TestDestructiveMarking pins that every command which loses context or a
// turn is marked, because a frontend gates confirmation on this field.
func TestDestructiveMarking(t *testing.T) {
	want := map[string]bool{"compact": true, "abort": true, "new": true, "queue-clear": true}
	r := NewRegistry()
	for name, wantDestructive := range want {
		s, ok := r.Lookup(name)
		if !ok {
			t.Errorf("Lookup(%q) not found", name)
			continue
		}
		if s.Destructive != wantDestructive {
			t.Errorf("%q Destructive = %v, want %v", name, s.Destructive, wantDestructive)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./command/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the types**

Create `command/command.go`:

```go
// Package command holds the slash-command vocabulary: what a human can
// type, and what each name resolves to. It performs no I/O and imports
// neither engine nor server, so a CLI, a server, and an HTTP client share
// one parse. See docs/design/slash-commands.md.
package command

// Kind separates the three things a name can resolve to.
type Kind string

const (
	// KindControl runs one session operation that already exists.
	KindControl Kind = "control"
	// KindFrontend changes which session the user talks to. It names no
	// Op and no route: the frontend that owns the pointer performs it.
	KindFrontend Kind = "frontend"
	// KindPrompt appends one user message. Reserved for the prompt-command
	// stage; no v1 entry uses it.
	KindPrompt Kind = "prompt"
)

// Op names a session operation abstractly. A route is one dispatcher's
// answer to an Op, not the operation itself.
type Op string

const (
	OpCompact        Op = "compact"
	OpSetModel       Op = "set_model"
	OpSetThinking    Op = "set_thinking"
	OpSetServiceTier Op = "set_service_tier"
	OpAbort          Op = "abort"
	OpSetGoal        Op = "set_goal"
	OpClearGoal      Op = "clear_goal"
	OpQueueList      Op = "queue_list"
	OpQueueClear     Op = "queue_clear"
	OpStatus         Op = "status"
	OpProcessList    Op = "process_list"
	OpMCP            Op = "mcp"
)

// ArgType is the type a dispatcher receives in Resolution.Args.
type ArgType string

const (
	ArgString ArgType = "string"
	ArgInt    ArgType = "int"
	// ArgRest takes the remainder of the line as one string. Only the
	// final ArgSpec of a Spec may use it.
	ArgRest ArgType = "rest"
)

// ArgSpec names one positional argument. Name is the key a dispatcher
// reads out of Resolution.Args.
type ArgSpec struct {
	Name     string
	Type     ArgType
	Optional bool
}

// Category groups commands in a menu.
type Category string

const (
	CategorySession  Category = "session"
	CategoryModel    Category = "model"
	CategoryInfo     Category = "info"
	CategoryFrontend Category = "frontend"
)

// Spec describes one command.
type Spec struct {
	Name        string
	Aliases     []string
	Kind        Kind
	Op          Op
	Summary     string
	ArgHint     string
	Args        []ArgSpec
	Category    Category
	Destructive bool
	Source      string
}
```

- [ ] **Step 4: Write the registry**

Create `command/registry.go`:

```go
package command

import "sort"

// Registry holds the resolved command set. NewRegistry builds it from a
// compiled-in table, so construction reads no disk.
type Registry struct {
	specs  []*Spec
	byName map[string]*Spec
}

func builtins() []*Spec {
	return []*Spec{
		{
			Name: "abort", Kind: KindControl, Op: OpAbort,
			Summary: "Stop the running turn", Category: CategorySession,
			Destructive: true, Source: "builtin",
		},
		{
			Name: "compact", Kind: KindControl, Op: OpCompact,
			Summary: "Summarize history and keep the last turns",
			ArgHint: "[keep_turns]", Category: CategorySession,
			Args:        []ArgSpec{{Name: "keep_turns", Type: ArgInt, Optional: true}},
			Destructive: true, Source: "builtin",
		},
		{
			Name: "goal", Kind: KindControl, Op: OpSetGoal,
			Summary: "Pursue a goal until its condition holds",
			ArgHint: "<condition>", Category: CategorySession,
			Args:    []ArgSpec{{Name: "condition", Type: ArgRest}},
			Source:  "builtin",
		},
		{
			Name: "goal-clear", Kind: KindControl, Op: OpClearGoal,
			Summary: "Stop pursuing the current goal", Category: CategorySession,
			Source: "builtin",
		},
		{
			Name: "mcp", Kind: KindControl, Op: OpMCP,
			Summary: "Reload MCP servers", Category: CategoryInfo,
			Source: "builtin",
		},
		{
			Name: "model", Kind: KindControl, Op: OpSetModel,
			Summary: "Change the session model", ArgHint: "<provider/model>",
			Args:     []ArgSpec{{Name: "model", Type: ArgString}},
			Category: CategoryModel, Source: "builtin",
		},
		{
			Name: "new", Aliases: []string{"clear"}, Kind: KindFrontend,
			Summary: "Start a new session", Category: CategoryFrontend,
			Destructive: true, Source: "builtin",
		},
		{
			Name: "processes", Kind: KindControl, Op: OpProcessList,
			Summary: "List managed processes", Category: CategoryInfo,
			Source: "builtin",
		},
		{
			Name: "queue", Kind: KindControl, Op: OpQueueList,
			Summary: "Show queued prompts", Category: CategorySession,
			Source: "builtin",
		},
		{
			Name: "queue-clear", Kind: KindControl, Op: OpQueueClear,
			Summary: "Drop every queued prompt", Category: CategorySession,
			Destructive: true, Source: "builtin",
		},
		{
			Name: "quit", Kind: KindFrontend,
			Summary: "Leave the session", Category: CategoryFrontend,
			Source: "builtin",
		},
		{
			Name: "resume", Kind: KindFrontend,
			Summary: "Talk to an existing session", ArgHint: "<session-id>",
			Args:     []ArgSpec{{Name: "session_id", Type: ArgString}},
			Category: CategoryFrontend, Source: "builtin",
		},
		{
			Name: "status", Kind: KindControl, Op: OpStatus,
			Summary: "Show the session state", Category: CategoryInfo,
			Source: "builtin",
		},
		{
			Name: "thinking", Kind: KindControl, Op: OpSetThinking,
			Summary: "Set the reasoning effort", ArgHint: "<effort>",
			Args:     []ArgSpec{{Name: "effort", Type: ArgString}},
			Category: CategoryModel, Source: "builtin",
		},
		{
			Name: "tier", Kind: KindControl, Op: OpSetServiceTier,
			Summary: "Set the provider service tier", ArgHint: "<tier>",
			Args:     []ArgSpec{{Name: "service_tier", Type: ArgString}},
			Category: CategoryModel, Source: "builtin",
		},
	}
}

// NewRegistry returns the builtin registry, sorted by name.
func NewRegistry() *Registry {
	specs := builtins()
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	r := &Registry{specs: specs, byName: make(map[string]*Spec, len(specs)*2)}
	for _, s := range specs {
		r.byName[s.Name] = s
		for _, a := range s.Aliases {
			r.byName[a] = s
		}
	}
	return r
}

// Lookup returns the Spec a name or alias resolves to.
func (r *Registry) Lookup(name string) (*Spec, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// All returns every Spec, sorted by name.
func (r *Registry) All() []*Spec {
	out := make([]*Spec, len(r.specs))
	copy(out, r.specs)
	return out
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./command/`
Expected: PASS, four tests.

- [ ] **Step 6: Check the startup budget**

The spec's §9 requires that building the registry reads no disk and adds
no `init()` side effect. Confirm both:

```bash
grep -n "func init(" command/*.go        # expect no output
grep -nE "os\.(Open|ReadFile|Stat)|filepath\.Walk" command/*.go   # expect no output
```

Expected: both greps silent. `NewRegistry` builds from the compiled-in
table only.

- [ ] **Step 7: Commit**

```bash
git add command/command.go command/registry.go command/command_test.go
git commit -m "feat(command): add the slash-command registry"
```

---

### Task 2: Strict parsing

**Files:**
- Create: `command/resolve.go`
- Test: `command/resolve_test.go`

**Interfaces:**
- Consumes: `Registry`, `Spec`, `ArgSpec`, `Kind`, `Op` from Task 1.
- Produces: `command.Resolution` (fields `Kind Kind`, `Spec *Spec`,
  `Op Op`, `Args map[string]any`, `Text string`),
  `func (*Registry) Resolve(line string) (Resolution, error)`,
  `var ErrNotCommand error`, `type UnknownCommandError struct{ Name string }`.

`Resolve` returns `ErrNotCommand` for ordinary text. `Resolution.Text`
then holds the exact text the caller must send, which is the line itself
or the unescaped form of a `//` escape.

- [ ] **Step 1: Write the failing test**

Create `command/resolve_test.go`:

```go
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
	_, err := NewRegistry().Resolve("/nope")
	_ = r
	if !errors.As(err, &unknown) {
		t.Fatalf("Resolve(\"/nope\") error = %v, want *UnknownCommandError", err)
	}
	if unknown.Name != "nope" {
		t.Errorf("UnknownCommandError.Name = %q, want %q", unknown.Name, "nope")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./command/ -run TestResolve`
Expected: FAIL — `Resolve` undefined.

- [ ] **Step 3: Write the parser**

Create `command/resolve.go`:

```go
package command

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNotCommand reports that a line is ordinary text. Resolution.Text
// holds what the caller must send.
var ErrNotCommand = errors.New("command: not a command")

// UnknownCommandError reports a /name with no entry. A caller must NOT
// send the line to the model as text.
type UnknownCommandError struct{ Name string }

func (e *UnknownCommandError) Error() string {
	return fmt.Sprintf("command: unknown command %q", e.Name)
}

// Resolution is what one input line resolves to.
type Resolution struct {
	Kind Kind
	Spec *Spec
	Op   Op
	Args map[string]any
	Text string
}

// Resolve parses one input line. It performs no I/O and calls no route.
//
// Four rules protect a destructive command from a bad parse:
//  1. surplus input is an error; Resolve never drops part of a line
//  2. a command is the whole input: one line, nothing after it
//  3. //name is the literal text /name
//  4. a line that parses only after dropping text is not a command
func (r *Registry) Resolve(line string) (Resolution, error) {
	// Rule 2: a multi-line message is text.
	if strings.ContainsAny(line, "\n\r") {
		return Resolution{Text: line}, ErrNotCommand
	}
	// Rule 3: // escapes to a literal leading slash.
	if strings.HasPrefix(line, "//") {
		return Resolution{Text: line[1:]}, ErrNotCommand
	}
	if !strings.HasPrefix(line, "/") {
		return Resolution{Text: line}, ErrNotCommand
	}
	name, rest, _ := strings.Cut(strings.TrimSpace(line[1:]), " ")
	if name == "" {
		return Resolution{Text: line}, ErrNotCommand
	}
	spec, ok := r.Lookup(name)
	if !ok {
		return Resolution{}, &UnknownCommandError{Name: name}
	}
	args, err := bindArgs(spec, strings.TrimSpace(rest))
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{Kind: spec.Kind, Spec: spec, Op: spec.Op, Args: args}, nil
}

// bindArgs binds the remainder of a line to a Spec's positional
// arguments. Rule 1 lives here: leftover input is an error.
func bindArgs(spec *Spec, rest string) (map[string]any, error) {
	args := map[string]any{}
	if len(spec.Args) == 0 {
		if rest != "" {
			return nil, fmt.Errorf("command: /%s takes no arguments, got %q", spec.Name, rest)
		}
		return args, nil
	}
	for i, a := range spec.Args {
		if a.Type == ArgRest {
			if rest == "" {
				if a.Optional {
					return args, nil
				}
				return nil, fmt.Errorf("command: /%s needs %s", spec.Name, a.Name)
			}
			args[a.Name] = rest
			return args, nil
		}
		var field string
		field, rest, _ = strings.Cut(rest, " ")
		rest = strings.TrimSpace(rest)
		if field == "" {
			if a.Optional {
				continue
			}
			return nil, fmt.Errorf("command: /%s needs %s", spec.Name, a.Name)
		}
		switch a.Type {
		case ArgInt:
			n, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf("command: /%s %s must be a number, got %q", spec.Name, a.Name, field)
			}
			args[a.Name] = n
		default:
			args[a.Name] = field
		}
		if i == len(spec.Args)-1 && rest != "" {
			return nil, fmt.Errorf("command: /%s takes %d argument(s), got extra %q", spec.Name, len(spec.Args), rest)
		}
	}
	return args, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./command/`
Expected: PASS, all tests including `TestResolveRejectsSurplusInput`.

- [ ] **Step 5: Red-verify rule 1 against its named mechanism**

Temporarily change `bindArgs` so the `len(spec.Args) == 0` branch returns
`args, nil` without checking `rest`. Run
`go test -race ./command/ -run TestResolveRejectsSurplusInput`. It MUST
fail. Restore the check and confirm it passes. This proves the test pins
the reported failure and not something adjacent.

- [ ] **Step 6: Commit**

```bash
git add command/resolve.go command/resolve_test.go
git commit -m "feat(command): parse a command line strictly"
```

---

### Task 3: `GET /commands`

**Files:**
- Create: `server/commands.go`
- Modify: `server/server.go` — one line in `routes()` beside
  `mux.HandleFunc("GET /health", s.handleHealth)` at line 1012
- Modify: `server/openapi.yaml`
- Test: `server/commands_test.go`

**Interfaces:**
- Consumes: `command.NewRegistry`, `command.Spec`, `command.Op`,
  `command.KindControl`, `command.KindFrontend` from Tasks 1 and 2;
  `writeJSON` from `server/server.go:1312`.
- Produces: `opRoutes map[command.Op]route` and
  `func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request)`.

- [ ] **Step 1: Write the failing test**

Create `server/commands_test.go`:

```go
package server

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/command"
)

type commandJSON struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Op       string `json:"op"`
	Summary  string `json:"summary"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Category string `json:"category"`
}

// TestCommandsListsEveryBuiltin pins that GET /commands is the whole
// registry, sorted, with no surplus entry. A frontend builds its menu
// from this response alone.
func TestCommandsListsEveryBuiltin(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("GET", "/commands", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /commands status %d: %s", resp.StatusCode, data)
	}
	var body struct {
		Commands []commandJSON `json:"commands"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]commandJSON{}
	for _, c := range body.Commands {
		got[c.Name] = c
	}
	for _, s := range command.NewRegistry().All() {
		if _, ok := got[s.Name]; !ok {
			t.Errorf("GET /commands missing %q", s.Name)
		}
		delete(got, s.Name)
	}
	for name := range got {
		t.Errorf("GET /commands has surplus entry %q", name)
	}
}

// TestCommandsRouteInvariant pins the spec's §6 contract in both
// directions: a control command carries an op, a method, and a path; a
// frontend command carries none of the three, which is how a client
// learns it owns the action itself.
func TestCommandsRouteInvariant(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	_, data := h.do("GET", "/commands", nil)
	var body struct {
		Commands []commandJSON `json:"commands"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, c := range body.Commands {
		switch c.Kind {
		case string(command.KindControl):
			if c.Op == "" || c.Method == "" || c.Path == "" {
				t.Errorf("control %q: op=%q method=%q path=%q, want all set", c.Name, c.Op, c.Method, c.Path)
			}
		case string(command.KindFrontend):
			if c.Op != "" || c.Method != "" || c.Path != "" {
				t.Errorf("frontend %q: op=%q method=%q path=%q, want all empty", c.Name, c.Op, c.Method, c.Path)
			}
		default:
			t.Errorf("%q has kind %q", c.Name, c.Kind)
		}
	}
}

// TestEveryControlOpHasARoute pins that the route map cannot drift from
// the registry: a new control Op with no route entry fails here rather
// than shipping a menu item that reaches nothing.
func TestEveryControlOpHasARoute(t *testing.T) {
	for _, s := range command.NewRegistry().All() {
		if s.Kind != command.KindControl {
			continue
		}
		if _, ok := opRoutes[s.Op]; !ok {
			t.Errorf("control command %q has Op %q with no route", s.Name, s.Op)
		}
	}
	registry := command.NewRegistry()
	for op := range opRoutes {
		found := false
		for _, s := range registry.All() {
			if s.Op == op {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("route map has Op %q with no command", op)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./server/ -run TestCommands`
Expected: FAIL — `opRoutes` undefined and `GET /commands` returns 404.

- [ ] **Step 3: Write the handler**

Create `server/commands.go`:

```go
package server

import (
	"net/http"

	"github.com/majorcontext/harness/command"
)

// route is the serve-mode answer to one Op. A route is this dispatcher's
// mapping, not the operation: cmd/harness maps the same Op to a method
// call. See docs/design/slash-commands.md §5.
type route struct {
	method string
	path   string
}

var opRoutes = map[command.Op]route{
	command.OpCompact:        {"POST", "/session/{id}/compact"},
	command.OpSetModel:       {"POST", "/session/{id}/model"},
	command.OpSetThinking:    {"POST", "/session/{id}/thinking"},
	command.OpSetServiceTier: {"POST", "/session/{id}/service-tier"},
	command.OpAbort:          {"POST", "/session/{id}/abort"},
	command.OpSetGoal:        {"POST", "/session/{id}/goal"},
	command.OpClearGoal:      {"DELETE", "/session/{id}/goal"},
	command.OpQueueList:      {"GET", "/session/{id}/queue"},
	command.OpQueueClear:     {"DELETE", "/session/{id}/queue"},
	command.OpStatus:         {"GET", "/session/{id}"},
	command.OpProcessList:    {"GET", "/process"},
	command.OpMCP:            {"POST", "/session/{id}/mcp"},
}

type commandArgJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

type commandEntryJSON struct {
	Name        string           `json:"name"`
	Aliases     []string         `json:"aliases,omitempty"`
	Kind        string           `json:"kind"`
	Op          string           `json:"op,omitempty"`
	Summary     string           `json:"summary"`
	ArgHint     string           `json:"arg_hint,omitempty"`
	Args        []commandArgJSON `json:"args,omitempty"`
	Category    string           `json:"category"`
	Destructive bool             `json:"destructive,omitempty"`
	Method      string           `json:"method,omitempty"`
	Path        string           `json:"path,omitempty"`
}

// handleCommands returns the resolved registry. A frontend renders its
// menu and its argument hints from this response alone.
func (s *Server) handleCommands(w http.ResponseWriter, _ *http.Request) {
	specs := command.NewRegistry().All()
	out := make([]commandEntryJSON, 0, len(specs))
	for _, spec := range specs {
		e := commandEntryJSON{
			Name:        spec.Name,
			Aliases:     spec.Aliases,
			Kind:        string(spec.Kind),
			Op:          string(spec.Op),
			Summary:     spec.Summary,
			ArgHint:     spec.ArgHint,
			Category:    string(spec.Category),
			Destructive: spec.Destructive,
		}
		for _, a := range spec.Args {
			e.Args = append(e.Args, commandArgJSON{Name: a.Name, Type: string(a.Type), Optional: a.Optional})
		}
		if rt, ok := opRoutes[spec.Op]; ok {
			e.Method, e.Path = rt.method, rt.path
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, struct {
		Commands []commandEntryJSON `json:"commands"`
	}{Commands: out})
}
```

- [ ] **Step 4: Register the route**

In `server/server.go`, in `routes()`, add one line directly after the
`GET /health` registration at line 1012:

```go
	mux.HandleFunc("GET /commands", s.auth(s.handleCommands))
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -race ./server/ -run TestCommands` then
`go test -race ./server/ -run TestEveryControlOpHasARoute`
Expected: PASS.

- [ ] **Step 6: Add the OpenAPI entry**

In `server/openapi.yaml`, add a `/commands` path beside the other `GET`
paths, matching the file's existing style:

```yaml
  /commands:
    get:
      operationId: commands
      summary: The slash-command registry.
      description: |
        Every command a human can type, with the argument shape each one
        takes. A control command carries `op`, `method`, and `path`. A
        frontend command carries none of the three: the client owns that
        action. See docs/design/slash-commands.md.
      responses:
        "200":
          description: The registry.
          content:
            application/json:
              schema:
                type: object
                properties:
                  commands:
                    type: array
                    items:
                      type: object
```

- [ ] **Step 7: Commit**

```bash
git add server/commands.go server/commands_test.go server/server.go server/openapi.yaml
git commit -m "feat(server): serve the command registry at GET /commands"
```

---

### Task 4: Run-mode dispatch

**Files:**
- Create: `cmd/harness/command.go`
- Test: `cmd/harness/command_test.go`

**Interfaces:**
- Consumes: `command.Resolution`, `command.Op`, `command.KindControl`,
  `command.KindFrontend` from Tasks 1 and 2; `engine.Session`'s existing
  methods `Compact(ctx, engine.CompactOptions)` (`engine/compact.go:331`),
  `SetModel(message.ModelRef)` (`engine/engine.go:1803`),
  `SetEffort(message.Effort)` (`engine/engine.go:1918`), and
  `SetServiceTier(string)` (`engine/engine.go:1949`).
- Produces: `runModeOps map[command.Op]bool` and
  `func dispatchCommand(ctx context.Context, s *engine.Session, res command.Resolution) error`.

The two dispatchers do NOT cover the same `Op` set. Run mode supports the
four operations the engine exports as a setter or a direct call. `/abort`
arbitrates a run slot, `Session.PursueGoal` is a long call and not a
setter, and the queue is server state — none has a run-mode shape, so each
reports "not available in this mode" with the reason.

- [ ] **Step 1: Write the failing test**

Create `cmd/harness/command_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/command"
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -race ./cmd/harness/ -run TestRunMode`
Expected: FAIL — `runModeOps` and `dispatchCommand` undefined.

- [ ] **Step 3: Write the dispatcher**

Create `cmd/harness/command.go`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/command"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// runModeOps declares which control Ops `harness run` performs. A false
// value is an explicit refusal with a reason, not an omission: the
// support matrix must stay total, so a new Op cannot go silently
// unhandled. See docs/design/slash-commands.md §5.
var runModeOps = map[command.Op]bool{
	command.OpCompact:        true,
	command.OpSetModel:       true,
	command.OpSetThinking:    true,
	command.OpSetServiceTier: true,
	command.OpAbort:          false,
	command.OpSetGoal:        false,
	command.OpClearGoal:      false,
	command.OpQueueList:      false,
	command.OpQueueClear:     false,
	command.OpStatus:         false,
	command.OpProcessList:    false,
	command.OpMCP:            false,
}

// dispatchCommand performs one resolved control command against the
// session a run holds. Session resolution is not a concern here: a run
// has exactly one session, and it is a root.
func dispatchCommand(ctx context.Context, s *engine.Session, res command.Resolution) error {
	if res.Kind == command.KindFrontend {
		return fmt.Errorf("/%s is a frontend command; harness run does not own the session pointer", res.Spec.Name)
	}
	if supported, known := runModeOps[res.Op]; !known || !supported {
		return fmt.Errorf("/%s is not available in this mode: harness run has no server to perform %q", res.Spec.Name, res.Op)
	}
	switch res.Op {
	case command.OpCompact:
		var opts engine.CompactOptions
		if n, ok := res.Args["keep_turns"].(int); ok {
			opts.KeepTurns = n
		}
		_, err := s.Compact(ctx, opts)
		return err
	case command.OpSetModel:
		ref, err := message.ParseModelRef(res.Args["model"].(string))
		if err != nil {
			return err
		}
		s.SetModel(ref)
		return nil
	case command.OpSetThinking:
		e, err := message.ParseEffort(res.Args["effort"].(string))
		if err != nil {
			return err
		}
		s.SetEffort(e)
		return nil
	case command.OpSetServiceTier:
		s.SetServiceTier(res.Args["service_tier"].(string))
		return nil
	}
	return fmt.Errorf("unhandled op %q", res.Op)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./cmd/harness/ -run 'TestRunMode|TestUnsupported|TestFrontend'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/harness/command.go cmd/harness/command_test.go
git commit -m "feat(harness): dispatch a control command in run mode"
```

---

### Task 5: Wire run mode and document

**Files:**
- Modify: `cmd/harness/main.go` — the `runCmd` prompt path near line 801
- Modify: `docs/README.md` — add the design row if absent
- Test: `cmd/harness/command_test.go` (extend)

**Interfaces:**
- Consumes: `dispatchCommand` and `runModeOps` from Task 4;
  `command.Registry.Resolve` and `command.ErrNotCommand` from Task 2.
- Produces: no new exported name. `runCmd` resolves `opts.prompt` before
  it calls `s.Prompt`.

- [ ] **Step 1: Write the failing test**

Append to `cmd/harness/command_test.go`:

```go
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
```

Add `"errors"` to that file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -race ./cmd/harness/ -run 'TestPromptText|TestUnknownCommand'`
Expected: FAIL — the file does not import `errors` yet.

- [ ] **Step 3: Wire the resolve into runCmd**

In `cmd/harness/main.go`, in the `else` branch that calls `s.Prompt` near
line 801, resolve first:

```go
		res, cerr := command.NewRegistry().Resolve(opts.prompt)
		switch {
		case cerr == nil:
			if derr := dispatchCommand(ctx, s, res); derr != nil {
				return derr
			}
			return nil
		case !errors.Is(cerr, command.ErrNotCommand):
			return cerr
		}
		sessMgr.ReportTurnStart(s)
		msg, promptErr := s.Prompt(ctx, res.Text)
```

`res.Text` replaces `opts.prompt` so a `//` escape sends its unescaped
form. Add `"github.com/majorcontext/harness/command"` to the import block
at `cmd/harness/main.go:40`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./cmd/harness/`
Expected: PASS.

- [ ] **Step 5: Verify by hand against a real session**

```bash
go build -o /tmp/harness ./cmd/harness
/tmp/harness run -p "hello"          # ordinary prompt still works
/tmp/harness run -p "/nope"          # exits non-zero, names the unknown command
/tmp/harness run -p "/clear is just an alias for new"   # exits non-zero, no command runs
```

Expected: the third command reports that `/clear` takes no arguments. It
must not start a turn and must not clear anything.

- [ ] **Step 6: Run the full gates**

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

Expected: `gofmt -l .` empty, `go vet` silent, every package ok.

- [ ] **Step 7: Commit**

```bash
git add cmd/harness/main.go cmd/harness/command_test.go docs/README.md
git commit -m "feat(harness): resolve a slash command before prompting"
```

---

## Self-review

Run after the last task, before requesting review.

**1. Spec coverage.** Walk `docs/design/slash-commands.md` section by
section and name the task that implements it:

| Spec section | Task |
|---|---|
| §1 three kinds | 1 |
| §2 registry, `Destructive` | 1 |
| §3 `Resolve`, four parse rules | 2 |
| §4 control table | 1, and the three deviations above |
| §5 two dispatchers, support matrix | 3, 4 |
| §6 `GET /commands` | 3 |
| §7 prompt commands | OUT of scope |
| §8 provenance | OUT of scope, needs §7 |
| §9 startup budget | 1, Step 6 |
| §12 testing | 1, 2, 3, 4 |

**2. Placeholder scan.** Search the plan for `TBD`, `TODO`, "similar to
Task", "handle edge cases", "add validation". There must be none.

**3. Type consistency.** Check that every name used in a later task is
defined in an earlier one: `command.Op` constants in Tasks 3 and 4 match
Task 1's list exactly; `Resolution` field names in Task 4 match Task 2's
struct; `opRoutes` in Task 3's test matches Task 3's declaration.

## Verification before completion

Follow superpowers:verification-before-completion. Claim nothing without
the output. The gates are `gofmt -l .`, `go vet ./...`, and
`go test -race ./...`, plus the by-hand check in Task 5 Step 5.
