package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
)

// ErrRequestNotPending reports a request that is not open.
var ErrRequestNotPending = errors.New("harness: request not pending")

// ErrBadAnswer reports an answer that the kind of its request does not take.
var ErrBadAnswer = errors.New("harness: bad answer")

// checkAnswer accepts for a question only a non-empty map of text.
func checkAnswer(kind string, answer json.RawMessage) error {
	if kind != eventlog.RequestQuestion {
		return nil
	}
	var choices map[string]string
	if err := json.Unmarshal(answer, &choices); err != nil || len(choices) == 0 {
		return fmt.Errorf("%w: a question takes a map of questions to text", ErrBadAnswer)
	}
	return nil
}

// questions reports whether the turn that starts now may ask the user a
// question: the embedder answers them, and a child or a goal supervised
// session has no user who watches the turn.
func (a *Actor) questions() bool {
	g, _ := a.state.Goal()
	return a.cfg.AskUserQuestion && a.state.Summary().ParentID == "" && g.State != eventlog.GoalActive
}

// Resolve closes request id. An answer starts a turn that has no input, so
// the backend that asked reads the answer; a dismissal starts none, and the
// next turn tells the backend.
func (a *Actor) Resolve(ctx context.Context, id string, answer json.RawMessage, dismiss bool) error {
	_, err := call(ctx, a, func(reply func(struct{}, error)) {
		i := slices.IndexFunc(a.state.Requests(), func(r eventlog.RequestOpened) bool { return r.RequestID == id })
		if i < 0 {
			reply(struct{}{}, ErrRequestNotPending)
			return
		}
		if !dismiss {
			if err := checkAnswer(a.state.Requests()[i].RequestKind, answer); err != nil {
				reply(struct{}{}, err)
				return
			}
		}
		ev := eventlog.RequestResolved{RequestID: id, Resolution: eventlog.ResolutionDismissed}
		if !dismiss {
			ev.Resolution, ev.Answer = eventlog.ResolutionAnswered, answer
		}
		if dismiss {
			reply(struct{}{}, a.append(ev))
			return
		}
		if a.run != nil {
			reply(struct{}{}, ErrBusy)
			return
		}
		turnID := newID("turn")
		if err := a.append(ev, eventlog.TurnStarted{TurnID: turnID, InputIDs: []string{}}); err != nil {
			reply(struct{}{}, err)
			return
		}
		a.start(turnID, nil)
		reply(struct{}{}, nil)
	})
	return err
}
