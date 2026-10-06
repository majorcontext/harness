package migrate

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// claudeCodeState is the backend.state name of the Claude Code backend.
const claudeCodeState = "claude-code"

// readJournal reads session id of the engine journals in dir. The engine
// loader is the reader, so the history is the transcript that the engine
// shows. model is the model of a journal that names none, as the engine
// gives it the model of its server.
func readJournal(dir, id string, model message.ModelRef) (old, error) {
	recs, err := engine.LoadJournal(dir, id)
	if err != nil {
		return old{}, err
	}
	for _, r := range recs {
		if (r.Type == "session" || r.Type == "model") && !r.Model.IsZero() {
			model = r.Model
		}
	}
	if model.IsZero() {
		return old{}, errors.New("the journal names no model, and no fallback model is given")
	}
	s, err := engine.LoadSession(engine.Config{SessionDir: dir, Model: model}, id)
	if err != nil {
		return old{}, err
	}
	history, commands := s.HistoryAndCommands()
	o := old{
		created: eventlog.SessionCreated{ParentID: s.TaskParentID(), Agent: s.TaskAgentType(), Model: s.Model().String(),
			Settings: eventlog.Settings{Effort: string(s.Effort()), ServiceTier: s.ServiceTier()},
			Origin:   origin(s.TaskParentID()), AllowedTools: s.TaskToolNames()},
		at:        s.CreatedAt(),
		history:   message.ResolveOrphanToolCalls(history),
		commands:  commands,
		compacted: s.CompactionCount() > 0,
		end:       eventlog.TurnEnded{StopReason: eventlog.StopCompleted},
		blobs:     map[string][]byte{},
	}
	if s.TaskParentID() != "" {
		o.end = childEnd(recs, history)
	}
	o.tail = children(recs)
	var cost *float64
	if sub := s.SubscriptionUsage(); sub != nil {
		cost = sub.SessionCostUSD
	}
	if u := usage(s.Usage()); u != (eventlog.Usage{}) || cost != nil {
		o.tail = append([]eventlog.Event{eventlog.ContextMeasured{Usage: u, CostUSD: cost, Source: "migrated"}}, o.tail...)
	}
	retained, err := retainedResults(dir, id, recs, o.blobs)
	if err != nil {
		return old{}, err
	}
	o.tail = append(o.tail, retained...)
	cli, err := claudeCodeSession(dir, id)
	if err != nil {
		return old{}, err
	}
	if cli != "" {
		blob, err := external.Mirror{SessionID: cli, Parked: s.PendingQuestion()}.Encode()
		if err != nil {
			return old{}, err
		}
		o.blobs[claudeCodeState] = blob
		o.tail = append(o.tail, eventlog.BackendState{Backend: claudeCodeState, BlobKey: claudeCodeState})
	}
	if cond, ok := s.ActiveGoal(); ok {
		o.tail = append(o.tail, eventlog.GoalSet{Condition: cond, MaxTurns: max(s.GoalMaxTurns(), 0)})
	}
	for _, p := range s.QueuedPrompts() {
		parts := []eventlog.Part{{Type: eventlog.PartText, Text: p.Text}}
		for _, b := range p.Blobs {
			parts = append(parts, attachment(b, o.blobs))
		}
		o.tail = append(o.tail, eventlog.InputAdmitted{InputID: cmp.Or(p.MessageID, fmt.Sprintf("queued_%d", p.ID)),
			Delivery: eventlog.DeliveryQueue, Source: cmp.Or(string(p.Source), "user"), SourceID: p.SourceID, SourceLabel: p.SourceLabel, Parts: parts})
	}
	return o, nil
}

func origin(parent string) string {
	if parent != "" {
		return "task"
	}
	return "migrated"
}

func usage(u provider.Usage) eventlog.Usage {
	return eventlog.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens)}
}

// childEnd ends the last turn of a child as engine recovery reports it: with
// the outcome committed since the last message, or, for a turn that never
// settled, done when it ends with an answer and failed otherwise.
func childEnd(recs []engine.JournalRecord, history []message.Message) eventlog.TurnEnded {
	var commit *engine.JournalRecord
	unsettled := false
	for i, r := range recs {
		switch r.Type {
		case "message":
			if !r.RecoveryMarker {
				commit = nil
			}
			unsettled = true
		case "child_turn.settled":
			unsettled = false
		case "task.outcome_committed":
			commit = &recs[i]
		}
	}
	switch {
	case commit != nil && (commit.TaskCanceled || commit.TaskStatus == string(engine.StatusCanceled)):
		return eventlog.TurnEnded{StopReason: eventlog.StopInterrupted, Cause: eventlog.CauseStopped}
	case commit != nil && commit.TaskStatus == string(engine.StatusFailed):
		end := eventlog.TurnEnded{StopReason: eventlog.StopFailed, Error: commit.TaskFailReason}
		if commit.TaskFailKind == engine.FailKindProviderExhausted {
			end.Cause = eventlog.CauseProviderExhausted
			end.ErrorClass, end.Error = wallCause(end.Error)
			end.RecoverHint = commit.TaskFailHint
		}
		return end
	case commit == nil && unsettled:
		return inFlightEnd(history)
	}
	return eventlog.TurnEnded{StopReason: eventlog.StopCompleted}
}

