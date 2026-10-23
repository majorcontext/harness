package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
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
	delta chan struct{}
	// second is closed at the second item.delta frame, whatever the dedup of steps.
	second chan struct{}
	deltas int
	steps  []string
	ids    map[string]int
}

func newItemFeed() *itemFeed {
	return &itemFeed{done: make(chan struct{}), delta: make(chan struct{}), second: make(chan struct{}), ids: map[string]int{}}
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
	if strings.HasPrefix(s, "delta ") {
		if f.deltas++; f.deltas == 2 {
			close(f.second)
		}
		select {
		case <-f.delta:
		default:
			close(f.delta)
		}
	}
}

// watchItems subscribes to the event stream of a session before its first
// input, and returns when the stream has sent its first frame. With late set,
// it first waits until the earlier stream of the session has carried an
// item.delta, so the new stream starts after an item began streaming. With
// past set as well, it waits for the second item.delta, resumes after the head
// seq of the session, which is past the start of the item, and queues one more
// input. The stream is registered once it carries that durable record, and a
// replay of the kept frames would come before it.
type watchItems struct {
	as         string
	late, past bool
}

func feedKey(id string, late, past bool) string {
	switch {
	case past:
		return id + "#past"
	case late:
		return id + "#late"
	}
	return id
}

func (a watchItems) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	id := r.id(t, a.as)
	feed := newItemFeed()
	if a.late || a.past {
		itemFeedsMu.Lock()
		early := itemFeeds[id]
		itemFeedsMu.Unlock()
		if early == nil {
			t.Fatalf("late watchItems before watchItems for %s", a.as)
		}
		wait := early.delta
		if a.past {
			wait = early.second
		}
		select {
		case <-wait:
		case <-early.done:
			t.Fatalf("the event stream of %s ended before an item.delta", id)
		}
	}
	itemFeedsMu.Lock()
	itemFeeds[feedKey(id, a.late, a.past)] = feed
	itemFeedsMu.Unlock()
	var after uint64
	if a.past {
		after = d.view(t, id).HeadSeq
	}
	subscribed := make(chan struct{})
	go func() {
		defer close(feed.done)
		d.stream(t, id, after, false, func(_ string, ev protocol.Event) bool {
			if !a.past || !ev.Ephemeral {
				select {
				case <-subscribed:
				default:
					close(subscribed)
				}
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
	if a.past {
		d.Enqueue(t, id, "marker")
	}
	select {
	case <-subscribed:
	case <-feed.done:
		t.Fatalf("the event stream of %s ended before its first frame", id)
	}
}

// itemFrames waits for the end of the first turn on the stream that
// watchItems opened, and records its item frames.
type itemFrames struct {
	as         string
	late, past bool
}

func (a itemFrames) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	itemFeedsMu.Lock()
	feed := itemFeeds[feedKey(id, a.late, a.past)]
	itemFeedsMu.Unlock()
	if feed == nil {
		t.Fatalf("itemFrames before watchItems for %s", a.as)
	}
	<-feed.done
	name := a.as
	switch {
	case a.past:
		name += "_past"
	case a.late:
		name += "_late"
	}
	r.record(t, "item_frames", name, callResult{Status: http.StatusOK, Body: map[string]any{"frames": feed.steps}})
}

// TestContractLateSubscriber covers a subscriber that starts while an item streams.
func TestContractLateSubscriber(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name: "a_late_subscriber_receives_the_frames_of_the_streaming_item",
			model: []harnesstest.Step{
				{Name: "slow", Reply: harnesstest.Reply{Text: "partial answer", Block: true}},
			},
			actions: []action{
				create{as: "a"},
				watchItems{as: "a"},
				submit{as: "a", text: "go"},
				watchItems{as: "a", late: true},
				release{step: "slow"},
				waitIdle{as: "a"},
				itemFrames{as: "a"},
				itemFrames{as: "a", late: true},
			},
		},
		{
			name: "a_subscriber_that_resumes_past_the_item_start_receives_no_frame_of_the_item",
			model: []harnesstest.Step{
				{Name: "slow", Reply: harnesstest.Reply{Text: "partial ", Tail: "answer", Block: true, TailBlock: true}},
				{Name: "next", Repeat: true, Reply: harnesstest.Reply{Text: "next answer"}},
			},
			actions: []action{
				create{as: "a"},
				watchItems{as: "a"},
				submit{as: "a", text: "go"},
				watchItems{as: "a", late: true},
				enqueue{as: "a", text: "more"},
				release{step: "slow"},
				watchItems{as: "a", past: true},
				release{step: "slow:tail"},
				waitIdle{as: "a"},
				itemFrames{as: "a", past: true},
			},
		},
		{
			name: "chat_subscriber_past_the_item_start_receives_no_frame_of_the_item",
			chat: true,
			model: []harnesstest.Step{
				{Name: "slow", Reply: harnesstest.Reply{Text: "partial ", Tail: "answer", Block: true, TailBlock: true}},
				{Name: "next", Repeat: true, Reply: harnesstest.Reply{Text: "next answer"}},
			},
			actions: []action{
				create{as: "a"},
				watchItems{as: "a"},
				submit{as: "a", text: "go"},
				watchItems{as: "a", late: true},
				enqueue{as: "a", text: "more"},
				release{step: "slow"},
				watchItems{as: "a", past: true},
				release{step: "slow:tail"},
				waitIdle{as: "a"},
				itemFrames{as: "a", past: true},
			},
		},
	})
}
