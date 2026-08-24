package engine

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// writeRawJournal writes recs directly to dir/id.jsonl, one JSON object per
// line, bypassing Session entirely — this test constructs every record type
// by hand (white-box, same package) so LoadJournal's projection can be
// exercised for record shapes (goal.*, task.*, compact, toolresult.retained)
// that would otherwise need a much larger scripted-provider turn sequence to
// reach.
func writeRawJournal(t *testing.T, dir, id string, recs []record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(sessionPath(dir, id))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, rec := range recs {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLoadJournal_ProjectsAllRecordTypes covers every recXxx type
// LoadJournal's projectJournalRecord switch handles: the exact fields it
// carries, in order (Seq assigned by scanLog's own 1-based line number),
// and — the load-bearing assertion — that a raw-looking secret embedded in
// a goal.stalled Reason and a task.notify_queued FailReason is redacted by
// plugin.SanitizeSessionError before it ever leaves this function.
func TestLoadJournal_ProjectsAllRecordTypes(t *testing.T) {
	dir := t.TempDir()
	id := newID("ses")
	createdAt := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	recs := []record{
		{Type: recSession, ID: id, CreatedAt: createdAt, WorkDir: "/repo", ParentSession: "ses_parent", TaskParentID: "ses_taskparent", TaskAgentType: "reviewer", Model: message.ModelRef{Provider: "anthropic", Model: "m1"}, Effort: message.Effort("high")},
		{Type: recMessage, Message: &message.Message{ID: "msg_1", Role: message.RoleUser, CreatedAt: createdAt}},
		{Type: recMessage, Message: &message.Message{ID: "msg_2", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: lostToRestartText}}, CreatedAt: createdAt}},
		{Type: recModel, Model: message.ModelRef{Provider: "anthropic", Model: "m2"}},
		{Type: recEffort, Effort: message.Effort("low")},
		{Type: recGoalStalled, Goal: &goalRecord{
			Condition: "tests pass", Reason: "Authorization: Bearer sk-secret-token-value failed",
			Attempt: 2, Retryable: true, RetryableClass: "overloaded", Waiting: true,
		}},
		{Type: recPromptQueued, Prompt: &promptRecord{ID: 3, Text: "queued text", Seq: 7}},
		{Type: recTaskSpawned, TaskSpawn: &taskSpawnRecord{ChildID: "ses_child1", Agent: "reviewer"}},
		{Type: recTaskNotifyQueued, TaskNotify: &taskNotifyRecord{
			ChildID: "ses_child1", Agent: "reviewer", Status: StatusFailed,
			FailReason: "api_key=sk-should-be-redacted failed the call", Canceled: false,
		}},
		{Type: recChildTurnSettled},
		{Type: recTaskOutcomeCommitted, TaskNotify: &taskNotifyRecord{ChildID: "ses_child1", Agent: "reviewer", Status: StatusDone, Result: "ok"}},
		{Type: recCompact, CreatedAt: createdAt, Compact: &compactRecord{FirstID: "msg_1", LastID: "msg_2", TurnsFolded: 4, Summary: message.Message{ID: "msg_summary"}}},
		{Type: recToolResultRetained, ToolResult: &toolResultRecord{Handle: "trh_1", Tool: "bash", Bytes: 4096, Lines: 100}},
	}
	writeRawJournal(t, dir, id, recs)

	got, err := LoadJournal(dir, id)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(recs))
	}

	// Seq is the 1-based line number, ascending.
	for i, r := range got {
		if r.Seq != i+1 {
			t.Errorf("record %d: Seq = %d, want %d", i, r.Seq, i+1)
		}
	}

	session := got[0]
	if session.Type != recSession || session.WorkDir != "/repo" || session.ParentSession != "ses_parent" ||
		session.TaskParentID != "ses_taskparent" || session.TaskAgentType != "reviewer" ||
		session.Model.Model != "m1" || session.Effort != message.Effort("high") || !session.CreatedAt.Equal(createdAt) {
		t.Errorf("session header record = %+v", session)
	}

	userMsg := got[1]
	if userMsg.MessageID != "msg_1" || userMsg.MessageRole != string(message.RoleUser) || userMsg.RecoveryMarker {
		t.Errorf("user message record = %+v", userMsg)
	}

	recoveryMsg := got[2]
	if recoveryMsg.MessageID != "msg_2" || !recoveryMsg.RecoveryMarker {
		t.Errorf("recovery-marker message record = %+v, want RecoveryMarker=true", recoveryMsg)
	}

	modelRec := got[3]
	if modelRec.Type != recModel || modelRec.Model.Model != "m2" {
		t.Errorf("model record = %+v", modelRec)
	}

	effortRec := got[4]
	if effortRec.Type != recEffort || effortRec.Effort != message.Effort("low") {
		t.Errorf("effort record = %+v", effortRec)
	}

	goalRec := got[5]
	if goalRec.GoalCondition != "tests pass" || goalRec.GoalAttempt != 2 || !goalRec.GoalRetryable ||
		goalRec.GoalRetryableClass != "overloaded" || !goalRec.GoalWaiting {
		t.Errorf("goal.stalled record = %+v", goalRec)
	}
	if want := "Authorization: Bearer [REDACTED] failed"; goalRec.GoalReason != want {
		t.Errorf("goal.stalled GoalReason = %q, want %q (sanitized)", goalRec.GoalReason, want)
	}

	promptRec := got[6]
	if promptRec.PromptID != 3 || promptRec.PromptSeq != 7 {
		t.Errorf("prompt.queued record = %+v", promptRec)
	}

	spawnRec := got[7]
	if spawnRec.ChildID != "ses_child1" || spawnRec.Agent != "reviewer" {
		t.Errorf("task.spawned record = %+v", spawnRec)
	}

	notifyRec := got[8]
	if notifyRec.ChildID != "ses_child1" || notifyRec.TaskStatus != string(StatusFailed) || notifyRec.TaskCanceled {
		t.Errorf("task.notify_queued record = %+v", notifyRec)
	}
	if want := "api_key=[REDACTED] failed the call"; notifyRec.TaskFailReason != want {
		t.Errorf("task.notify_queued TaskFailReason = %q, want %q (sanitized)", notifyRec.TaskFailReason, want)
	}

	settledRec := got[9]
	if settledRec.Type != recChildTurnSettled {
		t.Errorf("child_turn.settled record = %+v", settledRec)
	}

	committedRec := got[10]
	if committedRec.ChildID != "ses_child1" || committedRec.TaskStatus != string(StatusDone) {
		t.Errorf("task.outcome_committed record = %+v", committedRec)
	}

	compactRec := got[11]
	if compactRec.CompactFirstID != "msg_1" || compactRec.CompactLastID != "msg_2" ||
		compactRec.CompactTurnsFolded != 4 || !compactRec.CreatedAt.Equal(createdAt) {
		t.Errorf("compact record = %+v", compactRec)
	}

	toolResultRec := got[12]
	if toolResultRec.ToolResultHandle != "trh_1" || toolResultRec.ToolResultTool != "bash" || toolResultRec.ToolResultBytes != 4096 {
		t.Errorf("toolresult.retained record = %+v", toolResultRec)
	}
}

