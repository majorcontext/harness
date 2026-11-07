package session

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

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
}

func (t *turnRun) StepStarted(index int) error {
	_, err := callRun(context.Background(), t.a, func(reply func(struct{}, error)) {
		if t.a.run != t.r {
			reply(struct{}{}, ErrTurnMismatch)
			return
		}
		reply(struct{}{}, t.a.append(protocol.StepStarted{TurnID: t.r.id, Index: index}))
	})
	return err
}

func (t *turnRun) StepEnded(index int, reason string) error {
	_, err := callRun(context.Background(), t.a, func(reply func(struct{}, error)) {
		if t.a.run != t.r {
			reply(struct{}{}, ErrTurnMismatch)
			return
		}
		if err := context.Cause(t.r.step); err != nil {
			reply(struct{}{}, err)
			return
		}
		reply(struct{}{}, t.a.append(protocol.StepEnded{TurnID: t.r.id, Index: index, FinishReason: reason}))
	})
	return err
}

// Item records m under a new ID.
func (t *turnRun) Item(m protocol.LogMessage) error { return t.ItemAs("", m) }

// ItemAs records m under id, or a new ID when id is "".
func (t *turnRun) ItemAs(id string, m protocol.LogMessage) error {
	_, err := callRun(context.Background(), t.a, func(reply func(struct{}, error)) { reply(struct{}{}, t.a.item(t.r, id, m)) })
	return err
}

// Attach stores data under a key that its digest names, so a repeat of the same
// bytes is one blob. The Store holds the blob before the record that names it.
func (t *turnRun) Attach(mediaType string, data []byte) (protocol.LogPart, error) {
	sum := sha256.Sum256(data)
	key := "toolblob-" + hex.EncodeToString(sum[:])
	if err := t.a.cfg.Store.PutBlob(t.a.cfg.Base, key, bytes.NewReader(data)); err != nil {
		return protocol.LogPart{}, err
	}
	return protocol.LogPart{Type: protocol.LogPartBlob, MediaType: mediaType, BlobKey: key, Bytes: len(data)}, nil
}

// Started announces a new item that only ItemAs records.
func (t *turnRun) Started() string {
	id := newID("item")
	t.a.frame(protocol.KindItemStarted, protocol.ItemFrame{ItemID: id, TurnID: t.r.id})
	return id
}

// Delta streams d into the item itemID.
func (t *turnRun) Delta(itemID string, d turn.Delta) {
	t.a.frame(protocol.KindItemDelta, protocol.ItemFrame{ItemID: itemID, TurnID: t.r.id, Type: d.Type, Text: d.Text})
}

// Discard tells subscribers to remove the live item itemID.
func (t *turnRun) Discard(itemID string) {
	t.a.frame(protocol.KindItemDiscarded, protocol.ItemFrame{ItemID: itemID, TurnID: t.r.id})
}

// Alive has nothing to report: the stall watchdog is in the turn loop.
func (*turnRun) Alive() {}

// Status sends f.
func (t *turnRun) Status(f protocol.StatusFrame) {
	f.TurnID = t.r.id
	f.Status = compactionOutcome(f.Status, context.Cause(t.r.ctx))
	t.a.frame(protocol.KindStatus, f)
}

// Telemetry adds the usage of tel to the turn and records its context reading.
func (t *turnRun) Telemetry(tel turn.Telemetry) {
	_, _ = callRun(context.Background(), t.a, func(reply func(struct{}, error)) { reply(struct{}{}, t.a.telemetry(t.r, tel)) })
}

// Steer promotes the queued steer inputs into the turn and returns them.
func (t *turnRun) Steer() ([]protocol.LogMessage, error) {
	return callRun(context.Background(), t.a, func(reply func([]protocol.LogMessage, error)) { reply(t.a.steer(t.r)) })
}

// Settings returns the model and the settings of the session while the turn
// runs, so that a change takes effect at the next model call of the turn.
func (t *turnRun) Settings() (string, protocol.Settings) {
	type held struct {
		model string
		set   protocol.Settings
	}
	h, _ := callRun(context.Background(), t.a, func(reply func(held, error)) {
		if t.a.run == t.r {
			reply(held{t.a.state.Model(), t.a.state.Settings()}, nil)
			return
		}
		reply(held{}, nil)
	})
	return h.model, h.set
}

