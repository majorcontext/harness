package acp

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/majorcontext/harness/protocol"
)

// turnOwner tracks which prompts wait for each running turn. It returns the
// prompts whose last event e is: the end of their turn, the final status of
// their command, or the withdrawal of their input. The pump calls it for every
// event, in order.
func (h *held) turnOwner(e protocol.Event, st *pumpState) []*prompt {
	switch e.Kind {
	case protocol.KindTurnStarted, protocol.KindInputPromoted, protocol.KindTurnEnded,
		protocol.KindCommandRecorded, protocol.KindInputWithdrawn:
	default:
		return nil
	}
	p, err := e.Payload()
	if err != nil {
		return nil
	}
	switch p := p.(type) {
	case *protocol.TurnStarted:
		for _, id := range p.InputIDs {
			st.own(p.TurnID, h.byInput(id))
		}
	case *protocol.InputPromoted:
		st.own(p.TurnID, h.byInput(p.InputID))
	case *protocol.TurnEnded:
		owners := st.owners[p.TurnID]
		delete(st.owners, p.TurnID)
		return owners
	case *protocol.CommandRecorded:
		if p.Status != protocol.CommandAccepted {
			return one(h.byInput(p.InputID))
		}
	case *protocol.InputWithdrawn:
		return one(h.byInput(p.InputID))
	}
	return nil
}

func one(p *prompt) []*prompt {
	if p == nil {
		return nil
	}
	return []*prompt{p}
}

// own records that p waits for the turn. A turn can take several inputs, so a
// turn can have several prompts; p joins them once.
func (st *pumpState) own(turn string, p *prompt) {
	if p != nil && !slices.Contains(st.owners[turn], p) {
		st.owners[turn] = append(st.owners[turn], p)
	}
}

func (h *held) byInput(id string) *prompt {
	h.mu.Lock()
	defer h.mu.Unlock()
	for p := range h.prompts {
		if p.input == id {
			return p
		}
	}
	return nil
}

// reached waits until the pump has sent every update of the turn of p and holds
// at its end, or has ended.
func (h *held) reached(ctx context.Context, p *prompt) {
	for {
		h.mu.Lock()
		ready, wake := p.reached || h.ended, h.wake
		h.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
	}
}

// release lets the pump go on past the end of the turn of p.
func (h *held) release(p *prompt) {
	p.freeOnce.Do(func() { close(p.free) })
}

// handOff marks the answer of p as queued by the rpc layer: the pump stays at
// the end of the turn until AfterSent runs, so no update of a later turn
// precedes the answer.
func (h *held) handOff(p *prompt, v any, err error) (any, error) {
	h.mu.Lock()
	p.handed = true
	h.mu.Unlock()
	if err != nil {
		return nil, &sentError{err: err, release: func() { h.release(p); h.drop(p) }}
	}
	return &sentResult{v: v, release: func() { h.release(p); h.drop(p) }}, nil
}

// sentResult is the result of a session/prompt. The rpc layer calls AfterSent
// after it queued the response.
type sentResult struct {
	v       any
	release func()
}

// MarshalJSON encodes the result as the wrapped value.
func (r *sentResult) MarshalJSON() ([]byte, error) { return json.Marshal(r.v) }

// AfterSent releases the pump and ends the prompt.
func (r *sentResult) AfterSent() { r.release() }

// sentError is the error of a session/prompt, with the same release.
type sentError struct {
	err     error
	release func()
}

// Error returns the text of the wrapped error.
func (e *sentError) Error() string { return e.err.Error() }

// Unwrap returns the wrapped error.
func (e *sentError) Unwrap() error { return e.err }

// AfterSent releases the pump and ends the prompt.
func (e *sentError) AfterSent() { e.release() }
