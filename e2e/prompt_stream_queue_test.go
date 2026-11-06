package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

type promptStreamPreCanceled struct{}

func (promptStreamPreCanceled) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	head := s.Snapshot().HeadSeq
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	in := protocol.Input{ID: "canceled", Parts: []protocol.Part{{Type: protocol.PartText, Text: "never"}}}
	if _, err := s.PromptStream(ctx, in); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled prompt error = %v", err)
	}
	in.Delivery = protocol.DeliverySteer
	if _, err := s.PromptStream(t.Context(), in); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("steer prompt error = %v", err)
	}
	in.Delivery = protocol.DeliveryQueue
	in.Source = protocol.SourceTyped
	in.Parts[0].Text = "/status"
	if _, err := s.PromptStream(t.Context(), in); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("control command error = %v", err)
	}
	if s.Snapshot().HeadSeq != head {
		t.Fatal("rejected stream admitted an input")
	}
}

type promptStreamQueued struct{}

func (promptStreamQueued) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	in := protocol.Input{ID: "target", Parts: []protocol.Part{{Type: protocol.PartText, Text: "target"}}}
	stream, err := s.PromptStream(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := s.PromptStream(t.Context(), in); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("repeated queued input error = %v", err)
	}
	r.fake.Release("prior")
	if _, err := stream.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	started, completed := 0, 0
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		switch e.Kind {
		case protocol.KindItemStarted:
			started++
		case protocol.KindItemDelta:
			text.WriteString(decodeEvent[protocol.ItemFrame](t, e).Text)
		case "item.completed":
			completed++
		}
	}
	if text.String() != "answer" || started != 1 || completed != 1 {
		t.Fatalf("queued stream text=%q, started=%d, completed=%d", text.String(), started, completed)
	}
}
