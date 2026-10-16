package session

import (
	"context"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// usageResetText is the engine context of the input that continues a session
// after a usage limit reset. It does not repeat the last message of the user.
const usageResetText = "[usage limit: The usage limit of the provider has reset. Continue the task from the point where it stopped.]"

// pendingContinue returns the time at which this actor continues the session
// after a usage limit reset. It reports none when the runtime does not serve
// continues; the state alone says what is pending.
func (a *Actor) pendingContinue() (time.Time, bool) {
	at, ok := a.state.ContinueAt()
	return at, ok && a.cfg.ContinueOnReset
}

// continueLater arms the timer of the pending continue. It replaces the timer
// of an earlier continue.
func (a *Actor) continueLater() {
	a.stopContinue()
	at, ok := a.pendingContinue()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(a.cfg.Base)
	a.contStop, a.contArmed = cancel, at
	a.cfg.Go(func() {
		t := time.NewTimer(time.Until(at))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		case <-a.quit:
			return
		}
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) { reply(struct{}{}, a.continueNow(at)) })
	})
}

func (a *Actor) stopContinue() {
	if a.contStop != nil {
		a.contStop()
		a.contStop = nil
	}
	a.contArmed = time.Time{}
}

// rearmContinue arms the timer again when the pending continue changed.
func (a *Actor) rearmContinue() {
	if at, _ := a.pendingContinue(); !at.Equal(a.contArmed) {
		a.continueLater()
	}
}

// cancelContinue appends the cancel of the pending continue, if the state has one.
func (a *Actor) cancelContinue() error {
	at, ok := a.state.ContinueAt()
	if !ok {
		return nil
	}
	return a.append(protocol.ContinueCancelled{ContinueAt: at.Unix()})
}

// continueNow admits the usage_reset input at the front of the queue, and
// starts its turn in the same append when nothing runs. A continue that an input canceled does nothing.
func (a *Actor) continueNow(at time.Time) error {
	if p, ok := a.pendingContinue(); !ok || !p.Equal(at) {
		return nil
	}
	in := protocol.InputAdmitted{InputID: newID("input"), Delivery: protocol.DeliveryQueue, Source: protocol.SourceUsageReset,
		Parts: []protocol.LogPart{{Type: protocol.LogPartEngineContext, Text: usageResetText}}}
	q := append([]protocol.InputAdmitted{in}, a.state.Queue()...)
	if a.run != nil || a.compactDue() || a.heldByRequest(q) {
		if err := a.append(in); err != nil || a.run != nil {
			return err
		}
		return a.next(true)
	}
	id, ids := newID("turn"), a.startInputs(q)
	if err := a.append(in, protocol.TurnStarted{TurnID: id, InputIDs: ids}); err != nil {
		return err
	}
	a.start(id, ids)
	return nil
}

// continueIfDue admits the continue of a log that Open replayed, when its time
// has passed.
func (a *Actor) continueIfDue() error {
	if at, ok := a.pendingContinue(); ok && !at.After(time.Now()) {
		return a.continueNow(at)
	}
	return nil
}
