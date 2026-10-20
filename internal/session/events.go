package session

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// contextWindow is the window of the session model, or the window of the
// reading c when the model reports none. A model switch thus replaces the
// window of an older reading.
func contextWindow(model int, c protocol.ContextMeasured) int64 {
	return cmp.Or(int64(model), c.Window)
}

// Describe returns the protocol view of state. window is the context window
// of the session model, or 0 when it is not known, and estimated says that
// window is the default for a model that nothing states. With no window, the
// newest reading supplies the window and its estimated flag.
func Describe(id string, s *eventlog.State, window int, estimated bool) protocol.Session {
	sum, set, p := s.Summary(), s.Settings(), s.Progress()
	v := protocol.Session{
		ID: id, ParentID: sum.ParentID, Agent: s.Agent(), Origin: sum.Origin, WorkDir: sum.WorkDir, Model: sum.Model, Title: sum.Title,
		Effort: set.Effort, ServiceTier: set.ServiceTier, Status: p.Status, TurnID: p.TurnID, Queued: p.Queued,
		Goal: p.Goal, LastTurn: p.LastTurn, Usage: s.Usage(),
		HeadSeq: sum.HeadSeq, CreatedAt: sum.CreatedAt, UpdatedAt: sum.UpdatedAt,
	}
	c := s.Context()
	if c.Tokens == 0 && c.Source == "" {
		c = protocol.ContextMeasured{}
	}
	if window == 0 {
		estimated = c.WindowEstimated
	}
	v.Context = protocol.Context{Tokens: c.Tokens, Window: contextWindow(window, c), WindowEstimated: estimated && contextWindow(window, c) > 0}
	v.CompactionCount = s.CompactionCount()
	if sub := s.SubscriptionUsage(); sub != nil {
		v.SubscriptionUsage = subscriptionView(sub)
		if v.SubscriptionUsage.CapturedAt == 0 {
			v.SubscriptionUsage.CapturedAt = time.Now().Unix()
		}
	}
	return v
}

func subscriptionView(u *protocol.SubscriptionUsage) *protocol.SubscriptionUsage {
	v := &protocol.SubscriptionUsage{Provider: u.Provider, Plan: u.Plan, CapturedAt: u.CapturedAt, SessionCostUSD: u.SessionCostUSD,
		Windows: make([]protocol.SubscriptionUsageWindow, len(u.Windows))}
	for i, w := range u.Windows {
		v.Windows[i] = protocol.SubscriptionUsageWindow(w)
	}
	if o := u.Overage; o != nil {
		v.Overage = &protocol.SubscriptionOverage{InUse: o.InUse, Status: o.Status, ResetsAt: o.ResetsAt}
	}
	return v
}

// Events yields the durable records after seq, then each record that the
// actor appends, with the ephemeral frames of its turns between them. It
// subscribes before it yields. It first yields the frames of the item that
// streams, all of them or none: none when the item started before after. It loads the view before it drains the
// frames: a frame sent before an append is then never yielded after the
// record. A full subscriber drops frames, never records. When the actor
// stops, it waits for Done and ends with Err.
func (a *Actor) Events(ctx context.Context, after uint64) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		frames, kept := a.live.subscribe()
		defer a.live.unsubscribe(frames)
		place := func(f protocol.Event) bool {
			return through(ctx, a.cfg.Store, &after, f.Seq, yield) && yield(f, nil)
		}
		if len(kept) > 0 && kept[0].Seq >= after {
			for _, f := range kept {
				if !place(f) {
					return
				}
			}
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
			if !through(ctx, a.cfg.Store, &after, v.Session.HeadSeq, yield) {
				return
			}
			if v.Stopped {
				select {
				case <-a.done:
					yield(protocol.Event{}, a.cause)
				case <-ctx.Done():
					yield(protocol.Event{}, ctx.Err())
				}
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
	if env.Seq != r.Seq {
		return protocol.Event{}, fmt.Errorf("%w: record %d holds envelope seq %d", eventlog.ErrSeq, r.Seq, env.Seq)
	}
	data, err := json.Marshal(env.Event)
	return protocol.Event{Seq: env.Seq, Time: env.Time, Kind: env.Event.Kind(), Data: data}, err
}
