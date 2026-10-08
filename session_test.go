package harness_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// watch reads the events of s after seq after, submits in at the first
// event, and returns every event up to the turn.ended after seq end. With
// stall, it reads nothing more until the bubble is idle.
func watch(t *testing.T, s *harness.Session, after, end uint64, in protocol.Input, stall bool) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for e, err := range s.Events(bg, after) {
		if err != nil {
			t.Fatal(err)
		}
		if out = append(out, e); len(out) == 1 {
			if _, err := s.Submit(bg, in); err != nil {
				t.Fatal(err)
			}
			if stall {
				synctest.Wait()
			}
		}
		if e.Kind == "turn.ended" && e.Seq > end {
			break
		}
	}
	return out
}

// render prints each event as one line, with each item ID as i1, i2, ...
// in order of first use. An ephemeral frame starts with "~".
func render(events []protocol.Event) []string {
	ids, out := map[string]string{}, []string{}
	for _, e := range events {
		var d struct {
			ItemID             string `json:"item_id"`
			Type, Text, Status string
			Attempt            int
		}
		_ = json.Unmarshal(e.Data, &d)
		if d.ItemID != "" && ids[d.ItemID] == "" {
			ids[d.ItemID] = fmt.Sprintf("i%d", len(ids)+1)
		}
		f := fmt.Sprintf("%d %s %s", e.Seq, e.Kind, ids[d.ItemID])
		if e.Ephemeral {
			f = fmt.Sprintf("~%s %s %s %s %d", f, d.Type, d.Text, d.Status, d.Attempt)
		}
		out = append(out, strings.Join(strings.Fields(strings.TrimSuffix(f, " 0")), " "))
	}
	return out
}

func wantEvents(t *testing.T, got []protocol.Event, want ...string) {
	t.Helper()
	if g := render(got); !slices.Equal(g, want) {
		t.Fatalf("events =\n%s\nwant\n%s", strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
}

func TestEventsStreamCodexDeltas(t *testing.T) {
	s := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"hi": {Reasoning: []string{"plan"}}}},
		harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
	r, _, _ := codexRuntime(t, s, false, false, "")
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, watch(t, sess, 1, 0, text("a", "hi"), false), "2 owner.acquired", "3 input.admitted", "4 turn.started",
		"~4 item.started i1", "~4 item.delta i1 reasoning plan", "~4 item.delta i1 text hello",
		"5 context.measured", "6 item.completed i1", "7 turn.ended")
}

func deltas(n int, text string) func(turn.Sink) error {
	return func(out turn.Sink) error {
		for range n {
			out.Delta("backend-id", turn.Delta{Type: eventlog.PartText, Text: text})
		}
		return out.Item(say(strings.Repeat(text, n)))
	}
}

func TestEventsLiveFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flaky := func(out turn.Sink) error {
			out.Delta("backend-id", turn.Delta{Type: eventlog.PartText, Text: "z"})
			return fmt.Errorf("%w: flaky", turn.ErrRetryable)
		}
		r := runtime(t, harness.NewMemStore(), &scripted{steps: []func(turn.Sink) error{flaky, deltas(2, "x"), deltas(1000, "y")}})
		s := create(t, r)
		wantEvents(t, watch(t, s, 1, 0, text("a", "one"), false), "2 owner.acquired", "3 input.admitted", "4 turn.started",
			"~4 item.started i1", "~4 item.delta i1 text z", "~4 status retrying 1", "~4 status running",
			"~4 item.started i2", "~4 item.delta i2 text x", "~4 item.delta i2 text x", "5 item.completed i2", "6 turn.ended")

		got := watch(t, s, 4, 6, text("b", "two"), true)
		durable := slices.DeleteFunc(slices.Clone(got), func(e protocol.Event) bool { return e.Ephemeral })
		wantEvents(t, durable, "5 item.completed i1", "6 turn.ended", "7 input.admitted", "8 turn.started", "9 item.completed i2", "10 turn.ended")
		wantEvents(t, got[4:6], "~8 item.started i1", "~8 item.delta i1 text y")
		if live := len(got) - len(durable); live >= 1001 {
			t.Fatalf("a stalled reader got all %d frames, want deltas dropped", live)
		}
		closeRuntime(t, r)
	})
}

type windowed struct{ *scripted }

func (windowed) Capabilities(string) turn.Capabilities { return turn.Capabilities{ContextWindow: 1000} }

func compactingRuntime(t *testing.T, summary func(turn.Sink) error) *harness.Session {
	t.Helper()
	full := func(out turn.Sink) error {
		out.Telemetry(turn.Telemetry{Context: eventlog.ContextMeasured{Tokens: 900, Window: 1000, Source: "test"}})
		return out.Item(say("full"))
	}
	reply := func(out turn.Sink) error { return out.Item(say("ok")) }
	b := windowed{&scripted{steps: []func(turn.Sink) error{reply, full, summary, reply}}}
	r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Config: config.Config{CompactionKeepTurns: 1}}, b)
	if err != nil {
		t.Fatal(err)
	}
	s := create(t, r)
	converse(t, s, "one", "two")
	t.Cleanup(func() { closeRuntime(t, r) })
	return s
}

func TestEventsAutoCompactionFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := compactingRuntime(t, func(out turn.Sink) error { return out.Item(say("gist")) })
		head := s.View().HeadSeq
		wantEvents(t, watch(t, s, head-1, head, text("c", "three"), false)[1:], "12 input.admitted", "~12 status compacting", "13 compaction.applied", "~13 status idle",
			"14 turn.started", "15 item.completed i1", "16 turn.ended")
	})
}

func TestEventsFailedCompactionFrames(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := compactingRuntime(t, func(turn.Sink) error { return errors.New("summary failed") })
		head := s.View().HeadSeq
		wantEvents(t, watch(t, s, head-1, head, text("c", "three"), false)[1:], "12 input.admitted", "~12 status compacting", "~12 status compaction_failed",
			"13 turn.started", "14 item.completed i1", "15 turn.ended")
	})
}
