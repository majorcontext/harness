package e2e

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

// liveFeed is what the event stream of a session carried from its start to
// the end of its first turn: each delta, and each completed item.
type liveFeed struct {
	done  chan struct{}
	steps []liveStep
}

type liveStep struct {
	itemID string
	delta  string
	text   string
	item   logItem
}

var (
	liveFeedsMu sync.Mutex
	liveFeeds   = map[string]*liveFeed{}
)

// claudeWatchLive subscribes to the event stream of a session before its first
// input, and returns when the stream has sent its first frame: the stream
// subscribes to live frames before it replays the log, so a frame proves the
// subscription. A delta is live only; a subscriber that comes late misses it.
type claudeWatchLive struct{ as string }

func (a claudeWatchLive) run(t *testing.T, r *run) {
	t.Helper()
	d := claudeDriverOf(t, r).laneHost.(*runtimeDriver)
	id := r.id(t, a.as)
	feed := &liveFeed{done: make(chan struct{})}
	liveFeedsMu.Lock()
	liveFeeds[id] = feed
	liveFeedsMu.Unlock()
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
			case protocol.KindItemDelta:
				f := decodeEvent[protocol.ItemFrame](t, ev)
				feed.steps = append(feed.steps, liveStep{itemID: f.ItemID, delta: f.Type, text: f.Text})
			case "item.completed":
				feed.steps = append(feed.steps, liveStep{item: decodeEvent[logItem](t, ev)})
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

// claudeLive waits for the end of the first turn on the stream that
// claudeWatchLive opened, and checks that the deltas of each live item add up
// to the text of the item that it completes as, and that a main thread item
// came in pieces before it completed. A type that the item does not hold is
// not checked: a thread that a subagent frame cut in the middle of a block
// sends that block again, as the first delta of the item that records it. It records the stream, and the live
// items that never completed.
type claudeLive struct{ as string }

func (a claudeLive) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	liveFeedsMu.Lock()
	feed := liveFeeds[id]
	liveFeedsMu.Unlock()
	if feed == nil {
		t.Fatalf("claudeLive before claudeWatchLive for %s", a.as)
	}
	<-feed.done
	var out []any
	deltas := map[string]map[string][]string{}
	for _, s := range feed.steps {
		if s.delta != "" {
			if deltas[s.itemID] == nil {
				deltas[s.itemID] = map[string][]string{}
			}
			deltas[s.itemID][s.delta] = append(deltas[s.itemID][s.delta], s.text)
			continue
		}
		msg := s.item.Message
		if msg.Role != "assistant" {
			out = append(out, map[string]any{"item": msg.Role})
			continue
		}
		step := map[string]any{"item": "assistant"}
		if msg.ParentCallID != "" {
			step["parent"] = msg.ParentCallID
		}
		for _, typ := range []string{"reasoning", "text"} {
			final := ""
			for _, p := range msg.Parts {
				if p.Type == typ {
					final += p.Text
				}
			}
			if final == "" {
				continue
			}
			pieces := deltas[s.item.ItemID][typ]
			if got := strings.Join(pieces, ""); got != final {
				t.Errorf("deltas of %s before the item %v = %q, want the text of the item %q", typ, step, got, final)
			}
			if msg.ParentCallID == "" && len(pieces) < 2 {
				t.Errorf("%s of the item came in %d delta before the item, want it in pieces", typ, len(pieces))
			}
			step[typ] = map[string]any{"text": final, "pieces": pieces}
		}
		for _, p := range msg.Parts {
			if p.Type == "tool_call" {
				step["tool_call"] = p.Name
			}
		}
		out = append(out, step)
		delete(deltas, s.item.ItemID)
	}
	body := map[string]any{"stream": out}
	var unfinished []any
	for _, steps := range feed.steps {
		if steps.delta == "" {
			continue
		}
		if left, ok := deltas[steps.itemID]; ok {
			var texts []string
			for _, typ := range []string{"reasoning", "text"} {
				if len(left[typ]) > 0 {
					texts = append(texts, typ+":"+strings.Join(left[typ], ""))
				}
			}
			unfinished = append(unfinished, texts)
			delete(deltas, steps.itemID)
		}
	}
	if len(unfinished) > 0 {
		body["unfinished"] = unfinished
	}
	r.record(t, "live_stream", a.as, callResult{Status: http.StatusOK, Body: body})
}
