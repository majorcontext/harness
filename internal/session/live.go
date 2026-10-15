package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/majorcontext/harness/protocol"
)

// liveBuffer bounds the ephemeral frames that wait for one subscriber.
const liveBuffer = 256

// liveItemBytes bounds the item.delta data that live keeps for the item that
// streams. Past it, a later subscriber misses further deltas of that item.
const liveItemBytes = 256 << 10

// live fans ephemeral frames out to the subscribers of Events. A send to a
// full subscriber drops the frame; durable events come from the log. mu
// orders a frame's head read and send against publish. item holds the
// item.started frame and the item.delta frames of the item that streams, so
// a subscriber that registers later starts with them.
type live struct {
	mu       sync.Mutex
	subs     map[chan protocol.Event]struct{}
	itemID   string
	item     []protocol.Event
	itemSize int
}

// subscribe registers a subscriber. Its channel already holds the frames of
// the item that streams, in the order sent.
func (l *live) subscribe() chan protocol.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := make(chan protocol.Event, liveBuffer+len(l.item))
	for _, e := range l.item {
		ch <- e
	}
	if l.subs == nil {
		l.subs = map[chan protocol.Event]struct{}{}
	}
	l.subs[ch] = struct{}{}
	return ch
}

func (l *live) unsubscribe(ch chan protocol.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subs, ch)
}

// send needs l.mu.
func (l *live) send(e protocol.Event) {
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// remember keeps e when it belongs to the item that streams. A new item
// replaces the kept frames, and a discard drops them. It needs l.mu.
func (l *live) remember(e protocol.Event, f protocol.ItemFrame) {
	switch e.Kind {
	case protocol.KindItemDiscarded:
		l.forget()
	case protocol.KindItemStarted, protocol.KindItemDelta:
		if f.ItemID != l.itemID {
			l.forget()
			l.itemID = f.ItemID
		}
		if l.itemSize+len(e.Data) <= liveItemBytes {
			l.item = append(l.item, e)
			l.itemSize += len(e.Data)
		}
	}
}

// forget drops the kept frames. It needs l.mu.
func (l *live) forget() {
	l.itemID, l.item, l.itemSize = "", nil, 0
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
	e := protocol.Event{Seq: a.View().Session.HeadSeq, Time: time.Now(), Kind: kind, Data: d, Ephemeral: true}
	if f, ok := data.(protocol.ItemFrame); ok {
		a.live.remember(e, f)
	}
	a.live.send(e)
}
