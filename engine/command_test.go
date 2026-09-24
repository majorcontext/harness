package engine

import (
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestCommandRecordNeverEntersHistory: a command's accepted-then-succeeded
// trail folds into Commands() and never appends to s.history, live or after
// LoadSession. Failure: log replay feeds the record to a model.
func TestCommandRecordNeverEntersHistory(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	before := len(s.History())

	id := NewCommandID()
	accepted := message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}
	if err := s.RecordCommand(accepted); err != nil {
		t.Fatalf("RecordCommand accepted: %v", err)
	}
	succeeded := accepted
	succeeded.Status = message.CommandSucceeded
	succeeded.Text = "/compact succeeded"
	if err := s.RecordCommand(succeeded); err != nil {
		t.Fatalf("RecordCommand succeeded: %v", err)
	}

	if got := len(s.History()); got != before {
		t.Fatalf("History() length = %d, want unchanged %d", got, before)
	}
	cmds := s.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandSucceeded {
		t.Fatalf("Commands() = %+v, want one succeeded command", cmds)
	}

	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if got := len(loaded.History()); got != before {
		t.Fatalf("after LoadSession: History() length = %d, want unchanged %d", got, before)
	}
	cmds = loaded.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandSucceeded {
		t.Fatalf("after LoadSession: Commands() = %+v, want one succeeded command", cmds)
	}
}

// TestRecordCommandDurableDedupesSeqAcrossRestart: a duplicate seq is a
// no-op live and across a restart, and shares the durable-enqueue watermark
// with EnqueuePromptDurable. Failure: a retried /enqueue re-runs a command
// after a restart.
func TestRecordCommandDurableDedupesSeqAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})

	c := message.CommandRecord{
		ID: NewCommandID(), Line: "/compact 5", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}
	if dup, err := s.RecordCommandDurable(c, 5); err != nil || dup {
		t.Fatalf("first RecordCommandDurable: dup=%v err=%v", dup, err)
	}
	if dup, err := s.RecordCommandDurable(c, 5); err != nil || !dup {
		t.Fatalf("second RecordCommandDurable (same seq): dup=%v err=%v, want dup=true", dup, err)
	}

	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if dup, err := loaded.RecordCommandDurable(c, 5); err != nil || !dup {
		t.Fatalf("after LoadSession, RecordCommandDurable(seq 5): dup=%v err=%v, want dup=true", dup, err)
	}
	if got := loaded.EnqueueSeq(); got != 5 {
		t.Fatalf("EnqueueSeq() = %d, want 5", got)
	}
	if _, dup, err := loaded.EnqueuePromptDurable("retry", 5, PromptProvenance{}); err != nil || !dup {
		t.Fatalf("EnqueuePromptDurable(seq 5): dup=%v err=%v, want dup=true (shared watermark)", dup, err)
	}
}

