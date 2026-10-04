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
// turn spawns no child.
func (a *Actor) Spawn(ctx context.Context, child, agent string) (eventlog.SessionCreated, error) {
	return call(ctx, a, func(reply func(eventlog.SessionCreated, error)) {
		if err := ctx.Err(); err != nil {
			reply(eventlog.SessionCreated{}, err)
			return
		}
		c := eventlog.SessionCreated{ParentID: a.cfg.ID, Agent: agent, Model: a.state.Model(),
			Settings: a.state.Settings(), AllowedTools: a.state.AllowedTools()}
		reply(c, a.append(eventlog.ChildSpawned{ChildID: child, Agent: agent}))
	})
}

// Settle appends the outcome of an unsettled child, and admits text as an
// input with source child when text is not empty. A child that is not
// unsettled changes nothing, so a repeated report is safe.
func (a *Actor) Settle(ctx context.Context, s eventlog.ChildSettled, text string) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		switch {
		case !slices.Contains(a.state.Unsettled(), s.ChildID):
			reply(struct{}{}, nil)
		case text == "":
			reply(struct{}{}, a.append(s))
		default:
			in := eventlog.InputAdmitted{InputID: newID("input"), Delivery: eventlog.DeliveryQueue, Source: sourceChild,
				Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
			_, err := a.admit(in, "", append(a.resumed(), s)...)
			reply(struct{}{}, err)
		}
	})
	return err
}

// Unsettled returns the spawned children that have not settled.
func (a *Actor) Unsettled(ctx context.Context) ([]string, error) {
	return call(ctx, a, func(reply func([]string, error)) { reply(a.state.Unsettled(), nil) })
}

// report sends the outcome of the turn that ended to the parent.
func (a *Actor) report() {
	parent := a.state.Summary().ParentID
	if parent == "" || a.cfg.Report == nil {
		return
	}
	if s, text, ok := Settlement(a.cfg.ID, a.state); ok {
		a.cfg.Report(parent, s, text)
	}
}

// Settlement returns the outcome of the last ended turn of child session
// id, and the text that reports it to the parent: the last assistant text,
// as the Task tool of Claude Code returns. ok is false while a turn runs,
// is suspended, or waits for an answer, and after a completed turn while
// an input waits: the next turn reports.
func Settlement(id string, s *eventlog.State) (eventlog.ChildSettled, string, bool) {
	last := s.LastEnded()
	_, busy := s.Turn()
	if busy || last.TurnID == "" || last.StopReason == eventlog.StopAwaitingInput || last.StopReason == eventlog.StopCompleted && len(s.Queue()) > 0 {
		return eventlog.ChildSettled{}, "", false
	}
	out := eventlog.OutcomeDone
	switch {
	case last.StopReason == eventlog.StopFailed, eventlog.Cause(last.Error) == eventlog.CauseCrashed:
		out = eventlog.OutcomeFailed
	case last.StopReason == eventlog.StopInterrupted:
		out = eventlog.OutcomeCanceled
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A background task you started has finished.\n\ntask: %s (agent %s)\noutcome: %s", id, s.Agent(), out)
	if last.Error != "" {
		b.WriteString(": " + last.Error)
	}
	if text := LastText(s.History()); text != "" {
		b.WriteString("\n\n" + text)
	}
	return eventlog.ChildSettled{ChildID: id, Outcome: out, ResultRef: last.TurnID}, b.String(), true
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
