package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/eventlog"
)

// sourceChild is the source of an input that reports the outcome of a child.
const sourceChild = "child"

// Spawn appends child.spawned and returns the session.created record of the
// child: this session as its parent, with the model, settings, and allowed
// tools of this session. It appends nothing once ctx ends, so a stopped
// turn spawns no child, and nothing for a child that has not settled.
func (a *Actor) Spawn(ctx context.Context, child, agent string) (eventlog.SessionCreated, error) {
	return call(ctx, a, func(reply func(eventlog.SessionCreated, error)) {
		if err := ctx.Err(); err != nil {
			reply(eventlog.SessionCreated{}, err)
			return
		}
		c := eventlog.SessionCreated{ParentID: a.cfg.ID, Agent: agent, Model: a.state.Model(),
			Settings: a.state.Settings(), AllowedTools: a.state.AllowedTools()}
		if slices.Contains(a.state.Unsettled(), child) {
			reply(c, nil)
			return
		}
		reply(c, a.append(eventlog.ChildSpawned{ChildID: child, Agent: agent}))
	})
}

// Settle appends the outcome of an unsettled child, and admits report as an
// input with source child when report is not empty. A report joins a busy
// turn at its next item boundary, except for a backend that owns the loop:
// that report waits in the queue and reaches the model when the next turn
// starts. A child that is not unsettled changes nothing, so a repeated
// report is safe.
func (a *Actor) Settle(ctx context.Context, s eventlog.ChildSettled, report []eventlog.Part) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		switch {
		case !slices.Contains(a.state.Unsettled(), s.ChildID):
			reply(struct{}{}, nil)
		case len(report) == 0:
			reply(struct{}{}, a.append(s))
		default:
			delivery := eventlog.DeliverySteer
			if a.ownsLoop() {
				delivery = eventlog.DeliveryQueue
			}
			in := eventlog.InputAdmitted{InputID: newID("input"), Delivery: delivery, Source: sourceChild, Parts: report}
			_, err := a.admit(in, "", append(a.resumed(), s)...)
			reply(struct{}{}, err)
		}
	})
	return err
}

// Settlement returns the outcome of the last ended turn of child session
// id, and the parts that report it to the parent: the text for a parent that
// starts a turn with it, which holds the last assistant text as the Task tool
// of Claude Code returns, and the task line for a parent that takes it in a
// running turn. ok is false while a turn runs, is suspended, or waits for an
// answer, and after a completed turn while an input waits: the next turn
// reports.
func Settlement(id string, s *eventlog.State) (eventlog.ChildSettled, []eventlog.Part, bool) {
	last := s.LastEnded()
	_, busy := s.Turn()
	if busy || last.TurnID == "" || last.StopReason == eventlog.StopAwaitingInput || last.StopReason == eventlog.StopCompleted && len(s.Queue()) > 0 {
		return eventlog.ChildSettled{}, nil, false
	}
	out := eventlog.OutcomeDone
	switch {
	case last.StopReason == eventlog.StopFailed, last.Cause == eventlog.CauseCrashed:
		out = eventlog.OutcomeFailed
	case last.StopReason == eventlog.StopInterrupted:
		out = eventlog.OutcomeCanceled
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A background task you started has finished.\n\ntask: %s (agent %s)\noutcome: %s", id, s.Agent(), out)
	if d := last.Detail(); d != "" {
		b.WriteString(": " + d)
	}
	text := LastText(s.History())
	if text != "" {
		b.WriteString("\n\n" + text)
	}
	report := []eventlog.Part{{Type: eventlog.PartText, Text: b.String()}, {Type: eventlog.PartTaskReport, Text: taskLine(id, s, out, last.Detail(), text)}}
	return eventlog.ChildSettled{ChildID: id, Outcome: out, ResultRef: last.TurnID}, report, true
}

// taskLine is the line of a child in the task segment: its agent, its outcome
// word, its result or its error, and its token usage. A newline in the text
// of the child becomes a space, so the child cannot start a line of its own.
func taskLine(id string, s *eventlog.State, out eventlog.Outcome, detail, text string) string {
	word, body := "done", text
	switch out {
	case eventlog.OutcomeFailed:
		word, body = "failed", detail
	case eventlog.OutcomeCanceled:
		word, body = "failed", "canceled"
	}
	body = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(body)
	u := s.Usage()
	return fmt.Sprintf("%s (agent=%s) %s: %s (usage: %d in / %d out)", id, s.Agent(), word, body, u.InputTokens, u.OutputTokens)
}

// LastText returns the text of the newest assistant message with text in h.
func LastText(h []eventlog.Message) string {
	for _, m := range slices.Backward(h) {
		var parts []string
		for _, p := range m.Parts {
			if m.Role == eventlog.RoleAssistant && p.Type == eventlog.PartText {
				parts = append(parts, p.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}
