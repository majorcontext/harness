package session

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// Describe returns the protocol view of state.
func Describe(id string, s *eventlog.State) protocol.Session {
	sum, set, u := s.Summary(), s.Settings(), s.Usage()
	v := protocol.Session{
		ID: id, ParentID: sum.ParentID, Origin: sum.Origin, Model: sum.Model,
		Effort: set.Effort, ServiceTier: set.ServiceTier, Status: string(sum.Status),
		Usage:   protocol.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens},
		HeadSeq: sum.HeadSeq, CreatedAt: sum.CreatedAt, UpdatedAt: sum.UpdatedAt,
	}
	if t, ok := s.Turn(); ok && !t.Suspended {
		v.TurnID = t.ID
	}
	for _, in := range s.Queue() {
		v.Queued = append(v.Queued, in.InputID)
	}
	return v
}

// Events yields the durable records after seq, then each record that the
// actor appends, with the ephemeral frames of its turns between them. It
// subscribes before it yields. It loads the view before it drains the
// frames: a frame sent before an append is then never yielded after the
// record. A full subscriber drops frames, never records. It ends with
// ErrNotOwned when the actor stops.
func (a *Actor) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		frames := a.live.subscribe()
		defer a.live.unsubscribe(frames)
		place := func(f protocol.Event) bool {
			return through(ctx, a.cfg.Log, &after, f.Seq, yield) && yield(f, nil)
		}
		for {
			v := a.View()
			for drained := false; !drained; {
				select {
				case f := <-frames:
					if !place(f) {
						return
					}
				default:
					drained = true
				}
			}
			if !through(ctx, a.cfg.Log, &after, v.Session.HeadSeq, yield) {
				return
			}
			if v.Stopped {
				yield(protocol.Event{}, ErrNotOwned)
				return
			}
			select {
			case <-v.changed:
			case f := <-frames:
				if !place(f) {
					return
				}
			case <-ctx.Done():
				yield(protocol.Event{}, ctx.Err())
				return
			}
		}
	}
}

// Stored yields the records in log after seq and ends at seq head.
func Stored(ctx context.Context, log Log, after, head uint64) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) { through(ctx, log, &after, head, yield) }
}

// through yields the records after *after through head and advances
// *after. It reports false when it yielded an error or yield returned false.
func through(ctx context.Context, log Log, after *uint64, head uint64, yield func(protocol.Event, error) bool) bool {
	for *after < head {
		recs, err := log.Read(ctx, *after, int(min(head-*after, page)))
		if err == nil && len(recs) == 0 {
			err = fmt.Errorf("harness: log ends at %d before seq %d", *after, head)
		}
		if err != nil {
			yield(protocol.Event{}, err)
			return false
		}
		for _, r := range recs {
			e, err := event(r)
			if !yield(e, err) || err != nil {
				return false
			}
			*after = r.Seq
		}
	}
	return true
}

func event(r eventlog.Record) (protocol.Event, error) {
	env, err := eventlog.Decode(r.Data)
	if err != nil {
		return protocol.Event{}, err
	}
	data, err := json.Marshal(env.Event)
	return protocol.Event{Seq: env.Seq, Time: env.Time, Kind: env.Event.Kind(), Data: data}, err
}
