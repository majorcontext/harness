package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// State returns the state that backend saved, read in order from its chain
// of chunks, or a zero Snapshot when it saved none. It reads the blobs
// outside the actor.
func (t *turnRun) State(backend string) (turn.Snapshot, error) {
	a := t.a
	chain, err := call(context.Background(), a, func(reply func(eventlog.BackendChain, error)) {
		if a.run != t.r {
			reply(eventlog.BackendChain{}, ErrTurnMismatch)
			return
		}
		c, _ := a.state.BackendState(backend)
		reply(c, nil)
	})
	if err != nil {
		return turn.Snapshot{}, err
	}
	return snapshotOf(chain, func(key string) ([]byte, error) {
		rc, err := a.cfg.Store.GetBlob(a.cfg.Base, key)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(rc)
	})
}

type saveBase struct {
	chain  eventlog.BackendChain
	exists bool
	next   int
}

// SaveState writes the entries of s that the chain of backend lacks as
// chunk blobs, then appends one backend.state record for each chunk. A chunk
// key holds the backend, the fence of this ownership, and an index that no
// earlier chunk of this ownership used, so a fenced owner never overwrites a
// chunk of another owner and no chunk is written twice.
func (t *turnRun) SaveState(backend string, s turn.Snapshot) error {
	a := t.a
	base, err := call(context.Background(), a, func(reply func(saveBase, error)) {
		if a.run != t.r {
			reply(saveBase{}, ErrTurnMismatch)
			return
		}
		c, ok := a.state.BackendState(backend)
		reply(saveBase{c, ok, a.chunks[backend]}, nil)
	})
	if err != nil {
		return err
	}
	plan, err := planSave(backend, a.fenced, base.next, base.chain, base.exists, s)
	if err != nil || len(plan.events) == 0 {
		return err
	}
	for _, b := range plan.blobs {
		if err := a.cfg.Store.PutBlob(a.cfg.Base, b.key, bytes.NewReader(b.data)); err != nil {
			return err
		}
	}
	return a.onRun(t.r, func() error {
		if a.chunks == nil {
			a.chunks = map[string]int{}
		}
		a.chunks[backend] = base.next + len(plan.blobs)
		return a.append(plan.events...)
	})
}

// Compacted records a compaction by the backend of every record since the
// previous compaction.
func (t *turnRun) Compacted(summary string) error {
	a := t.a
	return a.onRun(t.r, func() error {
		from := uint64(1)
		if c, ok := a.state.Compaction(); ok {
			from = c.ToSeq + 1
		}
		return a.append(eventlog.CompactionApplied{FromSeq: from, ToSeq: a.state.Head(), Summary: summary, ByBackend: true})
	})
}

// onRun runs f in the actor while r runs.
func (a *Actor) onRun(r *running, f func() error) error {
	_, err := call(context.Background(), a, func(reply func(struct{}, error)) {
		if a.run != r {
			reply(struct{}{}, ErrTurnMismatch)
			return
		}
		reply(struct{}{}, f())
	})
	return err
}

// Ask opens a request on the open tool call callID of the turn.
func (t *turnRun) Ask(callID, kind string, payload json.RawMessage) error {
	a := t.a
	return a.onRun(t.r, func() error {
		i := slices.IndexFunc(a.state.OpenToolCalls(), func(c eventlog.OpenToolCall) bool { return c.CallID == callID })
		if i < 0 {
			return fmt.Errorf("session: tool call %s is not open", callID)
		}
		return a.append(eventlog.RequestOpened{RequestID: callID, ItemID: a.state.OpenToolCalls()[i].ItemID, RequestKind: kind, Payload: payload})
	})
}

// Resolution returns the record that closed request id, if one did.
func (t *turnRun) Resolution(id string) (eventlog.RequestResolved, bool) {
	type found struct {
		r  eventlog.RequestResolved
		ok bool
	}
	f, _ := call(context.Background(), t.a, func(reply func(found, error)) {
		r, ok := t.a.state.Resolution(id)
		reply(found{r, ok}, nil)
	})
	return f.r, f.ok
}