// Pin pins ns on the actor, which owns the pins, and returns every pin. The
// log records each new pin but the banner, so a new process starts with the
// pins of the process before it, each at its place. The first call of a
// process pins a status of a kind that the log never recorded after the banner,
// as the log alone fixes that place. A later change, and a change of a recorded
// status, follows the history of its call. A pin never falls before an older
// one, so the notices are set in the order of their places, and a notice of the
// same place keeps its order.
func (t *turnRun) Pin(ns []turn.Notice, at int) []turn.Pin {
	pins, _ := callRun(context.Background(), t.a, func(reply func([]turn.Pin, error)) {
		first := !t.a.pinned
		t.a.pinned = true
		placed := slices.Clone(ns)
		if len(placed) > 0 && placed[0].Kind == turn.BannerKind && placed[0].Text != "" && !t.a.pins.Has(turn.BannerKind) {
			b := placed[0]
			t.a.pins.Prepend(turn.Pin{Kind: b.Kind, Text: b.Text, At: b.At})
		}
		known := len(t.a.pins.Pinned())
		for i := range placed {
			if first && placed[i].At == 0 && !t.a.pins.Has(placed[i].Kind) {
				placed[i].At = t.a.state.BannerAt()
			}
			placed[i].At = cmp.Or(placed[i].At, at)
		}
		slices.SortStableFunc(placed, func(x, y turn.Notice) int { return cmp.Compare(x.At, y.At) })
		for _, n := range placed {
			t.a.pins.Set(n.Kind, n.Text, n.Cleared, n.At)
		}
		pinned := t.a.pins.Pinned()
		var recorded []protocol.Payload
		for _, p := range pinned[known:] {
			if p.Kind != turn.BannerKind {
				recorded = append(recorded, protocol.PinRecorded{Notice: p.Kind, Text: p.Text, At: p.At})
			}
		}
		if len(recorded) > 0 {
			_ = t.a.append(recorded...)
		}
		reply(pinned, nil)
	})
	return pins
}

// restoredPins returns the pins that the log of s records.
func restoredPins(s *eventlog.State) turn.Pins {
	var ps turn.Pins
	for _, p := range s.Pins() {
		ps.Add(turn.Pin{Kind: p.Notice, Text: p.Text, At: p.At})
	}
	return ps
}

// Ended ends the turn by the cause of its stop.
func (t *turnRun) Ended(runErr error) {
	_, _ = callRun(context.Background(), t.a, func(reply func(struct{}, error)) {
		t.a.ended(t.r, runErr)
		if t.r.span != nil {
			t.r.span.End()
		}
		reply(struct{}{}, nil)
	})
}

// CompactTurn folds the turns before the newest Config.KeepTurns, else the
// running turn before its newest round trip, into a summary while the turn
// runs, and returns the new history. ok is false when nothing folds. When ctx
// ends, it appends nothing.
func (t *turnRun) CompactTurn(ctx context.Context) ([]protocol.LogMessage, bool, error) {
	return t.compactRunning(ctx, false)
}

// AutoCompact compacts as CompactTurn does when the context passes the
// threshold. A summary that fails or comes back empty leaves the turn on its
// history, with no error.
func (t *turnRun) AutoCompact(ctx context.Context) ([]protocol.LogMessage, bool, error) {
	due, err := callRun(ctx, t.a, func(reply func(bool, error)) {
		if t.a.run != t.r {
			reply(false, ErrTurnMismatch)
			return
		}
		reply(t.a.compactDue(), nil)
	})
	if err != nil || !due {
		return nil, false, err
	}
	return t.compactRunning(ctx, true)
}

// compactRunning compacts for the running turn. With soft, a failed summary
// is no error unless ctx ended.
func (t *turnRun) compactRunning(ctx context.Context, soft bool) ([]protocol.LogMessage, bool, error) {
	a, r := t.a, t.r
	type folding struct {
		req   turn.Request
		src   turn.Source
		c     protocol.CompactionApplied
		ok    bool
		metas []toolresult.Meta
	}
	f, err := callRun(ctx, a, func(reply func(folding, error)) {
		if a.run != r {
			reply(folding{}, ErrTurnMismatch)
			return
		}
		req, c, ok := a.fold(r.id, a.cfg.KeepTurns)
		reply(folding{req, a.source(r), c, ok, a.retained()}, nil)
	})
	if err != nil || !f.ok {
		return nil, false, err
	}
	summary, usage, err := a.summarize(ctx, f.req, f.src)
	if err != nil {
		_, _ = callRun(context.Background(), a, func(reply func(struct{}, error)) {
			if a.run == r {
				recordErr := a.recordUsage(usage)
				a.latched = recordErr == nil && ctx.Err() == nil && (soft || errors.Is(err, turn.ErrEmptySummary))
				reply(struct{}{}, recordErr)
				return
			}
			reply(struct{}{}, nil)
		})
		if soft && ctx.Err() == nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	f.c.Summary, f.c.Index, f.c.Usage = summary, a.resultIndex(f.metas), usage
	h, err := callRun(ctx, a, func(reply func([]protocol.LogMessage, error)) {
		if a.run != r || ctx.Err() != nil {
			reply(nil, cmp.Or(context.Cause(ctx), error(ErrTurnMismatch)))
			return
		}
		err := a.append(f.c)
		a.latched = err == nil
		h := a.state.ModelHistory()
		reply(h, err)
	})
	return h, err == nil, err
}
