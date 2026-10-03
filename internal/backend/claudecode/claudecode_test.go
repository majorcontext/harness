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

func claudeRuntime(t *testing.T, st harness.Store, owner harness.Owner, mirror bool) *harness.Runtime {
	t.Helper()
	return retryingRuntime(t, st, owner, mirror, 0)
}

func retryingRuntime(t *testing.T, st harness.Store, owner harness.Owner, mirror bool, retries int, tools ...harness.Tool) *harness.Runtime {
	t.Helper()
	bin, err := fakeClaudeBin()
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.New(harness.Options{Store: st, Owner: owner, Tools: tools, Config: config.Config{PromptRetries: &retries,
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: bin, SessionMirror: mirror}}}})
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

func TestClaudeCodeTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		env     []string
		allowed []string
		want    []string
		args    []string
	}{
		{name: "a text turn records the assistant items", mode: "thinking",
			want: []string{"backend.state", "item.completed assistant Let me reason about this.", "item.completed assistant Here is my answer.", "turn.ended completed"}},
		{name: "a tool use inside Claude Code appears as items", mode: "",
			want: []string{"backend.state", "item.completed assistant Let me check that.", "item.completed assistant toolu_1",
				"item.completed tool toolu_1 hi", "item.completed assistant Done — it printed hi.", "turn.ended completed"}},
		{name: "a placeholder result of a queued notification does not end the turn", mode: "queued_empty_result",
			want: []string{"backend.state", "item.completed assistant second", "turn.ended completed"}},
		{name: "a compaction result with no local command ends the turn", mode: "compact_turn", env: []string{"FAKECLAUDE_COMPACT_LOCAL_COMMAND", ""},
			want: []string{"backend.state", "compaction.applied", "turn.ended completed"}},
		{name: "a failed result fails the turn", mode: "error",
			want: []string{"backend.state", "backend.state", "turn.ended failed turn: retryable backend error: claudecode: the turn failed (error_during_execution): fake failure"}},
		{name: "a compaction by Claude Code is logged", mode: "compact_boundary",
			want: []string{"backend.state", "compaction.applied", "item.completed assistant Continuing after compaction.", "turn.ended completed"}},
		{name: "the context reading is logged", mode: "per_call_usage",
			want: []string{"backend.state", "item.completed assistant toolu_1", "item.completed tool toolu_1 ok", "item.completed assistant toolu_2",
				"item.completed tool toolu_2 ok", "item.completed assistant done", "context.measured", "turn.ended completed"}},
		{name: "a restriction maps to the tool list", mode: "thinking", env: []string{toolsInit, `["Bash","Read"]`}, allowed: []string{"Read", "Bash"},
			want: []string{"backend.state", "item.completed assistant Let me reason about this.", "item.completed assistant Here is my answer.", "turn.ended completed"},
			args: []string{"--tools", "Bash,Read", "--strict-mcp-config"}},
		{name: "an empty restriction disables every built-in tool", mode: "thinking", env: []string{toolsInit, `[]`}, allowed: []string{},
			want: []string{"backend.state", "item.completed assistant Let me reason about this.", "item.completed assistant Here is my answer.", "turn.ended completed"},
			args: []string{"--tools", "", "--strict-mcp-config"}},
		{name: "a restricted CLI with no init frame fails the turn", mode: "no_init", allowed: []string{"Bash"},
			want: []string{"turn.ended failed claudecode: the CLI did not apply the tool restriction: assistant frame before init"}},
		{name: "a CLI that ignores the restriction fails the turn", mode: "thinking", env: []string{toolsInit, `["Bash","Write"]`}, allowed: []string{"Bash"},
			want: []string{"turn.ended failed claudecode: the CLI did not apply the tool restriction: Write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvLog := fakeClaude(t, tc.mode, tc.env...)
			st := harness.NewMemStore()
			r := claudeRuntime(t, st, nil, false)
			defer closeRuntime(t, r)
			s := createClaude(t, r, tc.allowed)
			turnOf(t, s, text("a", "hi"))
			wantLog(t, st, 2, append([]string{"input.admitted a", "turn.started a"}, tc.want...)...)
			argv := jsonLines[[]string](t, argvLog)
			if len(argv) != 1 {
				t.Fatalf("CLI runs = %d, want 1", len(argv))
			}
			if tc.args != nil && !hasArgs(argv[0], tc.args...) {
				t.Errorf("argv = %q, want %q in it", argv[0], tc.args)
			}
			if tc.allowed == nil && slices.Contains(argv[0], "--tools") {
				t.Errorf("argv = %q, want no --tools", argv[0])
			}
		})
	}
}

