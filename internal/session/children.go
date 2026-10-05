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
// running turn. The text holds at most ResultCap runes. ok is false while a
// turn runs, is suspended, or waits for an answer, and after a completed turn
// while an input waits: the next turn reports.
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
	reason, guidance := failReason(last), failGuidance(id, last)
	if reason != "" {
		b.WriteString(": " + reason + guidance)
	}
	text, _ := CapRunes(LastText(s.History()), ResultCap)
	if text != "" {
		b.WriteString("\n\n" + text)
	}
	report := []eventlog.Part{{Type: eventlog.PartText, Text: b.String()}, {Type: eventlog.PartTaskReport, Text: taskLine(id, s, out, reason, guidance, text)}}
	return eventlog.ChildSettled{ChildID: id, Outcome: out, ResultRef: last.TurnID}, report, true
}

// The reasons that a report gives for a failed turn, by the class of its error.
const (
	// ReasonExhausted is the reason of a turn that a usage limit failed.
	ReasonExhausted = "provider capacity exhausted for this account"
	// ReasonRateLimited is the reason of a turn that a rate limit failed after its retries.
	ReasonRateLimited = "provider rate limit outlasted the retry budget for this account"
	reasonPermanent   = "turn failed with a permanent provider error and cannot succeed on retry"
	reasonUnrecovered = "turn failed and did not recover"
)

// failReason is the reason that the report of a child gives for the end of
// its last turn: the error behind a prefix that names its class, so a parent
// knows which response fits. A turn with no error reads its cause. An error
// with no class, such as one that an engine journal gave, reads as it is.
func failReason(last eventlog.TurnEnded) string {
	if last.Error == "" {
		return last.Detail()
	}
	switch {
	case last.ErrorClass == eventlog.ErrorRateLimited:
		return ReasonRateLimited + ": " + last.Error
	case last.Cause == eventlog.CauseProviderExhausted:
		return ReasonExhausted + ": " + last.Error
	case last.ErrorClass == "":
		return last.Error
	case last.ErrorClass == eventlog.ErrorTimedOut:
		return "timed out"
	case last.ErrorClass == eventlog.ErrorPermanent:
		return reasonPermanent + ": " + last.Error
	case last.ErrorClass == eventlog.ErrorUnrecovered:
		return reasonUnrecovered + ": " + last.Error
	}
	return fmt.Sprintf("provider %s errors exhausted the retry budget: %s", last.ErrorClass, last.Error)
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

// taskLine is the line of a child in the task segment: its agent, its outcome
// word, its result or its reason, its token usage, and the guidance for its
// parent. A newline in the text of the child becomes a space, so the child
// cannot start a line of its own.
func taskLine(id string, s *eventlog.State, out eventlog.Outcome, reason, guidance, text string) string {
	word, body := "done", text
	switch out {
	case eventlog.OutcomeFailed:
		word, body = "failed", reason
	case eventlog.OutcomeCanceled:
		word, body, guidance = "failed", "canceled", ""
	}
	oneLine := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace
	u := s.Usage()
	return fmt.Sprintf("%s (agent=%s) %s: %s (usage: %d in / %d out)%s", id, s.Agent(), word, oneLine(body), u.InputTokens, u.OutputTokens, oneLine(guidance))
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
