package e2e

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

var (
	itemFeedsMu sync.Mutex
	itemFeeds   = map[string]*itemFeed{}
)

// itemFeed is the item frames that the event stream of a session carried from
// its start to the end of its first turn.
type itemFeed struct {
	done  chan struct{}
	steps []string
	ids   map[string]int
}

func (f *itemFeed) name(id string) string {
	if _, ok := f.ids[id]; !ok {
		f.ids[id] = len(f.ids) + 1
	}
	return fmt.Sprintf("item#%d", f.ids[id])
}

func (f *itemFeed) add(s string) {
	if n := len(f.steps); n == 0 || f.steps[n-1] != s {
		f.steps = append(f.steps, s)
	}
}

// watchItems subscribes to the event stream of a session before its first
// input, and returns when the stream has sent its first frame.
type watchItems struct{ as string }

func (a watchItems) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	id := r.id(t, a.as)
	feed := &itemFeed{done: make(chan struct{}), ids: map[string]int{}}
	itemFeedsMu.Lock()
	itemFeeds[id] = feed
	itemFeedsMu.Unlock()
	subscribed := make(chan struct{})
	go func() {
		defer close(feed.done)
		d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
			select {
			case <-subscribed:
			default:
				close(subscribed)
			}
			switch ev.Kind {
			case protocol.KindItemStarted:
				feed.add("started " + feed.name(decodeEvent[protocol.ItemFrame](t, ev).ItemID))
			case protocol.KindItemDelta:
				feed.add("delta " + feed.name(decodeEvent[protocol.ItemFrame](t, ev).ItemID))
			case protocol.KindItemDiscarded:
				feed.add("discarded " + feed.name(decodeEvent[protocol.ItemFrame](t, ev).ItemID))
			case "item.completed":
				it := decodeEvent[logItem](t, ev)
				feed.add(fmt.Sprintf("completed %s %s", feed.name(it.ItemID), it.Message.Role))
			case "turn.ended":
				return true
			}
			return false
		})
	}()
	select {
	case <-subscribed:
	case <-feed.done:
		t.Fatalf("the event stream of %s ended before its first frame", id)
	}
}

// itemFrames waits for the end of the first turn on the stream that
// watchItems opened, and records its item frames.
type itemFrames struct{ as string }

func (a itemFrames) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	itemFeedsMu.Lock()
	feed := itemFeeds[id]
	itemFeedsMu.Unlock()
	if feed == nil {
		t.Fatalf("itemFrames before watchItems for %s", a.as)
	}
	<-feed.done
	r.record(t, "item_frames", a.as, callResult{Status: http.StatusOK, Body: map[string]any{"frames": feed.steps}})
}
