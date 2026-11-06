package e2e

import (
	"slices"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

type promptStreamRetryFrames struct{}

func (promptStreamRetryFrames) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "retry", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	feed := newItemFeed()
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case protocol.KindItemStarted, protocol.KindItemDelta, protocol.KindItemDiscarded:
			f := decodeEvent[protocol.ItemFrame](t, e)
			feed.add(e.Kind + " " + feed.name(f.ItemID) + " " + f.Text)
		case "item.completed":
			item := decodeEvent[protocol.ItemCompleted](t, e)
			feed.add(e.Kind + " " + feed.name(item.ItemID))
		}
	}
	want := []string{
		"item.started item#1 ", "item.delta item#1 partial", "item.discarded item#1 ",
		"item.started item#2 ", "item.delta item#2 answer", "item.completed item#2",
	}
	if !slices.Equal(feed.steps, want) {
		t.Fatalf("retry lifecycle = %q, want %q", feed.steps, want)
	}
}
