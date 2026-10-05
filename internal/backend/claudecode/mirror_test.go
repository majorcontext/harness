package claudecode_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

func TestClaudeCodeContinuesATurnThatTheCLITook(t *testing.T) {
	for _, tc := range []struct {
		name, mode, next string
		hangAfter, seen  string
		want             string
	}{
		{name: "a handoff after init", mode: "hang", next: "thinking", seen: "backend.state", want: continuation},
		{name: "a mirrored handoff after a transcript", mode: "mirror", next: "mirror", hangAfter: "3", seen: "item.completed",
			want: continuation},
		{name: "a mirrored handoff before a transcript", mode: "mirror", next: "mirror", hangAfter: "0", seen: "backend.state", want: "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t, tc.mode, "FAKE_CLAUDE_MIRROR_FIXTURE", fixtures+"run1.stdout.jsonl", "FAKE_CLAUDE_MIRROR_HANG_AFTER", tc.hangAfter)
			st, mirror := harness.NewMemStore(), tc.mode == "mirror"
			if tc.next != "" {
				handOff(t, claudeRuntime(t, st, nil, mirror), tc.seen)
				t.Setenv("FAKE_CLAUDE_MODE", tc.next)
				t.Setenv("FAKE_CLAUDE_MIRROR_HANG_AFTER", "")
			}
			r := retryingRuntime(t, st, nil, mirror, 0, nil)
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

// recordedUsage returns the usage that the context.measured records of session s1 hold.
func recordedUsage(t *testing.T, st harness.Store) eventlog.Usage {
	t.Helper()
	recs, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var total eventlog.Usage
	for _, r := range recs {
		var env struct {
			K string
			D eventlog.ContextMeasured
		}
		if json.Unmarshal(r.Data, &env) == nil && env.K == "context.measured" {
			total = total.Add(env.D.Usage)
		}
	}
	return total
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

// toolPartNames returns the names of the tool call and tool result parts of s1.
func toolPartNames(t *testing.T, st harness.Store) []string {
	t.Helper()
	recs, err := st.Read(bg, "s1", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range recs {
		var env struct {
			D struct{ Message eventlog.Message }
		}
		_ = json.Unmarshal(r.Data, &env)
		for _, p := range env.D.Message.Parts {
			if p.CallID != "" {
				names = append(names, p.Name)
			}
		}
	}
	return names
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
		"owner.acquired 1", "item.completed assistant [harness: this turn was interrupted by a process restart and could not complete]", "turn.ended interrupted crashed",
		"input.admitted b", "turn.started b", "item.completed assistant Let me reason about this.", "item.completed assistant Here is my answer.", "context.measured", "turn.ended completed")
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
