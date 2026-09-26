package message

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCommandRecordWireShape(t *testing.T) {
	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	rec := CommandRecord{ID: "cmd_1", Line: "/compact 5", Name: "compact",
		Args: map[string]any{"keep_turns": 5}, Source: PromptSourceTyped,
		Status: CommandSucceeded, Text: "/compact succeeded",
		Result: json.RawMessage(`{"ok":true}`), AfterMessageID: "msg_1",
		CreatedAt: at, UpdatedAt: at}
	got, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"cmd_1","line":"/compact 5","name":"compact","args":{"keep_turns":5},"source":"typed","status":"succeeded","text":"/compact succeeded","result":{"ok":true},"after_message_id":"msg_1","created_at":"2026-09-24T12:00:00Z","updated_at":"2026-09-24T12:00:00Z"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestCommandStatusTerminal(t *testing.T) {
	for s, want := range map[CommandStatus]bool{CommandAccepted: false, CommandSucceeded: true,
		CommandFailed: true, CommandRefused: true, CommandUnsupported: true, CommandInterrupted: true} {
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
	}
}
