package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

var (
	bg      = context.Background()
	fakeDir string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "harness-fakeclaude-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var fakeClaudeBin = sync.OnceValues(func() (string, error) {
	bin := filepath.Join(fakeDir, "fakeclaude")
	if out, err := exec.Command("go", "build", "-o", bin, "../../../harnesstest/fakeclaude").CombinedOutput(); err != nil {
		return "", &exec.Error{Name: string(out), Err: err}
	}
	return bin, nil
})

// fakeClaude runs mode in each later CLI run and returns the file that
// receives the argv of each run, one JSON array per line.
func fakeClaude(t *testing.T, mode string, env ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(dir, "argv"))
	t.Setenv("FAKE_CLAUDE_STDIN_LOG", filepath.Join(dir, "stdin"))
	for i := 0; i+1 < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	return filepath.Join(dir, "argv")
}

func claudeRuntime(t *testing.T, st harness.Store, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	r, err := harness.New(harness.Options{Store: st, Tools: tools, Config: config.Config{PromptRetries: &retries,
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin}}}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func createClaude(t *testing.T, r *harness.Runtime, allowed []string) *harness.Session {
	t.Helper()
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet", AllowedTools: allowed})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// await returns the seq of the first event of kind after seq.
func await(t *testing.T, s *harness.Session, after uint64, kind string) uint64 {
	t.Helper()
	for e, err := range s.Events(bg, after) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == kind {
			return e.Seq
		}
	}
	t.Fatalf("no %s event", kind)
	return 0
}

// turnOf submits in and waits for the turn that it starts to end.
func turnOf(t *testing.T, s *harness.Session, in protocol.Input) {
	t.Helper()
	after := s.View().HeadSeq
	if _, err := s.Submit(bg, in); err != nil {
		t.Fatal(err)
	}
	await(t, s, after, "turn.ended")
}

func jsonLines[T any](t *testing.T, path string) []T {
	t.Helper()
	data, _ := os.ReadFile(path)
	var out []T
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var v T
		if line != "" && json.Unmarshal([]byte(line), &v) != nil {
			t.Fatalf("%s: bad line %q", path, line)
		}
		if line != "" {
			out = append(out, v)
		}
	}
	return out
}

// hasArgs reports whether argv holds want as consecutive arguments.
func hasArgs(argv []string, want ...string) bool {
	for i := range argv {
		if slices.Equal(argv[i:min(i+len(want), len(argv))], want) {
			return true
		}
	}
	return false
}

const toolsInit = "FAKE_CLAUDE_INIT_TOOLS"

