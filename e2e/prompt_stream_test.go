package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

const (
	promptStreamPrefixDeltas = 257
	promptStreamDeltaRunes   = 8
)

func promptStreamText() string {
	var text strings.Builder
	for i := range promptStreamPrefixDeltas {
		fmt.Fprintf(&text, "%07d|", i)
	}
	text.WriteString(strings.Repeat("x", 256<<10))
	return text.String()
}

func (a slowEmbeddedSubscriber) losslessRun(t *testing.T, r *run, d *runtimeDriver) {
	t.Helper()
	s, err := d.rt.Open(t.Context(), r.id(t, a.as))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "stream", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	defer r.fake.Release("answer")
	if !r.fake.AwaitBlocked("answer", waitBound) {
		t.Fatal("model did not reach the held prefix")
	}
	r.fake.Release("answer")
	if _, err := stream.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	feed := slowTextFeed{cursor: stream.Admitted().Seq - 1, prefix: map[string]*strings.Builder{}, text: map[string]string{}}
	var head uint64
	started, deltas := 0, 0
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Ephemeral {
			if e.Seq != head || feed.completed != 0 {
				t.Fatalf("%s at %d after head %d and %d completed items", e.Kind, e.Seq, head, feed.completed)
			}
		} else {
			head = e.Seq
		}
		if e.Kind == protocol.KindItemStarted {
			started++
		}
		if e.Kind == protocol.KindItemDelta {
			deltas++
		}
		feed.apply(t, e)
	}
	if feed.dropped || feed.completed != 1 || started != 1 || deltas != promptStreamPrefixDeltas+1 || deltas <= 256 {
		t.Fatalf("loss=%v, completed=%d, started=%d, deltas=%d", feed.dropped, feed.completed, started, deltas)
	}
	for id, text := range feed.text {
		if text != promptStreamText() || feed.preview(id).String() != text || len(text) <= 256<<10 {
			t.Fatalf("item %s: delta bytes=%d, completed bytes=%d", id, feed.preview(id).Len(), len(text))
		}
	}
}

var promptStreamRows = map[string]runtimeRow{
	"prompt_stream_keeps_retry_item_lifecycle_in_order":                   runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_held_iterator_cancellation_releases_a_held_subscriber": runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_iterator_cancellation_releases_a_held_subscriber":      runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_pre_canceled_context_admits_no_input":                  runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_queues_behind_a_running_turn_without_its_frames":       runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_context_cancellation_releases_a_held_subscriber":       runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_close_releases_a_held_subscriber":                      runtimeOnly("embedded-subscriber-held"),
	"prompt_stream_break_releases_a_held_subscriber":                      runtimeOnly("embedded-subscriber-held"),
}

func TestContractPromptStreamLifecycle(t *testing.T) {
	var rows []scenario
	for _, mode := range []string{"context_cancellation", "iterator_cancellation", "held_iterator_cancellation", "close", "break"} {
		rows = append(rows, scenario{
			name:    "prompt_stream_" + mode + "_releases_a_held_subscriber",
			model:   []harnesstest.Step{{Name: "answer", Repeat: true, Reply: harnesstest.Reply{Text: "answer", Block: true}}},
			actions: []action{create{as: "a"}, promptStreamLifecycle{mode: mode}},
		})
	}
	rows = append(rows,
		scenario{
			name: "prompt_stream_keeps_retry_item_lifecycle_in_order",
			model: []harnesstest.Step{
				{Name: "cut", Reply: harnesstest.Reply{Text: "partial", StopReason: "max_tokens"}},
				{Name: "whole", Reply: harnesstest.Reply{Text: "answer"}},
			},
			actions: []action{create{as: "a"}, promptStreamRetryFrames{}},
		},
		scenario{name: "prompt_stream_pre_canceled_context_admits_no_input", actions: []action{create{as: "a"}, promptStreamPreCanceled{}}},
		scenario{
			name: "prompt_stream_queues_behind_a_running_turn_without_its_frames",
			model: []harnesstest.Step{
				{Name: "prior", Reply: harnesstest.Reply{Text: "prior ", Tail: "end", Block: true}},
				{Name: "target", Reply: harnesstest.Reply{Text: "answer"}},
			},
			actions: []action{create{as: "a"}, watchItems{as: "a"}, submit{as: "a", text: "prior"}, watchItems{as: "a", late: true}, promptStreamQueued{}},
		},
	)
	runScenarios(t, rows)
}

type promptStreamLifecycle struct{ mode string }

func (a promptStreamLifecycle) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*runtimeDriver)
	s, err := d.rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := s.PromptStream(ctx, protocol.Input{ID: "stream", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	readCtx := t.Context()
	switch a.mode {
	case "held_iterator_cancellation":
		holdCanceledPromptStream(t, r, stream)
	case "iterator_cancellation":
		var stop context.CancelFunc
		readCtx, stop = context.WithCancel(readCtx)
		stop()
	case "context_cancellation":
		if !r.fake.AwaitBlocked("answer", waitBound) {
			t.Fatal("model did not reach its blocked reply")
		}
		cancel()
		if !r.fake.AwaitCanceled("answer", waitBound) {
			t.Fatal("prompt context did not cancel the model request")
		}
	case "close":
		stream.Close()
	case "break":
		for e, err := range stream.Events(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind == protocol.KindItemDelta {
				break
			}
		}
	}
	if a.mode != "break" && a.mode != "held_iterator_cancellation" {
		var ended error
		for _, err := range stream.Events(readCtx) {
			ended = err
		}
		if !errors.Is(ended, context.Canceled) {
			t.Fatalf("closed stream error = %v", ended)
		}
	}
	r.fake.Release("answer")
	result, err := stream.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a.mode == "context_cancellation" && result.StopReason != protocol.StopInterrupted {
		t.Fatalf("prompt context cancellation stop reason = %s, want interrupted", result.StopReason)
	}
	stream.Close()
	assertNextPromptStream(t, s)
}

func assertNextPromptStream(t *testing.T, s *harness.Session) {
	t.Helper()
	in := protocol.Input{ID: "next", Parts: []protocol.Part{{Type: protocol.PartText, Text: "again"}}}
	next, err := s.PromptStream(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	var text strings.Builder
	for e, err := range next.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == protocol.KindItemDelta {
			text.WriteString(decodeEvent[protocol.ItemFrame](t, e).Text)
		}
	}
	if text.String() != "answer" {
		t.Fatalf("next turn text = %q", text.String())
	}
	if _, err := s.PromptStream(t.Context(), in); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Fatalf("repeated completed input error = %v", err)
	}
	in.Parts[0].Text = "changed"
	if _, err := s.PromptStream(t.Context(), in); !errors.Is(err, harness.ErrInputConflict) {
		t.Fatalf("conflicting input error = %v", err)
	}
	var repeated error
	for _, err := range next.Events(t.Context()) {
		repeated = err
	}
	if !errors.Is(repeated, harness.ErrInvalidRequest) {
		t.Fatalf("second Events error = %v", repeated)
	}
}