func TestClaudeCodeCreateRefusesAToolThatIsNotBuiltIn(t *testing.T) {
	for _, name := range []string{"bash", "lookup"} {
		t.Run(name, func(t *testing.T) {
			r := retryingRuntime(t, harness.NewMemStore(), nil, false, 0, lookup{})
			defer closeRuntime(t, r)
			_, err := r.Create(bg, protocol.CreateSession{Model: "claude-code/sonnet", AllowedTools: []string{"Read", name}})
			if !errors.Is(err, harness.ErrInvalidRequest) {
				t.Errorf("Create = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// lookup is an embedder tool, which the CLI cannot run.
type lookup struct{}

func (lookup) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: "lookup"} }

func (lookup) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

func TestClaudeCodeSteerReachesStdin(t *testing.T) {
	fakeClaude(t, "steer")
	st := harness.NewMemStore()
	r := claudeRuntime(t, st, nil, false)
	defer closeRuntime(t, r)
	s := createClaude(t, r, nil)
	if _, err := s.Submit(bg, text("a", "run")); err != nil {
		t.Fatal(err)
	}
	seq := await(t, s, 0, "item.completed")
	steer := text("b", "left")
	steer.Delivery = protocol.DeliverySteer
	if _, err := s.Submit(bg, steer); err != nil {
		t.Fatal(err)
	}
	await(t, s, seq, "turn.ended")
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant toolu_s",
		"input.admitted b", "input.promoted b", "item.completed tool toolu_s slept", "item.completed assistant steered: left", "turn.ended completed")
	stdin := jsonLines[struct{ Message struct{ Content string } }](t, os.Getenv("FAKE_CLAUDE_STDIN_LOG"))
	if len(stdin) != 2 || stdin[1].Message.Content != "left" {
		t.Errorf("stdin lines = %+v, want the prompt, then the steer input", stdin)
	}
}

func TestClaudeCodeInterruptStopsTheCLI(t *testing.T) {
	const stopped = "turn.ended interrupted stopped"
	for _, tc := range []struct {
		mode string
		want []string
	}{
		{"hang_after_text", []string{"item.completed assistant Working on it.", stopped}},
		{"hang_in_tool", []string{"item.completed assistant Checking. toolu_h", "item.completed tool toolu_h " + interrupted, stopped}},
		{"tool_on_interrupt", []string{"item.completed assistant toolu_i", "item.completed tool toolu_i " + interrupted, stopped}},
		{"tool_result_on_interrupt", []string{"item.completed assistant toolu_i", "item.completed tool toolu_i ok", stopped}},
		{"success_on_interrupt", []string{"item.completed assistant Finished anyway.", "turn.ended completed"}},
		{"placeholder_on_interrupt", []string{stopped}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			signals := filepath.Join(t.TempDir(), "signals")
			fakeClaude(t, tc.mode, "FAKE_CLAUDE_SIGNAL_LOG", signals)
			st := harness.NewMemStore()
			r := claudeRuntime(t, st, nil, false)
			defer closeRuntime(t, r)
			s := createClaude(t, r, nil)
			if _, err := s.Submit(bg, text("a", "hi")); err != nil {
				t.Fatal(err)
			}
			await(t, s, 0, "backend.state")
			if err := s.Interrupt(bg, protocol.Interrupt{}); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"input.admitted a", "turn.started a", "backend.state"}, tc.want...)
			wantLog(t, st, 2, want...)
			if got, _ := os.ReadFile(signals); string(got) != "interrupt\n" {
				t.Errorf("signals = %q, want one SIGINT", got)
			}
			if u := endedUsage(t, st); u.InputTokens != 7 || u.OutputTokens != 2 {
				t.Errorf("turn usage = %+v, want the usage of the result after the SIGINT", u)
			}
		})
	}
}

const (
	continuation = "The previous turn was interrupted. Continue the unfinished work from the saved conversation. " +
		"Check the current state before repeating actions that may already have completed."
	interrupted = "interrupted before a result was recorded; check whether it took effect before running it again"
	cutOff      = "cut off before a result was recorded; check whether it took effect before running it again"
)

func TestClaudeCodeHandoffInterruptsTheCLI(t *testing.T) {
	signals := filepath.Join(t.TempDir(), "signals")
	fakeClaude(t, "tool_on_interrupt", "FAKE_CLAUDE_SIGNAL_LOG", signals)
	st := harness.NewMemStore()
	handOff(t, claudeRuntime(t, st, nil, false), "backend.state")
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant toolu_i",
		"backend.state", "item.completed tool toolu_i "+cutOff, "turn.suspended handoff")
	if got, _ := os.ReadFile(signals); string(got) != "interrupt\n" {
		t.Errorf("signals = %q, want one SIGINT", got)
	}
}

// handOff starts a turn on r, waits for the first event of kind, and closes r.
func handOff(t *testing.T, r *harness.Runtime, kind string) {
	t.Helper()
	s := createClaude(t, r, nil)
	if _, err := s.Submit(bg, text("a", "hi")); err != nil {
		t.Fatal(err)
	}
	await(t, s, 0, kind)
	closeRuntime(t, r)
}

