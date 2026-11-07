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
	"a_slow_embedded_subscriber_receives_every_text_delta_in_order":           runtimeOnly("embedded-subscriber-held"),
	"a_slow_embedded_subscriber_reconciles_dropped_text_from_completed_items": runtimeOnly("embedded-subscriber-held"),
}

func TestContractSlowEmbeddedSubscriber(t *testing.T) {
	var rows []scenario
	for _, row := range []struct {
		name     string
		lossless bool
	}{
		{"a_slow_embedded_subscriber_reconciles_dropped_text_from_completed_items", false},
		{"a_slow_embedded_subscriber_receives_every_text_delta_in_order", true},
	} {
		answer := harnesstest.Reply{Text: strings.Repeat("answer ", 257), TextChunkRunes: 7}
		if row.lossless {
			text := promptStreamText()
			prefix := promptStreamPrefixDeltas * promptStreamDeltaRunes
			answer = harnesstest.Reply{Text: text[:prefix], Tail: text[prefix:], TextChunkRunes: promptStreamDeltaRunes, Block: true}
		}
		rows = append(rows, scenario{
			name:    row.name,
			model:   []harnesstest.Step{{Name: "answer", Reply: answer}},
			actions: []action{create{as: "a"}, slowEmbeddedSubscriber{as: "a", lossless: row.lossless}},
		})
	}
	runScenarios(t, rows)
}

type slowEmbeddedSubscriber struct {
	as       string
	lossless bool
}

type slowTextFeed struct {
	cursor    uint64
	prefix    map[string]*strings.Builder
	text      map[string]string
	durable   []protocol.Event
	completed int
	dropped   bool
}

func (f *slowTextFeed) preview(id string) *strings.Builder {
	if f.prefix[id] == nil {
		f.prefix[id] = new(strings.Builder)
	}
	return f.prefix[id]
}

func (f *slowTextFeed) apply(t *testing.T, e protocol.Event) bool {
	if !e.Ephemeral && e.Seq > f.cursor {
		f.durable = append(f.durable, e)
	}
	switch e.Kind {
	case protocol.KindItemStarted:
		item := decodeEvent[protocol.ItemFrame](t, e)
		f.prefix[item.ItemID] = new(strings.Builder)
	case protocol.KindItemDelta:
		item := decodeEvent[protocol.ItemFrame](t, e)
		if item.Type == "text" {
			f.preview(item.ItemID).WriteString(item.Text)
		}
	case "item.completed":
		item := decodeEvent[protocol.ItemCompleted](t, e)
		var full strings.Builder
		for _, p := range item.Message.Parts {
			if p.Type == protocol.PartText {
				full.WriteString(p.Text)
			}
		}
		f.dropped = f.dropped || f.preview(item.ItemID).String() != full.String()
		f.text[item.ItemID] = full.String()
		f.completed++
	}
	return e.Kind == "turn.ended"
}

func (a slowEmbeddedSubscriber) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	if a.lossless {
		a.losslessRun(t, r, d)
		return
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := d.rt.Open(ctx, r.id(t, a.as))
	if err != nil {
		t.Fatal(err)
	}
	ready, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	feed := slowTextFeed{prefix: map[string]*strings.Builder{}, text: map[string]string{}}
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
	if feed.dropped == a.lossless || feed.completed != 1 || len(feed.text) != 1 {
		t.Fatalf("overflow=%v, completed=%d, items=%d", feed.dropped, feed.completed, len(feed.text))
	}
	for id, text := range feed.text {
		if text != strings.Repeat("answer ", 257) || feed.preview(id).Len() == 0 {
			t.Fatalf("item %s: reconciled bytes=%d, delta bytes=%d", id, len(text), feed.preview(id).Len())
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
