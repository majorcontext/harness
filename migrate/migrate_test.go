package migrate

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/message"
)

// copyDir copies the recorded journals to a new directory, so a test can
// check that the migration leaves every old file as it was.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := os.CopyFS(dst, os.DirFS(src))
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func replay(t *testing.T, st harness.Store, id string) *eventlog.State {
	t.Helper()
	recs, err := st.Read(context.Background(), id, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &eventlog.State{}
	for _, r := range recs {
		if err := s.Apply(eventlog.Record{Seq: r.Seq, Data: r.Data}); err != nil {
			t.Fatalf("replay %s seq %d: %v", id, r.Seq, err)
		}
	}
	return s
}

// oldTranscript is the transcript that the engine loader shows, one line
// per message. A tool call without a result gets the result that the
// engine adds before each request.
func oldTranscript(t *testing.T, dir, id string) []string {
	t.Helper()
	s, err := engine.LoadSession(engine.Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range message.ResolveOrphanToolCalls(s.History()) {
		line := string(m.Role)
		for _, p := range m.Parts {
			switch p := p.(type) {
			case *message.Text:
				line += "|text:" + p.Text
			case *message.Blob:
				line += "|text:" + blobText(p)
			case *message.Reasoning:
				line += "|reasoning:" + p.Text
			case *message.ToolCall:
				line += "|call:" + p.CallID + ":" + p.Name
			case *message.ToolResult:
				line += "|result:" + p.CallID + ":" + p.SafeContent().Text()
			}
		}
		out = append(out, line)
	}
	return out
}

func newTranscript(s *eventlog.State) []string {
	var out []string
	for _, m := range s.History() {
		line := m.Role
		for _, p := range m.Parts {
			switch p.Type {
			case eventlog.PartToolCall:
				line += "|call:" + p.CallID + ":" + p.Name
			case eventlog.PartToolResult:
				line += "|result:" + p.CallID + ":" + p.Text
			default:
				line += "|" + p.Type + ":" + p.Text
			}
		}
		out = append(out, line)
	}
	return out
}

func readBlob(t *testing.T, st harness.Store, id, key string) string {
	t.Helper()
	rc, err := st.GetBlob(context.Background(), id, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var journalCases = []struct {
	name, id string
	check    func(t *testing.T, s *eventlog.State, st harness.Store)
}{
	{"tool loop with an attachment, a command, and a cut-off tool call", "ses_000000000000000a", func(t *testing.T, s *eventlog.State, st harness.Store) {
		if s.Model() != "anthropic/claude-sonnet-4-5" || s.Settings() != (eventlog.Settings{Effort: "high", ServiceTier: "priority"}) {
			t.Errorf("model %q settings %+v", s.Model(), s.Settings())
		}
		if u := s.Usage(); u != (eventlog.Usage{InputTokens: 450, OutputTokens: 35, CacheReadTokens: 7}) {
			t.Errorf("usage %+v", u)
		}
		if s.Status() != eventlog.StatusIdle || len(s.OpenToolCalls()) != 0 {
			t.Errorf("status %s, open calls %v", s.Status(), s.OpenToolCalls())
		}
		if p := s.History()[1].Parts[0]; p.ProviderData == nil {
			t.Errorf("reasoning lost its provider data: %+v", p)
		}
		if c, _, ok := s.Command("cmd_a1"); !ok || c.Status != "succeeded" || c.Line != "/effort high" || c.Text != "effort is high" || c.Args["level"] != "high" {
			t.Errorf("command %+v %v", c, ok)
		}
		if prev := before(t, st, "ses_000000000000000a", "command.recorded"); prev != "item.completed msg_a4" {
			t.Errorf("the command follows %q, want the message it followed", prev)
		}
	}},
	{"compaction, retained result, model change, goal, and queued prompt", "ses_000000000000000b", func(t *testing.T, s *eventlog.State, st harness.Store) {
		if c, ok := s.Compaction(); !ok || c.Summary != "Summary: the user read big.log (trh_1)." {
			t.Errorf("compaction %+v %v", c, ok)
		}
		if s.Model() != "anthropic/claude-opus-4-1" || s.Settings().Effort != "low" {
			t.Errorf("model %q settings %+v", s.Model(), s.Settings())
		}
		if g, ok := s.Goal(); !ok || g.State != eventlog.GoalActive || g.Condition != "all tests pass" {
			t.Errorf("goal %+v", g)
		}
		if q := s.Queue(); len(q) != 1 || q[0].Parts[0].Text != "and add docs" || q[0].Source != "api" {
			t.Errorf("queue %+v", q)
		}
		r := s.Retained()
		if len(r) != 1 || r[0].Handle != "trh_1" || r[0].Tool != "read" || r[0].Lines != 3 {
			t.Fatalf("retained %+v", r)
		}
		if got := readBlob(t, st, "ses_000000000000000b", r[0].BlobKey); got != "line one\nline two\nline three\n" {
			t.Errorf("retained blob %q", got)
		}
	}},
	{"parent keeps the report it did not get", "ses_000000000000000c", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if got := s.Children(); !slices.Equal(got, []string{"ses_000000000000000d", "ses_000000000000000e"}) {
			t.Errorf("children %v", got)
		}
		if got := s.Unsettled(); !slices.Equal(got, []string{"ses_000000000000000e"}) {
			t.Errorf("unsettled %v", got)
		}
	}},
	{"child keeps its parent, profile, and tools", "ses_000000000000000d", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if sum := s.Summary(); sum.ParentID != "ses_000000000000000c" || s.Agent() != "explore" || !slices.Equal(s.AllowedTools(), []string{"read", "grep"}) {
			t.Errorf("summary %+v agent %q tools %v", sum, s.Agent(), s.AllowedTools())
		}
	}},
	{"failed child ends failed", "ses_000000000000000e", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopFailed || e.Error != "tool crashed" {
			t.Errorf("last ended %+v", e)
		}
		if tools := s.AllowedTools(); tools == nil || len(tools) != 0 {
			t.Errorf("a child with no tools has tools %v", tools)
		}
	}},
	{"child in a turn at the cutover fails as lost to restart", "ses_0000000000000012", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopFailed || e.Error != lostToRestart {
			t.Errorf("last ended %+v", e)
		}
	}},
	{"child that answered but did not settle ends done", "ses_0000000000000013", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopCompleted || e.Error != "" {
			t.Errorf("last ended %+v", e)
		}
	}},
	{"delegated session resumes its CLI session", "ses_000000000000000f", func(t *testing.T, s *eventlog.State, st harness.Store) {
		key := s.BackendState(claudeCodeState)
		m, err := external.LoadMirror([]byte(readBlob(t, st, "ses_000000000000000f", key)))
		if err != nil || m.SessionID != "cli-123" {
			t.Errorf("mirror %+v %v", m, err)
		}
	}},
}

