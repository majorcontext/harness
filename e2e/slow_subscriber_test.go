package e2e

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var slowSubscriberRows = map[string]runtimeRow{
	"a_slow_embedded_subscriber_reconciles_dropped_text_from_completed_items": runtimeOnly("embedded-subscriber-held"),
}

func TestContractSlowEmbeddedSubscriber(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "a_slow_embedded_subscriber_reconciles_dropped_text_from_completed_items",
		model: []harnesstest.Step{{Name: "answer", Reply: harnesstest.Reply{
			Text: strings.Repeat("answer ", 512), TextChunkRunes: 7,
		}}},
		actions: []action{create{as: "a"}, slowEmbeddedSubscriber{as: "a"}},
	}})
}

type slowEmbeddedSubscriber struct{ as string }

type slowTextFeed struct {
	cursor    uint64
	prefix    map[string]string
	text      map[string]string
	durable   []protocol.Event
	completed int
	dropped   bool
}

func (f *slowTextFeed) apply(t *testing.T, e protocol.Event) bool {
	if !e.Ephemeral && e.Seq > f.cursor {
		f.durable = append(f.durable, e)
	}
	switch e.Kind {
	case protocol.KindItemStarted:
		item := decodeEvent[protocol.ItemFrame](t, e)
		f.prefix[item.ItemID], f.text[item.ItemID] = "", ""
	case protocol.KindItemDelta:
		item := decodeEvent[protocol.ItemFrame](t, e)
		if item.Type == "text" {
			f.prefix[item.ItemID] += item.Text
			f.text[item.ItemID] += item.Text
		}
	case "item.completed":
		item := decodeEvent[protocol.ItemCompleted](t, e)
		var full strings.Builder
		for _, p := range item.Message.Parts {
			if p.Type == protocol.PartText {
				full.WriteString(p.Text)
			}
		}
		f.dropped = f.dropped || f.prefix[item.ItemID] != full.String()
		f.text[item.ItemID] = full.String()
		f.completed++
	}
	return e.Kind == "turn.ended"
}

func (a slowEmbeddedSubscriber) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := d.rt.Open(ctx, r.id(t, a.as))
	if err != nil {
		t.Fatal(err)
	}
	ready, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	feed := slowTextFeed{prefix: map[string]string{}, text: map[string]string{}}
	go func() {
		defer close(done)
		for e, err := range s.Events(ctx, 0) {
			if err != nil {
				t.Error(err)
				return
			}
			if feed.cursor == 0 {
				feed.cursor = e.Seq
				close(ready)
				select {
				case <-resume:
				case <-ctx.Done():
					return
				}
			}
			if feed.apply(t, e) {
				return
			}
		}
	}()
	<-ready
	d.Submit(t, s.ID(), "go")
	d.WaitIdle(t, s.ID())
	close(resume)
	<-done
	if !feed.dropped || feed.completed != 1 || len(feed.text) != 1 {
		t.Fatalf("overflow=%v, completed=%d, items=%d", feed.dropped, feed.completed, len(feed.text))
	}
	for id, text := range feed.text {
		if text != strings.Repeat("answer ", 512) || len(feed.prefix[id]) == 0 {
			t.Fatalf("item %s: reconciled bytes=%d, delta bytes=%d", id, len(text), len(feed.prefix[id]))
		}
	}
	var replay []protocol.Event
	for e, err := range harness.ReadEvents(ctx, store.NewDiskStore(d.store), s.ID(), feed.cursor) {
		if err != nil {
			t.Fatal(err)
		}
		replay = append(replay, e)
	}
	if !reflect.DeepEqual(replay, feed.durable) {
		t.Fatal("slow subscriber missed or reordered durable records after its cursor")
	}
}
