package harness_test

import (
	"encoding/json"
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

// watch reads the events of s after seq, submits in at the first event, and
// returns every event up to the turn.ended after seq end. With stall, it
// reads nothing more until the bubble is idle.
func watch(t *testing.T, s *harness.Session, after, end uint64, in protocol.Input, stall bool) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for e, err := range s.Events(bg, after) {
		if err != nil {
			t.Fatal(err)
		}
		if out = append(out, e); len(out) == 1 {
			submit := func() { _, _ = s.Submit(bg, in) }
			if submit(); stall {
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
	r, _, _ := codexRuntime(t, s, false, false)
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, watch(t, sess, 1, 0, text("a", "hi"), false), "2 owner.acquired", "3 input.admitted", "4 turn.started",
		"~4 item.started i1", "~4 item.delta i1 reasoning plan", "~4 item.delta i1 text hello",
		"5 item.completed i1", "6 turn.ended")
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
		flaky := func(turn.Sink) error { return fmt.Errorf("%w: flaky", turn.ErrRetryable) }
		st, retries := harness.NewMemStore(), 1
		r, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{PromptRetries: &retries}},
			&scripted{steps: []func(turn.Sink) error{flaky, deltas(2, "x"), deltas(1000, "y")}})
		if err != nil {
			t.Fatal(err)
		}
		s := create(t, r)
		wantEvents(t, watch(t, s, 1, 0, text("a", "one"), false), "2 owner.acquired", "3 input.admitted", "4 turn.started",
			"~4 status retrying 1", "~4 status running", "~4 item.started i1", "~4 item.delta i1 text x", "~4 item.delta i1 text x", "5 item.completed i1", "6 turn.ended")

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
