package acp

import (
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

type compactionHarness struct {
	h    *held
	st   *pumpState
	sent []map[string]any
}

func newCompactionHarness(enabled bool) *compactionHarness {
	c := &compactionHarness{st: newPumpState(protocol.Session{})}
	c.h = newHeld("ses_1", heldConfig{compaction: enabled, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		send: func(_ string, params any) error {
			body, _ := json.Marshal(params.(map[string]any)["update"])
			var u map[string]any
			_ = json.Unmarshal(body, &u)
			c.sent = append(c.sent, u)
			return nil
		}})
	return c
}

func (c *compactionHarness) event(t *testing.T, seq uint64, kind string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	e := protocol.Event{Seq: seq, Kind: kind, Data: raw}
	switch kind {
	case protocol.KindStatus:
		err = c.h.compactionStatus(e, c.st)
	case protocol.KindCompactionApplied:
		err = c.h.compactionApplied(e, c.st)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func (c *compactionHarness) lifecycle() (out []string) {
	for _, u := range c.sent {
		id, _ := u["compactionId"].(string)
		switch u["sessionUpdate"] {
		case "compaction_update":
			status, _ := u["status"].(string)
			out = append(out, id+":"+status)
		case "compaction_summary_chunk":
			out = append(out, id+":chunk")
		}
	}
	return out
}

func TestCompactionOfTheCLIEndsAtTheRecordAndNotAtTheRunningFrame(t *testing.T) {
	c := newCompactionHarness(true)
	c.event(t, 0, protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompacting, TurnID: "turn_1"})
	c.event(t, 0, protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusRunning, TurnID: "turn_1"})
	c.event(t, 7, protocol.KindCompactionApplied, protocol.CompactionApplied{Summary: "the summary"})
	c.event(t, 0, protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompacting, TurnID: "turn_1"})
	c.event(t, 0, protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompactionFailed, TurnID: "turn_1"})
	want := []string{"turn_1:in_progress", "turn_1:chunk", "turn_1:completed", "turn_1_2:in_progress", "turn_1_2:failed"}
	if got := c.lifecycle(); !slices.Equal(got, want) {
		t.Errorf("lifecycle = %v, want %v", got, want)
	}
}

func TestCompactionRecordWithoutAStartFrameUsesTheSummaryMessageID(t *testing.T) {
	c := newCompactionHarness(true)
	c.event(t, 9, protocol.KindCompactionApplied, protocol.CompactionApplied{Summary: "the summary"})
	if got, want := c.lifecycle(), []string{"cmpsum_9:chunk", "cmpsum_9:completed"}; !slices.Equal(got, want) {
		t.Errorf("lifecycle = %v, want %v", got, want)
	}
}

func TestCompactionSendsNothingWithoutTheCapability(t *testing.T) {
	c := newCompactionHarness(false)
	c.event(t, 0, protocol.KindStatus, protocol.StatusFrame{Status: protocol.StatusCompacting, TurnID: "turn_1"})
	c.event(t, 7, protocol.KindCompactionApplied, protocol.CompactionApplied{Summary: "the summary"})
	if err := c.h.replayCompaction(protocol.Message{ID: "cmpsum_7"}); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 0 {
		t.Errorf("sent %v, want nothing", c.sent)
	}
}
