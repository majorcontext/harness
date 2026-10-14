package acp

import (
	"context"
	"encoding/json"
	"sync"
)

// ticket orders the requests that open or close one session. A request takes its
// ticket in wire order, in Admit, and runs when the tickets before it for the
// same session are released. So a load that follows a close on the wire waits
// for the release, and two loads of one session run one after the other.
type ticket struct {
	a    *agent
	id   string
	prev <-chan struct{}
	done chan struct{}
	once sync.Once
}

type ticketKey struct{}

// reserve takes the next ticket for session id.
func (a *agent) reserve(id string) *ticket {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tails == nil {
		a.tails = map[string]chan struct{}{}
	}
	t := &ticket{a: a, id: id, prev: a.tails[id], done: make(chan struct{})}
	a.tails[id] = t.done
	return t
}

// ticketFor returns the ticket that Admit took for the request, or takes one.
func (a *agent) ticketFor(ctx context.Context, id string) *ticket {
	if t, ok := ctx.Value(ticketKey{}).(*ticket); ok && t.id == id {
		return t
	}
	return a.reserve(id)
}

// wait blocks until the tickets before t are released.
func (t *ticket) wait(ctx context.Context) error {
	if t.prev == nil {
		return nil
	}
	select {
	case <-t.prev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release lets the next request for the session run. It is safe to call twice.
func (t *ticket) release() {
	t.once.Do(func() {
		t.a.mu.Lock()
		if t.a.tails[t.id] == t.done {
			delete(t.a.tails, t.id)
		}
		t.a.mu.Unlock()
		close(t.done)
	})
}

// releasing is the answer of a request that holds a ticket. Serve calls
// AfterSent after it queued the answer, so the next request for the session
// sends its updates behind this answer.
type releasing struct {
	result any
	t      *ticket
}

// MarshalJSON writes the wrapped result.
func (r releasing) MarshalJSON() ([]byte, error) { return json.Marshal(r.result) }

// AfterSent runs the follow-up of the result, then releases the ticket, so the
// next request for the session runs behind the updates that the follow-up sends.
func (r releasing) AfterSent() {
	defer r.t.release()
	if f, ok := r.result.(interface{ AfterSent() }); ok {
		f.AfterSent()
	}
}

// ticketed runs f after the ticket of session id is due. A failure releases the
// ticket at once. A success releases it when the answer is sent.
func (a *agent) ticketed(ctx context.Context, id string, f func() (any, error)) (any, error) {
	t := a.ticketFor(ctx, id)
	if err := t.wait(ctx); err != nil {
		t.release()
		return nil, err
	}
	res, err := f()
	if err != nil {
		t.release()
		return nil, err
	}
	return releasing{result: res, t: t}, nil
}

// sessionIDOf returns the sessionId of the params of a request, or "".
func sessionIDOf(params json.RawMessage) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.SessionID
}
