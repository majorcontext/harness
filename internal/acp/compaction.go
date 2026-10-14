package acp

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/majorcontext/harness/protocol"
)

// Compaction statuses of ACP.
const (
	compactionInProgress = "in_progress"
	compactionCompleted  = "completed"
	compactionFailed     = "failed"
	compactionCancelled  = "cancelled"
)

// compactionStatus starts or fails the compaction that a status frame names.
// A frame with idle or running ends nothing: the summary of a compaction of the
// Claude Code CLI follows its running frame.
func (h *held) compactionStatus(e protocol.Event, st *pumpState) error {
	if !h.compaction {
		return nil
	}
	var f protocol.StatusFrame
	if json.Unmarshal(e.Data, &f) != nil {
		return nil
	}
	switch f.Status {
	case protocol.StatusCompacting:
		if st.compaction != "" {
			return nil
		}
		st.compaction = uniqueCompactionID(st, f.TurnID)
		return h.compactionUpdate(st.compaction, compactionInProgress, nil)
	case protocol.StatusCompactionFailed:
		return h.endCompaction(st, compactionFailed)
	}
	return nil
}

// compactionApplied sends the summary of a compaction record, then ends the
// compaction. A record with no earlier compacting frame gets the ID of its
// summary message.
func (h *held) compactionApplied(e protocol.Event, st *pumpState) error {
	if !h.compaction {
		return nil
	}
	p, err := e.Payload()
	if err != nil {
		return nil
	}
	rec, ok := p.(*protocol.CompactionApplied)
	if !ok {
		return nil
	}
	id := st.compaction
	if id == "" {
		id = protocol.CompactionSummaryIDPrefix + strconv.FormatUint(e.Seq, 10)
	}
	if rec.Summary != "" {
		if err := h.update(map[string]any{"sessionUpdate": "compaction_summary_chunk", "compactionId": id,
			"content": textContent{Type: "text", Text: rec.Summary}}); err != nil {
			return err
		}
	}
	st.compaction = ""
	return h.compactionUpdate(id, compactionCompleted, nil)
}

// compactionTurnEnded ends the compaction that its turn left open.
func (h *held) compactionTurnEnded(e protocol.Event, st *pumpState) error {
	if !h.compaction || st.compaction == "" {
		return nil
	}
	status := compactionFailed
	if p, err := e.Payload(); err == nil {
		if ended, ok := p.(*protocol.TurnEnded); ok && ended.StopReason == protocol.StopInterrupted {
			status = compactionCancelled
		}
	}
	return h.endCompaction(st, status)
}

func (h *held) endCompaction(st *pumpState, status string) error {
	id := st.compaction
	if id == "" {
		return nil
	}
	st.compaction = ""
	return h.compactionUpdate(id, status, nil)
}

// compactionUpdate sends a compaction_update. A non-nil summary replaces the
// summary of the entity.
func (h *held) compactionUpdate(id, status string, summary []textContent) error {
	body := map[string]any{"sessionUpdate": "compaction_update", "compactionId": id, "status": status}
	if summary != nil {
		body["summary"] = summary
	}
	return h.update(body)
}

// replayCompaction sends the summary message of a compaction as a completed
// compaction, or nothing when the client takes no compaction updates.
func (h *held) replayCompaction(m protocol.Message) error {
	if !h.compaction {
		return nil
	}
	var summary []textContent
	for _, part := range m.Parts {
		if part.Type == protocol.MessagePartText {
			summary = append(summary, textContent{Type: "text", Text: part.Text})
		}
	}
	return h.compactionUpdate(m.ID, compactionCompleted, summary)
}

// uniqueCompactionID returns id, or id with a count when an earlier compaction
// used it: the compactions of one turn share the ID of the turn.
func uniqueCompactionID(st *pumpState, id string) string {
	st.compactions[id]++
	if n := st.compactions[id]; n > 1 {
		return fmt.Sprintf("%s_%d", id, n)
	}
	return id
}