func TestDirConvertsEachJournal(t *testing.T) {
	ctx := context.Background()
	dir := copyDir(t, "testdata/journals")
	st := harness.NewDiskStore(t.TempDir())
	results, err := Dir(ctx, dir, st, message.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	failed := Failed(results)
	if len(results) != 10 || len(failed) != 1 || failed[0].Session != "ses_0000000000000010" {
		t.Fatalf("results %+v", results)
	}
	for _, c := range journalCases {
		t.Run(c.name, func(t *testing.T) {
			s := replay(t, st, c.id)
			want, got := oldTranscript(t, dir, c.id), newTranscript(s)
			if !slices.Equal(got, want) {
				t.Errorf("history\n got %q\nwant %q", got, want)
			}
			c.check(t, s, st)
		})
	}
	if head, err := st.Head(ctx, "ses_0000000000000010"); err != nil || head != 0 {
		t.Errorf("a failed session has head %d, %v", head, err)
	}
	if err := sameTree(dir, "testdata/journals"); err != nil {
		t.Error(err)
	}
	again, err := Dir(ctx, dir, st, message.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		if r.Session != "ses_0000000000000010" && (!r.Skipped || r.Err != nil) {
			t.Errorf("a second run did not skip %s: %+v", r.Session, r)
		}
	}
}

// sameTree reports a file of want that got lacks or holds different bytes.
func sameTree(got, want string) error {
	return filepath.WalkDir(want, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(want, p)
		a, err := os.ReadFile(filepath.Join(got, rel))
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return &os.PathError{Op: "changed", Path: rel, Err: os.ErrInvalid}
		}
		return nil
	})
}

// before returns the kind and item ID of the record before the first
// record of kind in the log of id.
func before(t *testing.T, st harness.Store, id, kind string) string {
	t.Helper()
	recs, err := st.Read(context.Background(), id, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	for _, r := range recs {
		e, err := eventlog.Decode(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		if e.Event.Kind() == kind {
			return prev
		}
		prev = e.Event.Kind()
		if item, ok := e.Event.(eventlog.ItemCompleted); ok {
			prev += " " + item.ItemID
		}
	}
	return ""
}

func TestDirGivesTheFallbackModelToAJournalThatNamesNone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journal := `{"type":"session","id":"ses_0000000000000014","created_at":"2026-09-08T07:00:00Z"}
{"type":"message","message":{"id":"msg_k1","role":"user","parts":[{"type":"text","text":"hi"}]}}
{"type":"message","message":{"id":"msg_k2","role":"assistant","parts":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, "ses_0000000000000014.jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	st := harness.NewMemStore()
	if results, err := Dir(ctx, dir, st, message.ModelRef{}); err != nil || len(Failed(results)) != 1 {
		t.Fatalf("with no fallback: results %+v, %v", results, err)
	}
	results, err := Dir(ctx, dir, st, message.ModelRef{Provider: "openai", Model: "gpt-5"})
	if err != nil || len(Failed(results)) != 0 {
		t.Fatalf("results %+v, %v", results, err)
	}
	s := replay(t, st, "ses_0000000000000014")
	if want := oldTranscript(t, dir, "ses_0000000000000014"); s.Model() != "openai/gpt-5" || !slices.Equal(newTranscript(s), want) {
		t.Errorf("model %q history %q, want %q", s.Model(), newTranscript(s), want)
	}
}
