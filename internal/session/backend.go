package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/majorcontext/harness/internal/eventlog"
)

// State returns the newest state blob of backend, or nil when it saved none.
// It reads the blob outside the actor.
func (t *turnRun) State(backend string) ([]byte, error) {
	a := t.a
	key, err := call(context.Background(), a, func(reply func(string, error)) {
		if a.run != t.r {
			reply("", ErrTurnMismatch)
			return
		}
		reply(a.state.BackendState(backend), nil)
	})
	if err != nil || key == "" {
		return nil, err
	}
	rc, err := a.cfg.Blobs.GetBlob(a.cfg.Base, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// SaveState writes blob under the key of this ownership, then appends
// backend.state. A key per ownership keeps a fenced owner from overwriting
// the blob of the next one, and keeps one blob per owner.
func (t *turnRun) SaveState(backend string, blob []byte) error {
	a := t.a
	key := fmt.Sprintf("%s-%d", backend, a.fenced)
	if err := a.onRun(t.r, func() error { return nil }); err != nil {
		return err
	}
	if err := a.cfg.Blobs.PutBlob(a.cfg.Base, key, bytes.NewReader(blob)); err != nil {
		return err
	}
	return a.onRun(t.r, func() error { return a.append(eventlog.BackendState{Backend: backend, BlobKey: key}) })
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