func TestClaudeCodeCreateRefusesAnUnknownTool(t *testing.T) {
	for _, name := range []string{"bash", "nope"} {
		t.Run(name, func(t *testing.T) {
			r := claudeRuntime(t, harness.NewMemStore(), lookup{})
			defer closeRuntime(t, r)
			_, err := r.Create(bg, protocol.CreateSession{Model: "claude-code/sonnet", AllowedTools: []string{"Read", name}})
			if !errors.Is(err, harness.ErrInvalidRequest) {
				t.Errorf("Create = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestClaudeCodeCreateRefusesAnEmbedderToolNamedLikeABuiltin(t *testing.T) {
	for _, allowed := range [][]string{nil, {"Read"}} {
		r := claudeRuntime(t, harness.NewMemStore(), newProbe("Read", false))
		defer closeRuntime(t, r)
		_, err := r.Create(bg, protocol.CreateSession{Model: "claude-code/sonnet", AllowedTools: allowed})
		if !errors.Is(err, harness.ErrInvalidRequest) {
			t.Errorf("Create with AllowedTools %v = %v, want ErrInvalidRequest", allowed, err)
		}
	}
}

func text(id, s string) protocol.Input {
	return protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: s}}}
}

func closeRuntime(t *testing.T, r *harness.Runtime) {
	t.Helper()
	if err := r.Close(bg); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// wantLog fails unless the records of session s1 after seq render as want.
func wantLog(t *testing.T, st harness.Store, after uint64, want ...string) {
	t.Helper()
	recs, err := st.Read(bg, "s1", after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		var env struct {
			K string
			D struct {
				Epoch      uint64
				InputID    string   `json:"input_id"`
				InputIDs   []string `json:"input_ids"`
				StopReason string   `json:"stop_reason"`
				Error      string
				Cause      string
				Message    eventlog.Message
			}
		}
		if err := json.Unmarshal(r.Data, &env); err != nil {
			t.Fatal(err)
		}
		d := env.D
		f := []string{env.K}
		for _, s := range append([]string{d.InputID, d.StopReason, d.Error, d.Cause, d.Message.Role}, d.InputIDs...) {
			if s != "" {
				f = append(f, s)
			}
		}
		if d.Epoch > 0 {
			f = append(f, fmt.Sprint(d.Epoch))
		}
		for _, part := range d.Message.Parts {
			f = append(f, strings.TrimSpace(part.CallID+" "+part.Text))
		}
		got = append(got, strings.Join(f, " "))
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("log after %d =\n%s\nwant\n%s", after, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestClaudeCodeStreamsDeltas(t *testing.T) {
	fakeClaude(t, "thinking_reserved_id")
	r := claudeRuntime(t, harness.NewMemStore())
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	var got []string
	ids := map[string]string{}
	for e, err := range s.Events(bg, s.View().HeadSeq-1) {
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			if _, err := s.Submit(bg, text("a", "hi")); err != nil {
				t.Fatal(err)
			}
		}
		var d struct {
			ItemID     string `json:"item_id"`
			Type, Text string
		}
		_ = json.Unmarshal(e.Data, &d)
		if d.ItemID != "" && ids[d.ItemID] == "" {
			ids[d.ItemID] = fmt.Sprintf("i%d", len(ids)+1)
		}
		got = append(got, strings.TrimSpace(fmt.Sprintf("%d %s %s %s %s", e.Seq, e.Kind, ids[d.ItemID], d.Type, d.Text)))
		if e.Kind == "turn.ended" {
			break
		}
	}
	want := []string{"2 owner.acquired", "3 input.admitted", "4 turn.started", "5 backend.state",
		"5 item.started i1", "5 item.delta i1 reasoning Reasoning under a reserved id.",
		"5 item.delta i1 text Answer under the same reserved id.", "6 item.completed i1", "7 context.measured", "8 turn.ended"}
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestClaudeCodeAppliesTheAllowedToolsOfASession(t *testing.T) {
	const answered = "item.completed assistant Here is my answer."
	for _, tc := range []struct {
		name, mode string
		env        []string
		allowed    []string
		want       []string
		args       []string
	}{
		{name: "a restriction maps to the tool list", mode: "thinking", env: []string{toolsInit, `["Bash","Read"]`}, allowed: []string{"Read", "Bash"},
			want: []string{"backend.state", "item.completed assistant Let me reason about this.", answered, "context.measured", "turn.ended completed"},
			args: []string{"--tools", "Bash,Read", "--strict-mcp-config"}},
		{name: "an empty restriction disables every built-in tool", mode: "thinking", env: []string{toolsInit, `[]`}, allowed: []string{},
			want: []string{"backend.state", "item.completed assistant Let me reason about this.", answered, "context.measured", "turn.ended completed"},
			args: []string{"--tools", "", "--strict-mcp-config"}},
		{name: "a restricted CLI with no init frame fails the turn", mode: "no_init", allowed: []string{"Bash"},
			want: []string{"context.measured", "turn.ended failed claudecode: the CLI did not apply the tool restriction: assistant frame before init"}},
		{name: "a CLI that ignores the restriction fails the turn", mode: "thinking", env: []string{toolsInit, `["Bash","Write"]`}, allowed: []string{"Bash"},
			want: []string{"context.measured", "turn.ended failed claudecode: the CLI did not apply the tool restriction: Write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvLog := fakeClaude(t, tc.mode, tc.env...)
			st := harness.NewMemStore()
			r := claudeRuntime(t, st)
			defer closeRuntime(t, r)
			turnOf(t, createClaude(t, r, tc.allowed), text("a", "hi"))
			wantLog(t, st, 2, append([]string{"input.admitted a", "turn.started a"}, tc.want...)...)
			argv := jsonLines[[]string](t, argvLog)
			if len(argv) != 1 {
				t.Fatalf("CLI runs = %d, want 1", len(argv))
			}
			if tc.args != nil && !hasArgs(argv[0], tc.args...) {
				t.Errorf("argv = %q, want %q in it", argv[0], tc.args)
			}
		})
	}
}