// TestLoadJournal_TruncatedFinalLineTolerated mirrors LoadSession's own
// crash-mid-write tolerance (see scanLog): a corrupt/incomplete FINAL line
// is silently dropped, never an error.
func TestLoadJournal_TruncatedFinalLineTolerated(t *testing.T) {
	dir := t.TempDir()
	id := newID("ses")
	path := sessionPath(dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good, err := json.Marshal(record{Type: recModel, Model: message.ModelRef{Provider: "anthropic", Model: "m1"}})
	if err != nil {
		t.Fatal(err)
	}
	data := append(good, '\n')
	data = append(data, []byte(`{"type":"model","mod`)...) // torn
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadJournal(dir, id)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (torn final line dropped)", len(got))
	}
}

// TestLoadJournal_NeverPersisted_ReturnsNotExist proves an id with no log
// file on disk yet reports an fs.ErrNotExist-wrapping error — the signal
// server.handleJournal relies on to answer an empty page (200) rather than
// a hard error for a session that genuinely exists but has not persisted
// its first record.
func TestLoadJournal_NeverPersisted_ReturnsNotExist(t *testing.T) {
	dir := t.TempDir()
	id := newID("ses")

	_, err := LoadJournal(dir, id)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadJournal error = %v, want fs.ErrNotExist", err)
	}
}

// TestLoadJournal_InvalidSessionID proves a malformed id (e.g. a
// path-traversal shape) is rejected before ever touching disk, mirroring
// LoadSession's own defense-in-depth guard.
func TestLoadJournal_InvalidSessionID(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadJournal(dir, "../../etc/passwd")
	if !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("LoadJournal error = %v, want ErrInvalidSessionID", err)
	}
}

// TestLoadJournal_LiveSessionMatchesReplay proves LoadJournal reads back
// exactly the records a real Session.Prompt run persisted — the end-to-end
// path (as opposed to the hand-built records above), covering the session
// header + model + two message records an ordinary turn produces.
func TestLoadJournal_LiveSessionMatchesReplay(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "hi"}),
	}}
	cfg := persistCfg(dir, prov)
	s := NewSession(cfg)
	if _, err := s.Prompt(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	got, err := LoadJournal(dir, s.ID)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	// session header, model, user message, assistant message.
	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4: %+v", len(got), got)
	}
	if got[0].Type != recSession {
		t.Errorf("got[0].Type = %q, want %q", got[0].Type, recSession)
	}
	if got[1].Type != recModel {
		t.Errorf("got[1].Type = %q, want %q", got[1].Type, recModel)
	}
	if got[2].Type != recMessage || got[2].MessageRole != string(message.RoleUser) {
		t.Errorf("got[2] = %+v, want a user message", got[2])
	}
	if got[3].Type != recMessage || got[3].MessageRole != string(message.RoleAssistant) {
		t.Errorf("got[3] = %+v, want an assistant message", got[3])
	}
}