// TestCommandFoldTornSeqLastWriterWins: two hand-written command records
// sharing one seq but carrying different IDs fold to the SECOND one — the
// same torn-write last-writer-wins rule promptQueueFold.queued applies to
// the prompt queue.
func TestCommandFoldTornSeqLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000001"
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"command","command":{"id":"cmd_first","line":"/compact","name":"compact","source":"typed","status":"accepted","created_at":"2026-07-21T00:00:01Z","updated_at":"2026-07-21T00:00:01Z","seq":3}}`,
		`{"type":"command","command":{"id":"cmd_second","line":"/compact","name":"compact","source":"typed","status":"accepted","created_at":"2026-07-21T00:00:02Z","updated_at":"2026-07-21T00:00:02Z","seq":3}}`,
	)
	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatal(err)
	}
	cmds := s.Commands()
	if len(cmds) != 1 || cmds[0].ID != "cmd_second" {
		t.Fatalf("Commands() = %+v, want exactly one entry, ID cmd_second", cmds)
	}
}

// TestRepairInterruptedCommands: an accepted command left over a crash
// repairs to interrupted exactly once, and a second repair pass finds
// nothing left to do. Failure: a crash leaves a command permanently
// accepted, or repair repeats.
func TestRepairInterruptedCommands(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	id := NewCommandID()
	if err := s.RecordCommand(message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	text := func(name string) string {
		return "harness restarted before /" + name + " finished; it will not run again"
	}
	n, err := loaded.RepairInterruptedCommands(text)
	if err != nil {
		t.Fatalf("RepairInterruptedCommands: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepairInterruptedCommands count = %d, want 1", n)
	}
	cmds := loaded.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandInterrupted {
		t.Fatalf("Commands() = %+v, want one interrupted command", cmds)
	}

	loaded2, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatalf("second LoadSession: %v", err)
	}
	cmds = loaded2.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandInterrupted {
		t.Fatalf("after second LoadSession: Commands() = %+v, want one interrupted command", cmds)
	}
	n, err = loaded2.RepairInterruptedCommands(text)
	if err != nil {
		t.Fatalf("second RepairInterruptedCommands: %v", err)
	}
	if n != 0 {
		t.Fatalf("second RepairInterruptedCommands count = %d, want 0", n)
	}
}

// TestCommandAnchorSkipsSyntheticOrphan: AfterMessageID names the last
// history message that is not a synthetic orphan tool result.
func TestCommandAnchorSkipsSyntheticOrphan(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	s.mu.Lock()
	s.history = []message.Message{
		{ID: "msg_durable", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
		{ID: message.SyntheticOrphanIDPrefix + "abc", Role: message.RoleTool},
	}
	s.mu.Unlock()

	if err := s.RecordCommand(message.CommandRecord{
		ID: NewCommandID(), Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}
	cmds := s.Commands()
	if len(cmds) != 1 || cmds[0].AfterMessageID != "msg_durable" {
		t.Fatalf("Commands() = %+v, want AfterMessageID = msg_durable", cmds)
	}
}

// TestCommandSurvivesSnapshotAnchoredLoad: a snapshot taken after the
// command records restores Commands() and the durable-enqueue watermark on
// a snapshot-anchored LoadSession.
func TestCommandSurvivesSnapshotAnchoredLoad(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, SnapshotEveryRecords: idleOnly})

	id := NewCommandID()
	accepted := message.CommandRecord{
		ID: id, Line: "/compact 5", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}
	if dup, err := s.RecordCommandDurable(accepted, 4); err != nil || dup {
		t.Fatalf("RecordCommandDurable: dup=%v err=%v", dup, err)
	}
	succeeded := accepted
	succeeded.Status = message.CommandSucceeded
	succeeded.Text = "/compact succeeded"
	if err := s.RecordCommand(succeeded); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	s.snapshotOnIdle()
	s.waitSnapshots()

	loaded, err := LoadSession(Config{SessionDir: dir, SnapshotEveryRecords: idleOnly}, s.ID)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.replayedRecords >= loaded.recordsWritten {
		t.Fatalf("replayed %d of %d records — the snapshot was not used", loaded.replayedRecords, loaded.recordsWritten)
	}
	cmds := loaded.Commands()
	if len(cmds) != 1 || cmds[0].Status != message.CommandSucceeded {
		t.Fatalf("Commands() = %+v, want one succeeded command", cmds)
	}
	if got := loaded.EnqueueSeq(); got != 4 {
		t.Fatalf("EnqueueSeq() = %d, want 4", got)
	}
}

// TestJournalProjectsCommandMetadataOnly: LoadJournal shows command_id,
// command_name, and command_status, and no line or text.
func TestJournalProjectsCommandMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	id := NewCommandID()
	if err := s.RecordCommand(message.CommandRecord{
		ID: id, Line: "/compact 5", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandSucceeded, Text: "/compact succeeded",
	}); err != nil {
		t.Fatalf("RecordCommand: %v", err)
	}

	records, err := LoadJournal(dir, s.ID)
	if err != nil {
		t.Fatalf("LoadJournal: %v", err)
	}
	var found bool
	for _, r := range records {
		if r.Type != recCommand {
			continue
		}
		found = true
		if r.CommandID != id {
			t.Errorf("CommandID = %q, want %q", r.CommandID, id)
		}
		if r.CommandName != "compact" {
			t.Errorf("CommandName = %q, want compact", r.CommandName)
		}
		if r.CommandStatus != string(message.CommandSucceeded) {
			t.Errorf("CommandStatus = %q, want %q", r.CommandStatus, message.CommandSucceeded)
		}
	}
	if !found {
		t.Fatal("no command record found in journal")
	}

	data, err := os.ReadFile(sessionPath(dir, s.ID))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"line":"/compact 5"`) || !strings.Contains(log, `"text":"/compact succeeded"`) {
		t.Fatalf("the raw journal should still carry line/text (only the projection excludes it): %s", log)
	}
}
