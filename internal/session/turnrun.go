package session

import (
	"cmp"
	"context"

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
	// item is the ID of the item that the deltas since the last Item
	// build. Only the turn goroutine reads it.
	item string
}

// Item records m under the ID that its deltas announced, or a new ID.
func (t *turnRun) Item(m eventlog.Message) error {
	id := t.item
	t.item = ""
	_, err := call(context.Background(), t.a, func(reply func(struct{}, error)) { reply(struct{}{}, t.a.item(t.r, id, m)) })
	return err
}

// Delta streams d into the next item. The backend item ID is not used: one
// harness item can join several backend items, such as reasoning and text.
func (t *turnRun) Delta(_ string, d turn.Delta) {
	if t.item == "" {
		t.item = newID("item")
		t.a.frame(protocol.KindItemStarted, protocol.ItemFrame{ItemID: t.item, TurnID: t.r.id})
	}
	t.a.frame(protocol.KindItemDelta, protocol.ItemFrame{ItemID: t.item, TurnID: t.r.id, Type: d.Type, Text: d.Text})
}

// Alive has nothing to report: the stall watchdog is in the turn loop.
func (*turnRun) Alive() {}

// Status sends f. A retrying frame abandons the item that the failed
// attempt streamed.
func (t *turnRun) Status(f protocol.StatusFrame) {
	if f.Status == protocol.StatusRetrying {
		t.item = ""
	}
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
		return nil, false, err
	}
	f.c.Summary, f.c.Usage = a.indexed(summary, f.metas), usage
	h, err := call(ctx, a, func(reply func([]eventlog.Message, error)) {
		if a.run != r || ctx.Err() != nil {
			reply(nil, cmp.Or(context.Cause(ctx), ErrTurnMismatch))
			return
		}
		err := a.append(f.c)
		reply(a.state.History(), err)
	})
	return h, err == nil, err
}
