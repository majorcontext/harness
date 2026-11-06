package session

import (
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// liveBuffer bounds the ephemeral frames that wait for one subscriber.
const liveBuffer = 256

// liveItemBytes bounds the item.delta data that live keeps for the item that
// streams. At the first delta past it, live keeps no more frames of that
// item, so the kept deltas are a prefix.
const liveItemBytes = 256 << 10

// live fans ephemeral frames out to subscribers. A bounded subscriber drops
// frames when full; a lossless subscriber keeps an unbounded FIFO. Durable
// events come from the log. mu orders frames, drains, and published heads.
// item holds a bounded preview for subscribers that register during an item.
type live struct {
	mu       sync.Mutex
	subs     map[*liveSubscriber]struct{}
	itemID   string
	item     []protocol.Event
	itemSize int
	full     bool
	stopped  bool
}

type liveSubscriber struct {
	frames   []protocol.Event
	ready    chan struct{}
	lossless bool
	closed   bool
}

func (l *live) subscribe(lossless bool) (*liveSubscriber, []protocol.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &liveSubscriber{ready: make(chan struct{}, 1), lossless: lossless}
	if !l.stopped {
		if l.subs == nil {
			l.subs = map[*liveSubscriber]struct{}{}
		}
		l.subs[s] = struct{}{}
	}
	return s, slices.Clone(l.item)
}

func (l *live) unsubscribe(s *liveSubscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subs, s)
	s.frames = nil
	if !s.closed {
		s.closed = true
		close(s.ready)
	}
}

// send needs l.mu and never waits for a consumer.
func (l *live) send(e protocol.Event) {
	for s := range l.subs {
		if !s.lossless && len(s.frames) >= liveBuffer {
			continue
		}
		s.frames = append(s.frames, e)
		select {
		case s.ready <- struct{}{}:
		default:
		}
	}
}

func (a *Actor) drain(s *liveSubscriber) (*View, []protocol.Event) {
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	v, frames := a.View(), s.frames
	s.frames = nil
	select {
	case <-s.ready:
	default:
	}
	return v, frames
}

// remember keeps e when it belongs to the item that streams. A new item
// replaces the kept frames, and a discard drops them. It needs l.mu.
func (l *live) remember(e protocol.Event, f protocol.ItemFrame) {
	if l.stopped {
		return
	}
	switch e.Kind {
	case protocol.KindItemDiscarded:
		l.forget()
	case protocol.KindItemStarted, protocol.KindItemDelta:
		if f.ItemID != l.itemID {
			l.forget()
			l.itemID = f.ItemID
		}
		if l.full || l.itemSize+len(e.Data) > liveItemBytes {
			l.full = true
			return
		}
		l.item = append(l.item, e)
		l.itemSize += len(e.Data)
	}
}

// forget drops the kept frames. It needs l.mu.
func (l *live) forget() {
	l.itemID, l.item, l.itemSize, l.full = "", nil, 0, false
}

// stop drops the kept frames and keeps no more, because a frame that is still
// in flight must not leave a partial item behind. It needs l.mu.
func (l *live) stop() {
	l.forget()
	clear(l.subs)
	l.stopped = true
}

// settle drops the kept frames once the log holds the item that streamed, or
// the turn that streamed it ends. It needs l.mu.
func (l *live) settle(events []protocol.Payload) {
	for _, p := range events {
		switch e := p.(type) {
		case protocol.ItemCompleted:
			if e.ItemID == l.itemID {
				l.forget()
			}
		case protocol.TurnEnded, protocol.TurnSuspended:
			l.forget()
		}
	}
}

// frame sends an ephemeral frame placed after the last durable record.
// The turn goroutine calls it, so a frame of an item always precedes the
// item.completed record of that item.
func (a *Actor) frame(kind string, data any) {
	d, err := json.Marshal(data)
	if err != nil {
		return
	}
	a.live.mu.Lock()
	defer a.live.mu.Unlock()
	if a.live.stopped {
		return
	}
	e := protocol.Event{Seq: a.View().Session.HeadSeq, Time: time.Now(), Kind: kind, Data: d, Ephemeral: true}
	if f, ok := data.(protocol.ItemFrame); ok {
		a.live.remember(e, f)
	}
	a.live.send(e)
}
