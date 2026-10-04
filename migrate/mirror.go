package migrate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/message"
)

// mirrorRecord is the part of a box journal record that the conversion reads.
type mirrorRecord struct {
	Type             string                 `json:"type"`
	SessionID        string                 `json:"session_id"`
	Seq              int64                  `json:"seq"`
	RecordedAt       time.Time              `json:"recorded_at"`
	Message          *message.Message       `json:"message"`
	Command          *message.CommandRecord `json:"command"`
	Model            message.ModelRef       `json:"model"`
	Effort           *string                `json:"effort"`
	ServiceTier      *string                `json:"service_tier"`
	Outcome          string                 `json:"outcome"`
	Error            string                 `json:"error"`
	GoalCondition    string                 `json:"goal_condition"`
	CompactFirstID   string                 `json:"compact_first_id"`
	CompactLastID    string                 `json:"compact_last_id"`
	CompactSummaryID string                 `json:"compact_summary_id"`
	ParentSessionID  string                 `json:"parent_session_id"`
	AgentType        string                 `json:"agent_type"`
	QueueID          int64                  `json:"queue_id"`
	QueueText        string                 `json:"queue_text"`
	QueueSource      string                 `json:"queue_source"`
	QueueMessageID   string                 `json:"queue_message_id"`
}

type mirrorSession struct {
	o      old
	goal   string
	active bool
	// end ends the last turn when a record after its last message ends it.
	end   *eventlog.TurnEnded
	queue []mirrorRecord
	err   error
}

// Mirror converts the box journal records that boxes mirrors in its
// box_journal_records table to a log for each session in st. records are
// the records of one box generation in seq order from seq 1. A session that
// st already holds is skipped. A missing seq fails every session, because
// the missing record can belong to any of them.
func Mirror(ctx context.Context, records []json.RawMessage, st harness.Store) ([]Result, error) {
	var order []string
	var gap error
	sessions := map[string]*mirrorSession{}
	get := func(id string) *mirrorSession {
		if sessions[id] == nil {
			order = append(order, id)
			sessions[id] = &mirrorSession{o: old{end: eventlog.TurnEnded{StopReason: eventlog.StopCompleted}}}
		}
		return sessions[id]
	}
	for i, raw := range records {
		var r mirrorRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("migrate: mirror record %d: %w", i, err)
		}
		if want := int64(i) + 1; gap == nil && r.Seq != want {
			gap = fmt.Errorf("the mirror has seq %d where seq %d belongs, so records are missing", r.Seq, want)
		}
		if r.SessionID == "" {
			continue
		}
		s := get(r.SessionID)
		if s.o.at.IsZero() {
			s.o.at = r.RecordedAt
		}
		if r.Type == "session.spawned" && r.ParentSessionID != "" {
			p := get(r.ParentSessionID)
			p.o.tail = append(p.o.tail, eventlog.ChildSpawned{ChildID: r.SessionID, Agent: r.AgentType})
		}
		if s.err == nil {
			s.err = s.apply(r)
		}
	}
	var out []Result
	for _, id := range order {
		s := sessions[id]
		if p := s.o.created.ParentID; p != "" && s.end != nil {
			sessions[p].o.tail = append(sessions[p].o.tail, eventlog.ChildSettled{ChildID: id, Outcome: outcome(*s.end)})
		}
	}
	for _, id := range order {
		s := sessions[id]
		if s.err == nil {
			s.err = gap
		}
		out = append(out, convert(ctx, st, id, s.finish))
	}
	return out, nil
}

// outcome is the outcome that the runtime settles a child with when its
// last turn ended as e.
func outcome(e eventlog.TurnEnded) eventlog.Outcome {
	switch e.StopReason {
	case eventlog.StopFailed:
		return eventlog.OutcomeFailed
	case eventlog.StopInterrupted:
		return eventlog.OutcomeCanceled
	}
	return eventlog.OutcomeDone
}

