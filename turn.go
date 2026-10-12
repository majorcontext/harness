package harness

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// Turn follows the outcome of one input. It holds no goroutine; Events and
// Wait read the log of the session, and any number of Turns may follow one
// input. It is safe for concurrent use.
type Turn struct {
	s   *Session
	adm protocol.Admitted
}

// TurnResult is the outcome of an input.
type TurnResult struct {
	InputID string
	// TurnID is empty for a withdrawn input and for a slash command.
	TurnID string
	// StopReason is completed, interrupted, failed, or awaiting_input.
	StopReason protocol.StopReason
	// Cause says why the turn stopped early; it is empty otherwise.
	Cause protocol.Cause
	// Error is the masked failure text.
	Error      string
	ErrorClass protocol.ErrorClass
	// Usage sums the model calls of the turn that the log holds from the
	// admission of the input on.
	Usage protocol.Usage
	// Command is the final status of a typed slash command.
	Command string
	// Withdrawn says that no turn took the input.
	Withdrawn bool
}

// Admitted returns the receipt of the input.
func (t *Turn) Admitted() protocol.Admitted { return t.adm }

// Events yields the events from the admission of the input through the end of
// the turn that took it, or through the withdrawal of the input, or through
// the final status of the command. It yields the ephemeral frames of the
// running turn too. When the session stops first, it ends with Session.Err.
func (t *Turn) Events(ctx context.Context) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if _, err := t.follow(ctx, func(e protocol.Event) bool { return yield(e, nil) }); err != nil {
			yield(protocol.Event{}, err)
		}
	}
}

// Wait returns when the turn that took the input ends, the input is
// withdrawn, or the command finishes. It fails with Session.Err when the
// session stops first, and with the error of ctx.
func (t *Turn) Wait(ctx context.Context) (TurnResult, error) {
	return t.follow(ctx, func(protocol.Event) bool { return true })
}

// Steer admits in as a steer input that joins this turn. It fails with
// ErrTurnMismatch when the turn does not run, also when no turn has taken the
// input yet and when the input is a command.
func (t *Turn) Steer(ctx context.Context, in protocol.Input) (*Turn, error) {
	if in.Delivery == protocol.DeliveryQueue || in.ExpectedTurnID != "" {
		return nil, fmt.Errorf("%w: Steer sets the delivery and the expected turn", ErrInvalidRequest)
	}
	turnID, _, err := t.fate(ctx)
	if err != nil {
		return nil, err
	}
	if turnID == "" {
		return nil, fmt.Errorf("%w: input %s has no running turn", ErrTurnMismatch, t.adm.InputID)
	}
	in.Delivery, in.ExpectedTurnID = protocol.DeliverySteer, turnID
	return t.s.Prompt(ctx, in)
}

// Cancel withdraws the input while it is queued, or interrupts the turn that
// took it. It returns after the turn has ended. A turn that has ended, an
// input that is withdrawn, and a command are no error.
func (t *Turn) Cancel(ctx context.Context) error {
	if t.adm.Command != "" {
		return nil
	}
	for range 2 {
		turnID, withdrawn, err := t.fate(ctx)
		if err != nil || withdrawn {
			return err
		}
		if turnID != "" {
			err := t.s.Interrupt(ctx, protocol.Interrupt{TurnID: turnID})
			if errors.Is(err, ErrTurnMismatch) {
				return nil
			}
			return err
		}
		if err := t.s.Withdraw(ctx, t.adm.InputID); err != nil {
			return err
		}
	}
	return nil
}

// fate returns the turn that took the input, and whether it was withdrawn.
func (t *Turn) fate(ctx context.Context) (turnID string, withdrawn bool, err error) {
	err = t.s.a.Read(ctx, func(st *eventlog.State) { turnID, withdrawn = st.InputFate(t.adm.InputID) })
	return turnID, withdrawn, err
}

// follow reads the events of the session from the admission of the input and
// passes each one to emit. It returns the outcome when it is final, with no
// error, also when emit returns false.
func (t *Turn) follow(ctx context.Context, emit func(protocol.Event) bool) (TurnResult, error) {
	res := TurnResult{InputID: t.adm.InputID}
	if t.adm.Seq == 0 || t.adm.InputID == "" {
		return res, fmt.Errorf("%w: the receipt names no input", ErrInvalidRequest)
	}
	var usage protocol.Usage
	admitted := false
	for e, err := range t.s.Events(ctx, t.adm.Seq-1) {
		if err != nil {
			return res, err
		}
		if e.Ephemeral && !admitted {
			continue
		}
		admitted = true
		done, err := step(e, &res, &usage)
		if err != nil {
			return res, err
		}
		if !emit(e) || done {
			return res, nil
		}
	}
	return res, nil
}

// step folds e into r, the outcome of the input, and reports whether r is final.
func step(e protocol.Event, r *TurnResult, usage *protocol.Usage) (done bool, err error) {
	switch e.Kind {
	case protocol.KindCommandRecorded, protocol.KindInputPromoted, protocol.KindInputWithdrawn,
		protocol.KindTurnStarted, protocol.KindContextMeasured, protocol.KindCompactionApplied, protocol.KindTurnEnded:
	default:
		return false, nil
	}
	p, err := e.Payload()
	if err != nil {
		return false, err
	}
	switch p := p.(type) {
	case *protocol.CommandRecorded:
		if p.InputID == r.InputID && p.Status != protocol.CommandAccepted {
			r.Command, done = p.Status, true
		}
	case *protocol.InputPromoted:
		if p.InputID == r.InputID {
			r.TurnID = p.TurnID
		}
	case *protocol.InputWithdrawn:
		if p.InputID == r.InputID {
			r.Withdrawn, done = true, true
		}
	case *protocol.TurnStarted:
		*usage = protocol.Usage{}
		for _, id := range p.InputIDs {
			if id == r.InputID {
				r.TurnID = p.TurnID
			}
		}
	case *protocol.ContextMeasured:
		*usage = usage.Add(p.Usage)
	case *protocol.CompactionApplied:
		*usage = usage.Add(p.Usage)
	case *protocol.TurnEnded:
		if r.TurnID != "" && p.TurnID == r.TurnID {
			r.StopReason, r.Cause, r.Error, r.ErrorClass, r.Usage, done = p.StopReason, p.Cause, p.Error, p.ErrorClass, *usage, true
		}
	}
	return done, nil
}