// wallCause splits the classified reason that the engine journaled for a wall
// of the provider account into its class and its cause. The report builds the
// prefix of the class again, so the end keeps only the cause.
func wallCause(reason string) (eventlog.ErrorClass, string) {
	if cause, ok := strings.CutPrefix(reason, session.ReasonRateLimited+": "); ok {
		return eventlog.ErrorRateLimited, cause
	}
	return "", strings.TrimPrefix(reason, session.ReasonExhausted+": ")
}

// inFlightEnd ends a child turn that was running at the cutover. A turn
// whose last message is an answer with no tool call is done.
func inFlightEnd(history []message.Message) eventlog.TurnEnded {
	if n := len(history); n > 0 && history[n-1].Role == message.RoleAssistant &&
		!slices.ContainsFunc(history[n-1].Parts, func(p message.Part) bool { _, ok := p.(*message.ToolCall); return ok }) {
		return eventlog.TurnEnded{StopReason: eventlog.StopCompleted}
	}
	return eventlog.TurnEnded{StopReason: eventlog.StopFailed, Error: session.ReasonLostToRestart}
}

// children returns child.spawned for each child, and child.settled for each
// child whose newest report reached the parent. A child with a report that
// did not reach the parent stays unsettled, so the runtime reports it again.
func children(recs []engine.JournalRecord) []eventlog.Event {
	var order []string
	agents := map[string]string{}
	last := map[string]engine.JournalRecord{}
	for _, r := range recs {
		switch r.Type {
		case "task.spawned":
			if _, ok := agents[r.ChildID]; !ok && r.ChildID != "" {
				order = append(order, r.ChildID)
			}
			agents[r.ChildID] = r.Agent
		case "task.notify_queued", "task.notify_delivered":
			last[r.ChildID] = r
		}
	}
	var out []eventlog.Event
	for _, id := range order {
		out = append(out, eventlog.ChildSpawned{ChildID: id, Agent: agents[id]})
		r, ok := last[id]
		if !ok || r.Type != "task.notify_delivered" {
			continue
		}
		outcome := eventlog.OutcomeDone
		switch {
		case r.TaskCanceled || r.TaskStatus == string(engine.StatusCanceled):
			outcome = eventlog.OutcomeCanceled
		case r.TaskStatus == string(engine.StatusFailed):
			outcome = eventlog.OutcomeFailed
		}
		out = append(out, eventlog.ChildSettled{ChildID: id, Outcome: outcome})
	}
	return out
}

// retainedResults reads each retained tool result file of the session into
// blobs, in handle order. The files are the source of truth: a crash can
// lose the record of a file that the history names.
func retainedResults(dir, id string, recs []engine.JournalRecord, blobs map[string][]byte) ([]eventlog.Event, error) {
	entries, err := os.ReadDir(toolResultDir(dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tools := map[string]string{}
	for _, r := range recs {
		if r.Type == "toolresult.retained" {
			tools[r.ToolResultHandle] = r.ToolResultTool
		}
	}
	var handles []string
	for _, e := range entries {
		h, ok := strings.CutSuffix(e.Name(), ".txt")
		if _, valid := toolresult.Number(h); ok && valid && e.Type().IsRegular() {
			handles = append(handles, h)
		}
	}
	slices.SortFunc(handles, func(a, b string) int {
		x, _ := toolresult.Number(a)
		y, _ := toolresult.Number(b)
		return x - y
	})
	var out []eventlog.Event
	for _, h := range handles {
		data, err := os.ReadFile(filepath.Join(toolResultDir(dir, id), h+".txt"))
		if err != nil {
			return nil, err
		}
		m := toolresult.NewMeta(h, tools[h], h, string(data))
		blobs[h] = data
		out = append(out, eventlog.ToolResultRetained{Handle: m.Handle, Tool: m.Tool, BlobKey: m.Key, Bytes: m.Bytes, Lines: m.Lines, Head: m.Head})
	}
	return out, nil
}

// claudeCodeSession returns the newest Claude Code CLI session ID of the
// journal, or "". A torn last line is skipped, as the engine skips it.
func claudeCodeSession(dir, id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		return "", err
	}
	var cli string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for sc.Scan() {
		if !bytes.Contains(sc.Bytes(), []byte(`"claude_code.session_id"`)) {
			continue
		}
		var r struct {
			Type string `json:"type"`
			ID   string `json:"claude_code_session_id"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type == "claude_code.session_id" {
			cli = r.ID
		}
	}
	return cli, sc.Err()
}
