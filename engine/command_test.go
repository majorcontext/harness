package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

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
	if _, dup, err := loaded.EnqueuePromptDurable("retry", "", 5, PromptProvenance{}); err != nil || !dup {
		t.Fatalf("EnqueuePromptDurable(seq 5): dup=%v err=%v, want dup=true (shared watermark)", dup, err)
	}
}

func TestCommandFoldTornSeqLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	const id = "ses_0000000000000001"
	writeSessionLog(t, dir, id,
		`{"type":"session","id":"ses_0000000000000001","created_at":"2026-07-21T00:00:00Z"}`,
		`{"type":"command","command":{"id":"cmd_first","line":"/compact","name":"compact","source":"typed","status":"accepted","seq":3}}`,
		`{"type":"command","command":{"id":"cmd_second","line":"/compact","name":"compact","source":"typed","status":"accepted","seq":3}}`,
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

func TestRecordCommandTerminalInheritsClientRef(t *testing.T) {
	dir := t.TempDir()
	var events []Event
	s := NewSession(Config{SessionDir: dir, OnEvent: func(ev Event) { events = append(events, ev) }})
	id := NewCommandID()
	accepted := message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
		ClientRef: "pd_01abc",
	}
	if err := s.RecordCommand(accepted); err != nil {
		t.Fatal(err)
	}
	terminal := message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandSucceeded,
	}
	if err := s.RecordCommand(terminal); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Command == nil || events[1].Command.ClientRef != "pd_01abc" {
		t.Fatalf("terminal event = %+v, want ClientRef pd_01abc", events)
	}
	loaded, err := LoadSession(Config{SessionDir: dir}, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cmds := loaded.Commands()
	if len(cmds) != 1 || cmds[0].ClientRef != "pd_01abc" {
		t.Fatalf("after LoadSession: Commands() = %+v, want one record with ClientRef pd_01abc", cmds)
	}
}

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

	loaded, err := LoadSession(Config{
		SessionDir: dir,
		OnEvent:    func(Event) { panic("repair emitted an event") },
	}, s.ID)
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

func TestRecordCommandTerminalKeepsCreatedAtAndAnchor(t *testing.T) {
	dir := t.TempDir()
	var events []Event
	s := NewSession(Config{
		SessionDir: dir,
		OnEvent:    func(ev Event) { events = append(events, ev) },
	})
	s.mu.Lock()
	s.history = []message.Message{
		{ID: "msg_durable", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	}
	s.mu.Unlock()

	id := NewCommandID()
	accepted := message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandAccepted,
	}
	if err := s.RecordCommand(accepted); err != nil {
		t.Fatalf("RecordCommand accepted: %v", err)
	}
	if len(events) != 1 || events[0].Command == nil {
		t.Fatalf("events after accepted = %+v, want one command event", events)
	}
	wantCreatedAt := events[0].Command.CreatedAt
	wantAnchor := events[0].Command.AfterMessageID
	if wantCreatedAt.IsZero() {
		t.Fatal("accepted event's own CreatedAt is zero, cannot assert against it")
	}
	if wantAnchor != "msg_durable" {
		t.Fatalf("accepted event AfterMessageID = %q, want msg_durable", wantAnchor)
	}

	succeeded := message.CommandRecord{
		ID: id, Line: "/compact", Name: "compact",
		Source: message.PromptSourceTyped, Status: message.CommandSucceeded, Text: "/compact succeeded",
	}
	if err := s.RecordCommand(succeeded); err != nil {
		t.Fatalf("RecordCommand succeeded: %v", err)
	}
	if len(events) != 2 || events[1].Command == nil {
		t.Fatalf("events after succeeded = %+v, want two command events", events)
	}
	if got := events[1].Command.CreatedAt; !got.Equal(wantCreatedAt) {
		t.Errorf("terminal event CreatedAt = %v, want %v (the original accepted CreatedAt)", got, wantCreatedAt)
	}
	if got := events[1].Command.AfterMessageID; got != wantAnchor {
		t.Errorf("terminal event AfterMessageID = %q, want %q", got, wantAnchor)
	}

	data, err := os.ReadFile(sessionPath(dir, s.ID))
	if err != nil {
		t.Fatal(err)
	}
	type rawCommandLine struct {
		Type    string `json:"type"`
		Command struct {
			Status         string `json:"status"`
			CreatedAt      string `json:"created_at"`
			AfterMessageID string `json:"after_message_id"`
		} `json:"command"`
	}
	var acceptedRaw, succeededRaw *rawCommandLine
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rl rawCommandLine
		if err := json.Unmarshal([]byte(line), &rl); err != nil || rl.Type != "command" {
			continue
		}
		switch rl.Command.Status {
		case "accepted":
			rl := rl
			acceptedRaw = &rl
		case "succeeded":
			rl := rl
			succeededRaw = &rl
		}
	}
	if acceptedRaw == nil || succeededRaw == nil {
		t.Fatalf("raw journal missing accepted or succeeded command line: %s", data)
	}
	if succeededRaw.Command.CreatedAt == "" || succeededRaw.Command.CreatedAt == "0001-01-01T00:00:00Z" {
		t.Fatalf("on-disk terminal record CreatedAt = %q, want the original accepted timestamp", succeededRaw.Command.CreatedAt)
	}
	if succeededRaw.Command.CreatedAt != acceptedRaw.Command.CreatedAt {
		t.Errorf("on-disk terminal record CreatedAt = %q, want %q (the accepted record's own)", succeededRaw.Command.CreatedAt, acceptedRaw.Command.CreatedAt)
	}
	if succeededRaw.Command.AfterMessageID != "msg_durable" {
		t.Errorf("on-disk terminal record AfterMessageID = %q, want msg_durable", succeededRaw.Command.AfterMessageID)
	}
}

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
