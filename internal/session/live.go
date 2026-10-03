package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// liveBuffer bounds the ephemeral frames that wait for one subscriber.
const liveBuffer = 256

// live fans ephemeral frames out to the subscribers of Events. A send to a
// full subscriber drops the frame; durable events come from the log. mu
// orders a frame's head read and send against publish.
type live struct {
	mu   sync.Mutex
	subs map[chan protocol.Event]struct{}
}

func (l *live) subscribe() chan protocol.Event {
	ch := make(chan protocol.Event, liveBuffer)
	l.mu.Lock()
	defer l.mu.Unlock()
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
	a.live.send(protocol.Event{Seq: a.View().Session.HeadSeq, Time: time.Now(), Kind: kind, Data: d, Ephemeral: true})
}

// Started announces a new item of turnID and returns its ID.
func (a *Actor) Started(turnID string) string {
	id := newID("item")
	a.frame(protocol.KindItemStarted, protocol.ItemFrame{ItemID: id, TurnID: turnID})
	return id
}

// Delta sends a piece of item itemID of turnID.
func (a *Actor) Delta(turnID, itemID string, d turn.Delta) {
	a.frame(protocol.KindItemDelta, protocol.ItemFrame{ItemID: itemID, TurnID: turnID, Type: d.Type, Text: d.Text})
}

// Status sends the status of turnID.
func (a *Actor) Status(turnID string, f protocol.StatusFrame) {
	f.TurnID = turnID
	a.frame(protocol.KindStatus, f)
}