func TestClaudeCodeContinuesATurnThatTheCLITook(t *testing.T) {
	for _, tc := range []struct {
		name, mode, next string
		hangAfter, seen  string
		retries          int
		want             string
	}{
		{name: "a handoff after init", mode: "hang", next: "thinking", seen: "backend.state", want: continuation},
		{name: "a mirrored handoff after a transcript", mode: "mirror", next: "mirror", hangAfter: "3", seen: "item.completed",
			want: continuation},
		{name: "a mirrored handoff before a transcript", mode: "mirror", next: "mirror", hangAfter: "0", seen: "backend.state", want: "hi"},
		{name: "a retry after init", mode: "crash", retries: 1, want: continuation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t, tc.mode, "FAKE_CLAUDE_MIRROR_FIXTURE", fixtures+"run1.stdout.jsonl", "FAKE_CLAUDE_MIRROR_HANG_AFTER", tc.hangAfter)
			st, mirror := harness.NewMemStore(), tc.mode == "mirror"
			if tc.next != "" {
				handOff(t, claudeRuntime(t, st, nil, mirror), tc.seen)
				t.Setenv("FAKE_CLAUDE_MODE", tc.next)
				t.Setenv("FAKE_CLAUDE_MIRROR_HANG_AFTER", "")
			}
			r := retryingRuntime(t, st, nil, mirror, tc.retries)
			defer closeRuntime(t, r)
			if tc.next == "" {
				turnOf(t, createClaude(t, r, nil), text("a", "hi"))
			} else if s, err := r.Open(bg, "s1"); err != nil {
				t.Fatal(err)
			} else {
				await(t, s, 0, "turn.ended")
			}
			stdin := jsonLines[struct{ Message struct{ Content string } }](t, os.Getenv("FAKE_CLAUDE_STDIN_LOG"))
			if len(stdin) != 2 || stdin[0].Message.Content != "hi" || stdin[1].Message.Content != tc.want {
				t.Errorf("stdin lines = %+v, want the prompt, then %q", stdin, tc.want)
			}
		})
	}
}

// endedUsage returns the usage of the last turn.ended record of session s1.
func endedUsage(t *testing.T, st harness.Store) eventlog.Usage {
	t.Helper()
	recs, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var ended eventlog.TurnEnded
	for _, r := range recs {
		var env struct {
			K string
			D json.RawMessage
		}
		if json.Unmarshal(r.Data, &env) == nil && env.K == "turn.ended" {
			_ = json.Unmarshal(env.D, &ended)
		}
	}
	return ended.Usage
}

// mirrorSeen is what the mirror mode found in its config dir at start.
type mirrorSeen struct {
	ConfigDir string            `json:"config_dir"`
	Files     map[string]string `json:"files"`
}

const fixtures = "../../../harnesstest/fakeclaude/testdata/"

func TestClaudeCodeResumesTheMirroredSessionAfterAHandoff(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	argvLog := fakeClaude(t, "mirror", "FAKE_CLAUDE_MIRROR_FIXTURE", fixtures+"run1.stdout.jsonl", "FAKE_CLAUDE_MIRROR_SEEN", seen)
	st := harness.NewDiskStore(t.TempDir())
	r1 := claudeRuntime(t, st, nil, true)
	turnOf(t, createClaude(t, r1, nil), text("a", "reply with ok"))
	closeRuntime(t, r1)
	t.Setenv("FAKE_CLAUDE_MIRROR_FIXTURE", fixtures+"run2.stdout.jsonl")
	r2 := claudeRuntime(t, st, nil, true)
	defer closeRuntime(t, r2)
	s, err := r2.Open(bg, "s1")
	if err != nil {
		t.Fatal(err)
	}
	turnOf(t, s, text("b", "reply with ok again"))
	turnOf(t, s, text("c", "once more"))
	const id = "11111111-2222-4333-8444-555555555555"
	argv := jsonLines[[]string](t, argvLog)
	if len(argv) != 3 || hasArgs(argv[0], "--resume") || !hasArgs(argv[1], "--resume", id) || !hasArgs(argv[1], "--session-mirror") {
		t.Fatalf("argv = %q, want a new session, then --resume %s", argv, id)
	}
	runs := jsonLines[mirrorSeen](t, seen)
	transcript := "projects/-home-u-proj/" + id + ".jsonl"
	run1, run2 := fixtures+"run1.stdout.jsonl", fixtures+"run2.stdout.jsonl"
	if len(runs) != 3 || len(runs[0].Files) != 0 || runs[1].Files[transcript] != mirrored(t, run1) || runs[2].Files[transcript] != mirrored(t, run1, run2) {
		t.Errorf("restored transcripts have %d runs, want none, then run1, then run1 and run2", len(runs))
	}
	if keys := blobKeys(t, st); len(keys) != 2 {
		t.Errorf("state blob keys = %q, want one for each owner", keys)
	}
}

