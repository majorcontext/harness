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

// Settle appends the outcome of an unsettled child, and admits rep as an
// input with source child when rep is not nil. A report joins a busy turn at
// its next item boundary, except for a backend that owns the loop: that
// report waits in the queue and reaches the model when the next turn starts.
// A child that is not unsettled changes nothing, so a repeated report is
// safe.
func (a *Actor) Settle(ctx context.Context, s eventlog.ChildSettled, rep *Report) error {
	var big bigResult
	if rep != nil {
		big = a.stage(ctx, s.ChildID, rep)
	}
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		switch {
		case !slices.Contains(a.state.Unsettled(), s.ChildID):
			reply(struct{}{}, nil)
		case rep == nil:
			reply(struct{}{}, a.append(s))
		default:
			text, retained := a.shown(rep, big)
			_, err := a.admit(a.reportInput(rep, text), "", append(append(a.resumed(), s), retained...)...)
			reply(struct{}{}, err)
		}
	})
	return err
}

// Relay admits rep as an input with source child, with the delivery of
// Settle, for a descendant that is no child of this session. It appends no
// child.settled.
func (a *Actor) Relay(ctx context.Context, rep *Report) error {
	big := a.stage(ctx, rep.child, rep)
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		text, retained := a.shown(rep, big)
		_, err := a.admit(a.reportInput(rep, text), "", append(a.resumed(), retained...)...)
		reply(struct{}{}, err)
	})
	return err
}

func (a *Actor) reportInput(rep *Report, text string) eventlog.InputAdmitted {
	delivery := eventlog.DeliverySteer
	if a.ownsLoop() {
		delivery = eventlog.DeliveryQueue
	}
	return eventlog.InputAdmitted{InputID: newID("input"), Delivery: delivery, Source: sourceChild, Parts: rep.Parts(text)}
}

// Report is the outcome of a child as its parent reads it.
type Report struct {
	child, agent, turn string
	outcome            eventlog.Outcome
	reason, guidance   string
	result             string
	usage              eventlog.Usage
}

// Reason returns why the child failed, as the report gives it.
func (r *Report) Reason() string { return r.reason }

// Settlement returns the outcome of the last ended turn of child session
// id, and its report. A turn that the end of a session stopped (cause ended)
// gives no report. ok is false while a turn runs, is suspended, or waits for
// an answer, and after a completed turn while an input waits: the next turn
// reports.
func Settlement(id string, s *eventlog.State) (eventlog.ChildSettled, *Report, bool) {
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
	settled := eventlog.ChildSettled{ChildID: id, Outcome: out, ResultRef: last.TurnID}
	if last.Cause == eventlog.CauseEnded {
		return settled, nil, true
	}
	r := &Report{child: id, agent: s.Agent(), turn: last.TurnID, outcome: out, reason: failReason(last), guidance: failGuidance(id, last),
		result: LastText(s.History()), usage: s.Usage()}
	return settled, r, true
}

// Parts returns the report as the parts of an input: the trigger sentence of
// the engine, which is the text of a turn that only reports start, and the
// task line, which holds the result as the Task tool of Claude Code returns
// the last assistant text. result is the text of the child as the parent
// reads it.
func (r *Report) Parts(result string) []eventlog.Part {
	return []eventlog.Part{{Type: eventlog.PartText, Text: eventlog.ReportTrigger}, {Type: eventlog.PartTaskReport, Text: r.line(result)}}
}

// The reasons that a report gives for a failed turn, by the class of its error.
const (
	// reasonExhausted is the reason of a turn that a usage limit failed.
	reasonExhausted = "provider capacity exhausted for this account"
	// reasonRateLimited is the reason of a turn that a rate limit failed after its retries.
	reasonRateLimited = "provider rate limit outlasted the retry budget for this account"
	// reasonLostToRestart is the reason of a child whose turn a restart ended before it recorded an outcome.
	reasonLostToRestart = "lost to restart: turn was in flight when the process last stopped"
	reasonPermanent     = "turn failed with a permanent provider error and cannot succeed on retry"
	reasonUnrecovered   = "turn failed and did not recover"
)

// failReason is the reason that the report of a child gives for the end of
// its last turn: the error behind a prefix that names its class, so a parent
// knows which response fits. A turn with no error reads its cause. An error
// with no class reads as it is.
func failReason(last eventlog.TurnEnded) string {
	if last.Error == "" && last.Cause == eventlog.CauseCrashed {
		return reasonLostToRestart
	}
	if last.Error == "" {
		return last.Detail()
	}
	detail := last.ReportError()
	switch {
	case last.ErrorClass == eventlog.ErrorRateLimited:
		return reasonRateLimited + ": " + detail
	case last.Cause == eventlog.CauseProviderExhausted:
		return reasonExhausted + ": " + detail
	case last.ErrorClass == "":
		return detail
	case last.ErrorClass == eventlog.ErrorTimedOut:
		return "timed out"
	case last.ErrorClass == eventlog.ErrorPermanent:
		return reasonPermanent + ": " + detail
	case last.ErrorClass == eventlog.ErrorUnrecovered:
		return reasonUnrecovered + ": " + detail
	}
	return fmt.Sprintf("provider %s errors exhausted the retry budget: %s", last.ErrorClass, detail)
}

// failGuidance is what the parent of a child that a wall of the provider
// account stopped should do: the child is kept, a replacement would meet the
// same wall, and a send of the task tool runs this same child again.
func failGuidance(id string, last eventlog.TurnEnded) string {
	if last.Cause != eventlog.CauseProviderExhausted && last.ErrorClass != eventlog.ErrorRateLimited {
		return ""
	}
	when := ""
	if last.RecoverHint != "" {
		when = " after " + last.RecoverHint
	}
	return " — provider exhausted, child preserved: do not spawn a replacement (every session on this provider account hits the same wall); resume this child" + when + " with task send on session_id " + id
}

// line is the line of the child in the task segment: its agent, its outcome
// word, its result or its reason, its token usage, and the guidance for its
// parent. A newline in the text of the child becomes a space, so the child
// cannot start a line of its own.
func (r *Report) line(result string) string {
	word, body, guidance := "done", result, r.guidance
	switch r.outcome {
	case eventlog.OutcomeFailed:
		word, body = "failed", r.reason
	case eventlog.OutcomeCanceled:
		word, body, guidance = "failed", "canceled", ""
	}
	oneLine := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace
	return fmt.Sprintf("%s (agent=%s) %s: %s (usage: %d in / %d out)%s", r.child, r.agent, word, oneLine(body), r.usage.InputTokens, r.usage.OutputTokens, oneLine(guidance))
}

// ResultCap bounds the result of a child in a report, in runes.
const ResultCap = 4000

// CutMark ends a text that CapRunes cut.
const CutMark = "… [truncated]"

// CapRunes cuts s to n runes and marks the cut.
func CapRunes(s string, n int) (string, bool) {
	runes := 0
	for i := range s {
		if runes == n {
			return s[:i] + CutMark, true
		}
		runes++
	}
	return s, false
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
