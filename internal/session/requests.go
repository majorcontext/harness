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

// fromPerson reports whether a person sent in: a typed line, an expanded slash
// command, or an input with no named source, which the admission step stores
// as "user". Only such an input closes an open request. An input of any other
// source waits in the queue until the request resolves.
func fromPerson(in protocol.InputAdmitted) bool {
	switch in.Source {
	case "", "user", protocol.SourceTyped, "command":
		return true
	}
	return false
}

// dismissFrom returns the dismissal of every open request when q holds an
// input from a person, and nothing otherwise.
func (a *Actor) dismissFrom(q []protocol.InputAdmitted) []protocol.Payload {
	if slices.ContainsFunc(q, fromPerson) {
		return a.dismissRequests()
	}
	return nil
}

// heldByRequest reports whether a request is open and q holds no input from a
// person, so no turn starts from q.
func (a *Actor) heldByRequest(q []protocol.InputAdmitted) bool {
	return len(a.state.Requests()) > 0 && !slices.ContainsFunc(q, fromPerson)
}

// Resolve closes request id and returns the seq of its request.resolved
// record. An answer starts a turn that has no input, so the backend that
// asked reads the answer; a dismissal starts none for the request, and the next
// turn tells the backend. A dismissal starts the turn of the queued inputs.
func (a *Actor) Resolve(ctx context.Context, id string, answer json.RawMessage, dismiss bool) (protocol.Resolved, error) {
	return call(ctx, a, func(reply func(protocol.Resolved, error)) {
		i := slices.IndexFunc(a.state.Requests(), func(r protocol.RequestOpened) bool { return r.RequestID == id })
		if i < 0 {
			reply(protocol.Resolved{}, ErrRequestNotPending)
			return
		}
		if !dismiss {
			if err := checkAnswer(a.state.Requests()[i].RequestKind, answer); err != nil {
				reply(protocol.Resolved{}, err)
				return
			}
		}
		ev := protocol.RequestResolved{RequestID: id, Resolution: protocol.ResolutionDismissed}
		if !dismiss {
			ev.Resolution, ev.Answer = protocol.ResolutionAnswered, answer
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