// blobKeys returns the distinct blob keys of the backend.state records of s1.
func blobKeys(t *testing.T, st harness.Store) []string {
	t.Helper()
	recs, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range recs {
		var env struct {
			K string
			D eventlog.BackendState
		}
		if json.Unmarshal(r.Data, &env) == nil && env.K == "backend.state" && !slices.Contains(keys, env.D.BlobKey) {
			keys = append(keys, env.D.BlobKey)
		}
	}
	return keys
}

// mirrored returns the transcript that the mirror frames of runs write.
func mirrored(t *testing.T, runs ...string) string {
	t.Helper()
	var sb strings.Builder
	for _, run := range runs {
		for _, f := range jsonLines[struct{ Entries []json.RawMessage }](t, run) {
			for _, e := range f.Entries {
				sb.Write(append(e, '\n'))
			}
		}
	}
	return sb.String()
}

func TestClaudeCodeCrashWaitsForInput(t *testing.T) {
	argvLog := fakeClaude(t, "hang_after_text")
	st := harness.NewMemStore()
	k := killable{make(chan struct{})}
	r1 := claudeRuntime(t, st, k, false)
	s1 := createClaude(t, r1, nil)
	if _, err := s1.Submit(bg, text("a", "hi")); err != nil {
		t.Fatal(err)
	}
	await(t, s1, 0, "item.completed")
	close(k.lost)
	closeRuntime(t, r1)
	r2 := claudeRuntime(t, st, nil, false)
	defer closeRuntime(t, r2)
	s2, err := r2.Open(bg, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if v := s2.View(); v.Status != protocol.StatusIdle || len(jsonLines[[]string](t, argvLog)) != 1 {
		t.Fatalf("View = %+v after %d CLI runs, want idle after one", v, len(jsonLines[[]string](t, argvLog)))
	}
	t.Setenv("FAKE_CLAUDE_MODE", "thinking")
	turnOf(t, s2, text("b", "again"))
	wantLog(t, st, 2, "input.admitted a", "turn.started a", "backend.state", "item.completed assistant Working on it.",
		"owner.acquired 1", "turn.ended interrupted crashed",
		"input.admitted b", "turn.started b", "item.completed assistant Let me reason about this.", "item.completed assistant Here is my answer.", "turn.ended completed")
	if argv := jsonLines[[]string](t, argvLog); !hasArgs(argv[1], "--resume", "fake-session-1") {
		t.Errorf("argv after the crash = %q, want --resume fake-session-1", argv[1])
	}
	if stdin := jsonLines[struct{ Message struct{ Content string } }](t, os.Getenv("FAKE_CLAUDE_STDIN_LOG")); stdin[1].Message.Content != "again" {
		t.Errorf("stdin after the crash = %+v, want the next input", stdin[1])
	}
}

func TestClaudeCodeMirrorCrashBeforeATranscriptStartsANewSession(t *testing.T) {
	argvLog := fakeClaude(t, "mirror", "FAKE_CLAUDE_MIRROR_FIXTURE", fixtures+"run1.stdout.jsonl", "FAKE_CLAUDE_MIRROR_HANG_AFTER", "1")
	st := harness.NewDiskStore(t.TempDir())
	k := killable{make(chan struct{})}
	r1 := claudeRuntime(t, st, k, true)
	s1 := createClaude(t, r1, nil)
	if _, err := s1.Submit(bg, text("a", "reply with ok")); err != nil {
		t.Fatal(err)
	}
	await(t, s1, 0, "backend.state")
	close(k.lost)
	closeRuntime(t, r1)
	t.Setenv("FAKE_CLAUDE_MIRROR_HANG_AFTER", "")
	r2 := claudeRuntime(t, st, nil, true)
	defer closeRuntime(t, r2)
	s2, err := r2.Open(bg, "s1")
	if err != nil {
		t.Fatal(err)
	}
	turnOf(t, s2, text("b", "again"))
	if argv := jsonLines[[]string](t, argvLog); len(argv) != 2 || hasArgs(argv[1], "--resume") {
		t.Errorf("argv = %q, want a new session: the saved state holds no transcript", argv)
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

// killable is an Owner whose one grant ends when the test closes lost.
type killable struct{ lost chan struct{} }

func (k killable) Acquire(context.Context, string) (harness.Ownership, error) { return k, nil }
func (k killable) Epoch() uint64                                              { return 1 }
func (k killable) Lost() <-chan struct{}                                      { return k.lost }
func (k killable) Release()                                                   {}

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
