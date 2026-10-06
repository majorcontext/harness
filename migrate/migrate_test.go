package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
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
				if len(p.Data) == 0 {
					line += "|text:" + blobText(p)
					break
				}
				sum := sha256.Sum256(p.Data)
				line += fmt.Sprintf("|blob:%s:%d:attachment-%x", p.MediaType, len(p.Data), sum)
			case *message.Reasoning:
				line += "|reasoning:" + p.Text
			case *message.ToolCall:
				line += "|call:" + p.CallID + ":" + p.Name + ":" + compactJSON(p.Arguments)
			case *message.ToolResult:
				line += "|result:" + p.CallID + ":" + p.SafeContent().Text()
			}
		}
		out = append(out, line)
	}
	return out
}

// compactJSON returns raw with no insignificant space, so the arguments of
// a call compare by value.
func compactJSON(raw []byte) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func newTranscript(s *eventlog.State) []string {
	var out []string
	for _, m := range s.History() {
		line := m.Role
		for _, p := range m.Parts {
			switch p.Type {
			case eventlog.PartToolCall:
				line += "|call:" + p.CallID + ":" + p.Name + ":" + compactJSON(p.Arguments)
			case eventlog.PartToolResult:
				line += "|result:" + p.CallID + ":" + p.Text
			case eventlog.PartBlob:
				line += fmt.Sprintf("|blob:%s:%d:%s", p.MediaType, p.Bytes, p.BlobKey)
			default:
				line += "|" + p.Type + ":" + p.Text
			}
		}
		out = append(out, line)
	}
	return out
}

