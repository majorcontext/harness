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

func TestUpdateAppliesToTheNextTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, f := harness.NewMemStore(), newFake()
		r := runtime(t, st, f)
		s := create(t, r)
		submit(t, s, text("a", "one"))
		run := <-f.runs
		p := protocol.SettingsPatch{Model: new("test/other"), Effort: new("high")}
		v, err := s.Update(bg, p)
		if err != nil || v.Model != "test/other" || v.Effort != "high" || v.Status != protocol.StatusRunning {
			t.Fatalf("Update = %+v, %v", v, err)
		}
		if again, err := s.Update(bg, p); err != nil || again.HeadSeq != v.HeadSeq {
			t.Fatalf("unchanged Update = head %d, %v, want head %d", again.HeadSeq, err, v.HeadSeq)
		}
		submit(t, s, text("b", "two"))
		run.end()
		next := <-f.runs
		next.end()
		if run.req.Model != "test/model" || next.req.Model != "test/other" || next.req.Settings.Effort != "high" {
			t.Fatalf("models = %s then %s %+v, want test/model then test/other high", run.req.Model, next.req.Model, next.req.Settings)
		}
		wantLog(t, st, 4, "settings.changed", "input.admitted b", "turn.ended completed", "turn.started b", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestUpdateSwitchesTheCodexProvider(t *testing.T) {
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"hi": {Reasoning: []string{"plan"}}}},
		harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}},
		harnesstest.Step{Name: "again", Match: harnesstest.LastUserText("again"), Reply: harnesstest.Reply{Text: "ok"}})
	r, _, rec := codexRuntime(t, o, false, false, "")
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "codex/gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	converse(t, s, "hi")
	for p, want := range map[protocol.SettingsPatch]error{{Model: new("codex/no-such-model")}: harness.ErrModelUnavailable,
		{Model: new("nope/gpt-5")}: harness.ErrModelUnavailable, {Effort: new("hard")}: harness.ErrInvalidRequest,
		{Model: new("openai/gpt-5"), Effort: new("high")}: nil} {
		if _, err := s.Update(bg, p); !errors.Is(err, want) {
			t.Errorf("Update(%v) = %v, want %v", p, err, want)
		}
	}
	cc, err := r.Create(bg, protocol.CreateSession{ID: "s2", Model: "claude-code/opus"})
	if err != nil {
		t.Fatal(err)
	}
	for m, want := range map[string]error{"codex/gpt-5": nil, "claude-code/sonnet": nil} {
		if _, err := cc.Update(bg, protocol.SettingsPatch{Model: new(m)}); !errors.Is(err, want) {
			t.Errorf("claude-code Update(%s) = %v, want %v", m, err, want)
		}
	}
	watch(t, s, s.View().HeadSeq-1, s.View().HeadSeq, text("b", "again"), false)
	if got, want := transcript(o.Requests()[1]), []string{"user text :hi", "assistant text :hello", "user text :again"}; !slices.Equal(got, want) {
		t.Errorf("request after the switch = %q, want %q", got, want)
	}
	if calls, e := rec.Calls(), o.WireEvents()[1].ReasoningEffort; e != "high" || !slices.Equal(calls, []string{codexPost, "openai POST /backend-api/codex/responses"}) {
		t.Errorf("transport calls = %q, effort = %q, want codex then openai, high", calls, e)
	}
}

func TestSetGoalNeedsAnEvaluator(t *testing.T) {
	r, _, _ := codexRuntime(t, harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}), false, false, "")
	s, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "claude-code/opus"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGoal(bg, protocol.Goal{Condition: "say done"}); !errors.Is(err, harness.ErrInvalidRequest) || s.View().Goal != nil {
		t.Errorf("SetGoal with no goal_evaluator_model = %v, goal %v, want %v and no goal", err, s.View().Goal, harness.ErrInvalidRequest)
	}
}
