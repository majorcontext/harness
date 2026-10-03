package harness

import (
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/session"
	"github.com/majorcontext/harness/protocol"
)

// Session is a session that this runtime runs.
type Session struct {
	a *session.Actor
}

// View returns the session as of its last durable record.
func (s *Session) View() protocol.Session { return detach(s.a.View().Session) }

func detach(s protocol.Session) protocol.Session {
	s.Queued = slices.Clone(s.Queued)
	return s
}

// Submit admits an input. It starts a turn when none runs and queues the
// input otherwise. A steer input joins the running turn at its next item
// when the backend accepts steering. A repeated input ID returns the
// original receipt.
func (s *Session) Submit(ctx context.Context, in protocol.Input) (protocol.Admitted, error) {
	ev, err := admission(in)
	if err != nil {
		return protocol.Admitted{}, err
	}
	seq, err := s.a.Submit(ctx, ev, in.ExpectedTurnID)
	if err != nil {
		return protocol.Admitted{}, err
	}
	return protocol.Admitted{InputID: in.ID, Seq: seq}, nil
}

func admission(in protocol.Input) (eventlog.InputAdmitted, error) {
	ev := eventlog.InputAdmitted{InputID: in.ID, Delivery: eventlog.Delivery(in.Delivery), Source: in.Source}
	if ev.Delivery == "" {
		ev.Delivery = eventlog.DeliveryQueue
	}
	if ev.Source == "" {
		ev.Source = "user"
	}
	switch {
	case in.ID == "":
		return ev, fmt.Errorf("%w: input id is empty", ErrInvalidRequest)
	case len(in.Parts) == 0:
		return ev, fmt.Errorf("%w: input %s has no parts", ErrInvalidRequest, in.ID)
	case ev.Delivery != eventlog.DeliveryQueue && ev.Delivery != eventlog.DeliverySteer:
		return ev, fmt.Errorf("%w: input %s has delivery %q", ErrInvalidRequest, in.ID, in.Delivery)
	}
	for _, p := range in.Parts {
		if p.Type != protocol.PartText {
			return ev, fmt.Errorf("%w: input %s has part type %q", ErrInvalidRequest, in.ID, p.Type)
		}
		ev.Parts = append(ev.Parts, eventlog.Part{Type: eventlog.PartText, Text: p.Text})
	}
	return ev, nil
}

// Interrupt stops the running turn and returns after it has ended. The
// partial turn stays in the log, and the next queued input starts.
func (s *Session) Interrupt(ctx context.Context, req protocol.Interrupt) error {
	return s.a.Interrupt(ctx, req.TurnID)
}

// Events yields the durable events after seq, then each new one as it is
// appended. It ends with ErrSessionNotOwned when the session stops here.
func (s *Session) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return s.a.Events(ctx, after)
}

// Release hands the session off: the running turn suspends at an item
// boundary for the next owner, and the ownership is released.
func (s *Session) Release(ctx context.Context) error { return s.a.Release(ctx) }

// View is a read-only session that no runtime needs to own.
type View struct {
	st    Store
	id    string
	state protocol.Session
}

// OpenView reads a session from st without owning it: no Acquire, no appends.
func OpenView(ctx context.Context, st Store, id string) (*View, error) {
	s, err := session.Load(ctx, id, storeLog{st, id})
	if err != nil {
		return nil, err
	}
	return &View{st: st, id: id, state: session.Describe(id, s)}, nil
}

// Session returns the session as of OpenView.
func (v *View) Session() protocol.Session { return detach(v.state) }

// Events yields the events after seq and ends at the head that OpenView read.
func (v *View) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return session.Stored(ctx, storeLog{v.st, v.id}, after, v.state.HeadSeq)
}