func attachmentBytes(t *testing.T, b64 string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readBlob(t *testing.T, st harness.Store, id, key string) string {
	t.Helper()
	rc, err := st.GetBlob(context.Background(), id, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
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
		first := s.History()[0].Parts
		if len(first) != 3 || first[1].Type != eventlog.PartBlob || first[2].Type != eventlog.PartBlob {
			t.Fatalf("the first message holds %+v, want text, png, and pdf blob parts", first)
		}
		if got := readBlob(t, st, "ses_000000000000000a", first[1].BlobKey); got != attachmentBytes(t, "iVBORw0K") || first[1].MediaType != "image/png" {
			t.Errorf("png blob %q (%s)", got, first[1].MediaType)
		}
		if got := readBlob(t, st, "ses_000000000000000a", first[2].BlobKey); got != attachmentBytes(t, "JVBERi0xLjQKJSVFT0YK") || first[2].MediaType != "application/pdf" {
			t.Errorf("pdf blob %q (%s)", got, first[2].MediaType)
		}
		if again := s.History()[4].Parts[1]; again.BlobKey != first[1].BlobKey {
			t.Errorf("equal bytes have keys %q and %q, want one", again.BlobKey, first[1].BlobKey)
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
		if p := s.Queue()[0].Parts; len(p) != 2 || p[1].Type != eventlog.PartBlob || p[1].MediaType != "image/png" ||
			readBlob(t, st, "ses_000000000000000b", p[1].BlobKey) != attachmentBytes(t, "iVBORw0K") {
			t.Errorf("queued prompt parts %+v", p)
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
	{"child that a usage limit failed reports its reason once", "ses_0000000000000014", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopFailed || e.Cause != eventlog.CauseProviderExhausted || e.Error != "usage limit reached" || e.RecoverHint != "2026-09-03 09:00 UTC" {
			t.Errorf("last ended %+v", e)
		}
		const want = "ses_0000000000000014 (agent=explore) failed: provider capacity exhausted for this account: usage limit reached (usage: 9 in / 7 out)" + wallGuidanceAfter + "ses_0000000000000014"
		if _, report, ok := session.Settlement("ses_0000000000000014", s); !ok || report.Parts("")[1].Text != want {
			t.Errorf("report %+v, want line %q", report, want)
		}
	}},
	{"child that a rate limit wall failed reports its reason once", "ses_0000000000000015", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopFailed || e.Cause != eventlog.CauseProviderExhausted || e.ErrorClass != eventlog.ErrorRateLimited || e.Error != "too many requests" {
			t.Errorf("last ended %+v", e)
		}
		const want = "ses_0000000000000015 (agent=explore) failed: provider rate limit outlasted the retry budget for this account: too many requests (usage: 9 in / 7 out)" + wallGuidance + "ses_0000000000000015"
		if _, report, ok := session.Settlement("ses_0000000000000015", s); !ok || report.Parts("")[1].Text != want {
			t.Errorf("report %+v, want line %q", report, want)
		}
	}},
	{"child in a turn at the cutover fails as lost to restart", "ses_0000000000000012", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopFailed || e.Error != session.ReasonLostToRestart {
			t.Errorf("last ended %+v", e)
		}
	}},
	{"child that answered but did not settle ends done", "ses_0000000000000013", func(t *testing.T, s *eventlog.State, _ harness.Store) {
		if e := s.LastEnded(); e.StopReason != eventlog.StopCompleted || e.Error != "" {
			t.Errorf("last ended %+v", e)
		}
	}},
	{"delegated session resumes its CLI session and denies its parked question", "ses_000000000000000f", func(t *testing.T, s *eventlog.State, st harness.Store) {
		chain, _ := s.BackendState(claudeCodeState)
		head, _, _ := strings.Cut(readBlob(t, st, "ses_000000000000000f", chain.Legacy), "\n")
		m, err := external.MirrorOf(json.RawMessage(head), nil)
		if err != nil || m.SessionID != "cli-123" || m.Parked != "toolu_q1" {
			t.Errorf("mirror %+v %v", m, err)
		}
		if sub := s.SubscriptionUsage(); sub == nil || sub.SessionCostUSD == nil || *sub.SessionCostUSD != 0.25 {
			t.Errorf("subscription usage %+v, want a session cost of 0.25", sub)
		}
	}},
}

// wallGuidance is the guidance that ends the line of a child that a wall of the provider account stopped, up to its session ID.
const wallGuidance = " — provider exhausted, child preserved: do not spawn a replacement (every session on this provider account hits the same wall); resume this child with task send on session_id "

// wallGuidanceAfter is wallGuidance for a wall whose journal holds the time that it lifts.
const wallGuidanceAfter = " — provider exhausted, child preserved: do not spawn a replacement (every session on this provider account hits the same wall); resume this child after 2026-09-03 09:00 UTC with task send on session_id "

func TestDirConvertsEachJournal(t *testing.T) {
	ctx := context.Background()
	dir := copyDir(t, "testdata/journals")
	st := harness.NewDiskStore(t.TempDir())
	results, err := Dir(ctx, dir, st, message.ModelRef{})
	if err != nil {
		t.Fatal(err)
	}
	failed := Failed(results)
	if len(results) != 12 || len(failed) != 1 || failed[0].Session != "ses_0000000000000010" {
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

func TestConvertMessageKeepsTheSubagentParent(t *testing.T) {
	m := message.Message{Role: message.RoleAssistant, ParentToolUseID: "toolu_parent", Parts: message.Parts{&message.Text{Text: "inside"}}}
	if got := convertMessage(m, map[string]string{}, map[string][]byte{}); got.ParentCallID != "toolu_parent" {
		t.Errorf("converted ParentCallID = %q, want toolu_parent", got.ParentCallID)
	}
}

func TestDirKeepsThePromptProvenanceOfAMessageAndAQueuedPrompt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	journal := `{"type":"session","id":"ses_0000000000000016","model":"anthropic/claude-sonnet-4-5","created_at":"2026-09-08T07:00:00Z"}
{"type":"message","message":{"id":"msg_s1","role":"user","source":"slack","source_id":"slack:C1:1.2","source_label":"Ann (Slack)","parts":[{"type":"text","text":"hi"}]}}
{"type":"message","message":{"id":"msg_s2","role":"assistant","parts":[{"type":"text","text":"hello"}]}}
{"type":"prompt.queued","created_at":"2026-09-08T07:01:00Z","prompt":{"id":1,"text":"later","message_id":"msg_s3","source":"schedule","source_id":"cron-7","source_label":"Nightly"}}
`
	if err := os.WriteFile(filepath.Join(dir, "ses_0000000000000016.jsonl"), []byte(journal), 0o644); err != nil {
		t.Fatal(err)
	}
	st := harness.NewMemStore()
	if results, err := Dir(ctx, dir, st, message.ModelRef{}); err != nil || len(Failed(results)) != 0 {
		t.Fatalf("results %+v, %v", results, err)
	}
	s := replay(t, st, "ses_0000000000000016")
	sent, _, _ := s.Input("msg_s1")
	if sent.Source != "slack" || sent.SourceID != "slack:C1:1.2" || sent.SourceLabel != "Ann (Slack)" {
		t.Errorf("the message converts to %+v", sent)
	}
	if q := s.Queue(); len(q) != 1 || q[0].Source != "schedule" || q[0].SourceID != "cron-7" || q[0].SourceLabel != "Nightly" {
		t.Errorf("the queued prompt converts to %+v", q)
	}
}
