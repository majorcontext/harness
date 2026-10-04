package migrate

import (
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
	Type             string           `json:"type"`
	SessionID        string           `json:"session_id"`
	RecordedAt       time.Time        `json:"recorded_at"`
	Message          *message.Message `json:"message"`
	Model            message.ModelRef `json:"model"`
	Effort           *string          `json:"effort"`
	ServiceTier      *string          `json:"service_tier"`
	GoalCondition    string           `json:"goal_condition"`
	CompactFirstID   string           `json:"compact_first_id"`
	CompactLastID    string           `json:"compact_last_id"`
	CompactSummaryID string           `json:"compact_summary_id"`
	ParentSessionID  string           `json:"parent_session_id"`
	AgentType        string           `json:"agent_type"`
}

type mirrorSession struct {
	o      old
	goal   string
	active bool
	err    error
}

// Mirror converts the box journal records that boxes mirrors in its
// box_journal_records table to a log for each session in st. records are
// the records of one box generation in seq order. A session that st
// already holds is skipped.
func Mirror(ctx context.Context, records []json.RawMessage, st harness.Store) ([]Result, error) {
	var order []string
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
		out = append(out, convert(ctx, st, id, s.finish))
	}
	return out, nil
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
		}
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
	o.history = message.ResolveOrphanToolCalls(o.history)
	if s.active {
		o.tail = append(o.tail, eventlog.GoalSet{Condition: s.goal})
	}
	return o, nil
}
