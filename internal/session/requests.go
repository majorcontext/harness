package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// ErrRequestNotPending reports a request that is not open.
var ErrRequestNotPending = &protocol.Error{Code: protocol.CodeRequestNotPending, Message: "harness: request not pending"}

// ErrBadAnswer reports an answer that the kind of its request does not take.
var ErrBadAnswer = errors.New("a question takes a map of questions to text")

// checkAnswer accepts for a question only a non-empty map of text.
func checkAnswer(kind string, answer json.RawMessage) error {
	if kind != protocol.RequestQuestion {
		return nil
	}
	var choices map[string]string
	if err := json.Unmarshal(answer, &choices); err != nil || len(choices) == 0 {
		return ErrBadAnswer
	}
	return nil
}

// questions reports whether the turn that starts now may ask the user a
// question: the embedder answers them, and a child or a goal supervised
// session has no user who watches the turn.
func (a *Actor) questions() bool {
	g, _ := a.state.Goal()
	return a.cfg.AskUserQuestion && a.state.Summary().ParentID == "" && g.State != protocol.GoalActive
}

// replies reports whether in is a reply that closes an open request: the input
// of a person (a typed line, an expanded slash command, or an input with no
// named source, which the admission step stores as "user") or the reply of the
// agent of another box. An input of any other source waits in the queue until
// the request resolves.
func replies(in protocol.InputAdmitted) bool {
	switch in.Source {
	case "", protocol.SourceUser, protocol.SourceTyped, protocol.SourceCommand, protocol.SourceBox:
		return true
	}
	return false
}

// dismissFrom returns the dismissal of every open request when q holds a
// reply, and nothing otherwise. A dismissal by the reply of a box names that
// box, so the model does not read that the user dismissed the question.
func (a *Actor) dismissFrom(q []protocol.InputAdmitted) []protocol.Payload {
	i := slices.IndexFunc(q, replies)
	if i < 0 {
		return nil
	}
	events := a.dismissRequests()
	if q[i].Source == protocol.SourceBox {
		for k, ev := range events {
			r := ev.(protocol.RequestResolved)
			r.Source, r.SourceID, r.SourceLabel = q[i].Source, q[i].SourceID, q[i].SourceLabel
			events[k] = r
		}
	}
	return events
}

// heldByRequest reports whether a request is open and q holds no reply, so no
// turn starts from q.
func (a *Actor) heldByRequest(q []protocol.InputAdmitted) bool {
	return len(a.state.Requests()) > 0 && !slices.ContainsFunc(q, replies)
}

// Resolve closes request id and returns the seq of its request.resolved
// record. An answer starts a turn that has no input, so the backend that
// asked reads the answer; a dismissal starts none for the request, and the next
// turn tells the backend. A dismissal starts the turn of the queued inputs.
func (a *Actor) Resolve(ctx context.Context, id string, res protocol.Resolution) (protocol.Resolved, error) {
	answer, dismiss := res.Answer, res.Dismiss
	return call(ctx, a, func(reply func(protocol.Resolved, error)) {
		i := slices.IndexFunc(a.state.Requests(), func(r protocol.RequestOpened) bool { return r.RequestID == id })
		if i < 0 {
			reply(protocol.Resolved{}, ErrRequestNotPending)
			return
		}
		kind := a.state.Requests()[i].RequestKind
		switch {
		case res.Response != "" && kind != protocol.RequestQuestion:
			reply(protocol.Resolved{}, ErrBadAnswer)
			return
		case !dismiss && res.Response == "":
			if err := checkAnswer(kind, answer); err != nil {
				reply(protocol.Resolved{}, err)
				return
			}
		}
		ev := protocol.RequestResolved{RequestID: id, Resolution: protocol.ResolutionDismissed, Source: res.Source, SourceID: res.SourceID, SourceLabel: res.SourceLabel}
		if !dismiss {
			ev.Resolution, ev.Answer, ev.Response = protocol.ResolutionAnswered, answer, res.Response
		}
		got := protocol.Resolved{Seq: a.state.Head() + 1, Status: protocol.ResolvedDismissed}
		if dismiss {
			err := a.append(ev)
			if err == nil && a.run == nil {
				err = a.next(true)
			}
			if err == nil {
				err = a.blockUndriven()
			}
			reply(got, err)
			return
		}
		if a.run != nil {
			reply(protocol.Resolved{}, ErrBusy)
			return
		}
		turnID := newID("turn")
		if err := a.append(ev, protocol.TurnStarted{TurnID: turnID, InputIDs: []string{}}); err != nil {
			reply(protocol.Resolved{}, err)
			return
		}
		a.start(turnID, nil)
		got.Status = protocol.ResolvedStarted
		reply(got, nil)
	})
}
