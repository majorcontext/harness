package session

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// turnRun is the turn.Turn of one running turn. Each durable change runs in
// the actor, and fails with ErrTurnMismatch after the turn stops being the
// run of the actor.
type turnRun struct {
	a *Actor
	r *running
	// item is the ID that Started minted for the next Item. Only the turn
	// goroutine reads it.
	item string
}

// Item records m under the ID that Started minted for it, or a new ID.
func (t *turnRun) Item(m eventlog.Message) error {
	id := t.item
	t.item = ""
	return t.ItemAs(id, m)
}

// ItemAs records m under id, or a new ID when id is "".
func (t *turnRun) ItemAs(id string, m eventlog.Message) error {
	_, err := call(context.Background(), t.a, func(reply func(struct{}, error)) { reply(struct{}{}, t.a.item(t.r, id, m)) })
	return err
}

// Attach stores data under a key that its digest names, so a repeat of the same
// bytes is one blob. The Store holds the blob before the record that names it.
func (t *turnRun) Attach(mediaType string, data []byte) (eventlog.Part, error) {
	sum := sha256.Sum256(data)
	key := "toolblob-" + hex.EncodeToString(sum[:])
	if err := t.a.cfg.Store.PutBlob(t.a.cfg.Base, key, bytes.NewReader(data)); err != nil {
		return eventlog.Part{}, err
	}
	return eventlog.Part{Type: eventlog.PartBlob, MediaType: mediaType, BlobKey: key, Bytes: len(data)}, nil
}

// Started announces a new item and makes it the item of the next Item.
func (t *turnRun) Started() string {
	t.item = t.Announce()
	return t.item
}

// Announce announces a new item that only ItemAs records.
func (t *turnRun) Announce() string {
	id := newID("item")
	t.a.frame(protocol.KindItemStarted, protocol.ItemFrame{ItemID: id, TurnID: t.r.id})
	return id
}

// Delta streams d into the item itemID.
func (t *turnRun) Delta(itemID string, d turn.Delta) {
	t.a.frame(protocol.KindItemDelta, protocol.ItemFrame{ItemID: itemID, TurnID: t.r.id, Type: d.Type, Text: d.Text})
}

// Alive has nothing to report: the stall watchdog is in the turn loop.
func (*turnRun) Alive() {}

// Status sends f.
func (t *turnRun) Status(f protocol.StatusFrame) {
	f.TurnID = t.r.id
	t.a.frame(protocol.KindStatus, f)
}

// Telemetry adds the usage of tel to the turn and records its context reading.
func (t *turnRun) Telemetry(tel turn.Telemetry) {
	_, _ = call(context.Background(), t.a, func(reply func(struct{}, error)) { reply(struct{}{}, t.a.telemetry(t.r, tel)) })
}

// Steer promotes the queued steer inputs into the turn and returns them.
func (t *turnRun) Steer() ([]eventlog.Message, error) {
	return call(context.Background(), t.a, func(reply func([]eventlog.Message, error)) { reply(t.a.steer(t.r)) })
}

// Settings returns the model and the settings of the session while the turn
// runs, so that a change takes effect at the next model call of the turn.
func (t *turnRun) Settings() (string, eventlog.Settings) {
	type held struct {
		model string
		set   eventlog.Settings
	}
	h, _ := call(context.Background(), t.a, func(reply func(held, error)) {
		if t.a.run == t.r {
			reply(held{t.a.state.Model(), t.a.state.Settings()}, nil)
			return
		}
		reply(held{}, nil)
	})
	return h.model, h.set
}

// Pin pins ns on the actor, which owns the pins, and returns every pin.
func (t *turnRun) Pin(ns []turn.Notice, at int) []turn.Pin {
	pins, _ := call(context.Background(), t.a, func(reply func([]turn.Pin, error)) {
		for _, n := range ns {
			t.a.pins.Set(n.Kind, n.Text, n.Cleared, at)
		}
		reply(t.a.pins.Pinned(), nil)
	})
	return pins
}

// Ended ends the turn by the cause of its stop.
func (t *turnRun) Ended(runErr error) {
	_, _ = call(context.Background(), t.a, func(reply func(struct{}, error)) {
		t.a.ended(t.r, runErr)
		reply(struct{}{}, nil)
	})
}

// CompactTurn folds the turns before the newest Config.KeepTurns into a
// summary while the turn runs, and returns the new history. ok is false when
// no turn can fold. When ctx ends, it appends nothing.
func (t *turnRun) CompactTurn(ctx context.Context) ([]eventlog.Message, bool, error) {
	a, r := t.a, t.r
	type folding struct {
		req   turn.Request
		c     eventlog.CompactionApplied
		ok    bool
		metas []toolresult.Meta
	}
	f, err := call(ctx, a, func(reply func(folding, error)) {
		if a.run != r {
			reply(folding{}, ErrTurnMismatch)
			return
		}
		req, c, ok := a.fold(r.id, a.cfg.KeepTurns)
		reply(folding{req, c, ok, a.retained()}, nil)
	})
	if err != nil || !f.ok {
		return nil, false, err
	}
	summary, usage, err := turn.Summarize(ctx, a.cfg.Backend, f.req, a.cfg.Limits.Idle)
	if err != nil {
		_, _ = call(context.Background(), a, func(reply func(struct{}, error)) {
			if a.run == r {
				reply(struct{}{}, a.recordUsage(usage))
				return
			}
			reply(struct{}{}, nil)
		})
		return nil, false, err
	}
	f.c.Summary, f.c.Usage = a.indexed(summary, f.metas), usage
	h, err := call(ctx, a, func(reply func([]eventlog.Message, error)) {
		if a.run != r || ctx.Err() != nil {
			reply(nil, cmp.Or(context.Cause(ctx), ErrTurnMismatch))
			return
		}
		err := a.append(f.c)
		h := a.state.ModelHistory()
		reply(h, err)
	})
	return h, err == nil, err
}
