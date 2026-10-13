package acp

import (
	"context"
	"encoding/json"

	"github.com/majorcontext/harness/protocol"
)

// turnOwner tracks which prompt waits for each running turn. It returns the
// prompt whose turn e ends, or nil. The pump calls it for every event, in order.
func (h *held) turnOwner(e protocol.Event, st *pumpState) *prompt {
	switch e.Kind {
	case protocol.KindTurnStarted, protocol.KindInputPromoted, protocol.KindTurnEnded:
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
			if pr := h.byInput(id); pr != nil {
				st.owners[p.TurnID] = pr
			}
		}
	case *protocol.InputPromoted:
		if pr := h.byInput(p.InputID); pr != nil {
			st.owners[p.TurnID] = pr
		}
	case *protocol.TurnEnded:
		pr := st.owners[p.TurnID]
		delete(st.owners, p.TurnID)
		return pr
	}
	return nil
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

func (e *sentError) Error() string { return e.err.Error() }
func (e *sentError) Unwrap() error { return e.err }

// AfterSent releases the pump and ends the prompt.
func (e *sentError) AfterSent() { e.release() }
