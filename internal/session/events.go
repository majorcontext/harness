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
// actor appends. The view holds the head and the signal of the next append
// together, so no record falls between the read and the wait. It ends with
// ErrNotOwned when the actor stops.
func (a *Actor) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return events(ctx, a.cfg.Log, after, a.View)
}

// Stored yields the records in log after seq and ends at the head.
func Stored(ctx context.Context, log Log, after uint64) iter.Seq2[protocol.Event, error] {
	return events(ctx, log, after, nil)
}

func events(ctx context.Context, log Log, after uint64, view func() *View) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		for {
			limit := page
			if view != nil {
				v := view()
				if v.Session.HeadSeq <= after {
					if v.Stopped {
						yield(protocol.Event{}, ErrNotOwned)
						return
					}
					select {
					case <-v.changed:
						continue
					case <-ctx.Done():
						yield(protocol.Event{}, ctx.Err())
						return
					}
				}
				limit = int(min(v.Session.HeadSeq-after, page))
			}
			recs, err := log.Read(ctx, after, limit)
			if err == nil && len(recs) == 0 && view != nil {
				err = fmt.Errorf("harness: log ends at %d before the published head", after)
			}
			if err != nil {
				yield(protocol.Event{}, err)
				return
			}
			for _, r := range recs {
				e, err := event(r)
				if !yield(e, err) || err != nil {
					return
				}
				after = r.Seq
			}
			if view == nil && len(recs) < limit {
				return
			}
		}
	}
}

func event(r eventlog.Record) (protocol.Event, error) {
	env, err := eventlog.Decode(r.Data)
	if err != nil {
		return protocol.Event{}, err
	}
	data, err := json.Marshal(env.Event)
	return protocol.Event{Seq: env.Seq, Time: env.Time, Kind: env.Event.Kind(), Data: data}, err
}