// apply folds one record as the server journal reader folds it.
func (s *mirrorSession) apply(r mirrorRecord) error {
	o := &s.o
	switch r.Type {
	case "session.created", "model":
		if !r.Model.IsZero() {
			o.created.Model = r.Model.String()
		}
	case "session.spawned":
		o.created.ParentID, o.created.Agent = r.ParentSessionID, r.AgentType
	case "effort":
		if r.Effort != nil {
			o.created.Settings.Effort = *r.Effort
		}
	case "service_tier":
		if r.ServiceTier != nil {
			o.created.Settings.ServiceTier = *r.ServiceTier
		}
	case "message":
		if r.Message != nil {
			m := *r.Message
			m.Normalize()
			o.history = append(o.history, m)
			s.end = nil
		}
	case "turn.end", "session.error", "session.aborted":
		s.end = turnEnd(r)
	case "command":
		if r.Command != nil {
			s.command(*r.Command)
		}
	case "prompt.queued":
		s.queue = append(s.queue, r)
	case "prompt.dequeued":
		s.queue = slices.DeleteFunc(s.queue, func(q mirrorRecord) bool { return q.QueueID == r.QueueID })
	case "history.compacted":
		return s.compact(r)
	case "goal.set":
		s.active, s.goal = true, r.GoalCondition
	case "goal.updated":
		if s.active {
			s.goal = r.GoalCondition
		}
	case "goal.achieved", "goal.cleared":
		s.active, s.goal = false, ""
	}
	return nil
}

// turnEnd returns the end of a turn that r records. A turn that waits for
// an answer to a question ends completed, as its question is in the
// history, and so does the last turn of a goal that used its turns.
func turnEnd(r mirrorRecord) *eventlog.TurnEnded {
	switch {
	case r.Type == "session.aborted":
		return &eventlog.TurnEnded{StopReason: eventlog.StopInterrupted, Error: string(eventlog.CauseStopped)}
	case r.Type == "turn.end" && slices.Contains([]string{"completed", "awaiting_input", "max_turns_exceeded"}, r.Outcome):
		return &eventlog.TurnEnded{StopReason: eventlog.StopCompleted}
	}
	return &eventlog.TurnEnded{StopReason: eventlog.StopFailed, Error: cmp.Or(r.Error, r.Outcome)}
}

// command folds c as the engine folds a command: the newest record of an
// ID replaces the older one, and keeps its place in the history.
func (s *mirrorSession) command(c message.CommandRecord) {
	cmds := s.o.commands
	if i := slices.IndexFunc(cmds, func(e message.CommandRecord) bool { return e.ID == c.ID }); i >= 0 {
		c.AfterMessageID = cmds[i].AfterMessageID
		cmds[i] = c
		return
	}
	s.o.commands = append(cmds, c)
}

// compact replaces the folded messages with the summary, which an earlier
// message record carried.
func (s *mirrorSession) compact(r mirrorRecord) error {
	h := s.o.history
	at := func(id string) int { return slices.IndexFunc(h, func(m message.Message) bool { return m.ID == id }) }
	sum := at(r.CompactSummaryID)
	if sum < 0 {
		return fmt.Errorf("compaction summary %q is not in the history", r.CompactSummaryID)
	}
	summary := h[sum]
	h = slices.Delete(slices.Clone(h), sum, sum+1)
	first, last := at(r.CompactFirstID), at(r.CompactLastID)
	if first < 0 || last < first {
		return fmt.Errorf("compaction range [%s, %s] is not in the history", r.CompactFirstID, r.CompactLastID)
	}
	s.o.history = slices.Concat(h[:first], []message.Message{summary}, h[last+1:])
	s.o.compacted = true
	return nil
}

func (s *mirrorSession) finish() (old, error) {
	if s.err != nil {
		return old{}, s.err
	}
	o := s.o
	if o.created.Model == "" {
		return old{}, errors.New("the mirror names no model")
	}
	o.created.Origin = origin(o.created.ParentID)
	switch {
	case s.end != nil:
		o.end = *s.end
	case o.created.ParentID != "":
		o.end = inFlightEnd(o.history)
	}
	o.history = message.ResolveOrphanToolCalls(o.history)
	if s.active {
		o.tail = append(o.tail, eventlog.GoalSet{Condition: s.goal})
	}
	for _, q := range s.queue {
		o.tail = append(o.tail, eventlog.InputAdmitted{InputID: cmp.Or(q.QueueMessageID, fmt.Sprintf("queued_%d", q.QueueID)),
			Delivery: eventlog.DeliveryQueue, Source: cmp.Or(q.QueueSource, "user"), Parts: []eventlog.Part{{Type: eventlog.PartText, Text: q.QueueText}}})
	}
	return o, nil
}
