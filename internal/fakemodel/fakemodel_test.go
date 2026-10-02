package fakemodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

func ask(t testing.TB, s *Server, text string) (provider.Stream, error) {
	t.Helper()
	c := &anthropic.Client{APIKey: "k", BaseURL: s.URL()}
	return c.Stream(context.Background(), &provider.Request{
		Model:     message.ModelRef{Provider: anthropic.Family, Model: "m"},
		System:    []string{"sys one", "sys two"},
		Messages:  []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: text}}}},
		MaxTokens: 10,
	})
}

func drain(t testing.TB, st provider.Stream) []provider.Event {
	t.Helper()
	var evs []provider.Event
	for {
		ev, err := st.Next()
		if err == io.EOF {
			return evs
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		evs = append(evs, ev)
	}
}

func done(t testing.TB, evs []provider.Event) provider.Event {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].Type != provider.EventDone {
		t.Fatalf("stream did not end with done: %+v", evs)
	}
	return evs[len(evs)-1]
}

func textOf(evs []provider.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Type == provider.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func TestTextReply(t *testing.T) {
	s := New(t, Step{Match: Any(), Reply: Reply{Text: "hello"}})
	st, err := ask(t, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, st)
	d := done(t, evs)
	if got := textOf(evs); got != "hello" {
		t.Errorf("text = %q, want hello", got)
	}
	if d.StopReason != provider.StopEndTurn {
		t.Errorf("stop = %q, want end_turn", d.StopReason)
	}
	if d.Usage.InputTokens != 5 || d.Usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want 5/3", d.Usage)
	}
}

func TestToolCallReply(t *testing.T) {
	s := New(t, Step{Match: Any(), Reply: Reply{ToolCalls: []ToolCall{
		{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo hi"}},
	}}})
	st, err := ask(t, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, st)
	var calls []*message.ToolCall
	for _, ev := range evs {
		if ev.Type == provider.EventToolCall {
			calls = append(calls, ev.ToolCall)
		}
	}
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("calls = %+v, want one bash call", calls)
	}
	var in map[string]any
	if err := json.Unmarshal(calls[0].Arguments, &in); err != nil || in["command"] != "echo hi" {
		t.Errorf("arguments = %s (%v), want command echo hi", calls[0].Arguments, err)
	}
	if got := done(t, evs).StopReason; got != provider.StopToolUse {
		t.Errorf("stop = %q, want tool_use", got)
	}
}

func TestStepsMatchInOrder(t *testing.T) {
	s := New(t,
		Step{Name: "a", Match: LastUserText("one"), Reply: Reply{Text: "A"}},
		Step{Name: "b", Match: LastUserText("two"), Reply: Reply{Text: "B"}},
	)
	var got []string
	for _, q := range []string{"two", "one"} {
		st, err := ask(t, s, q)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, textOf(drain(t, st)))
	}
	if strings.Join(got, ",") != "B,A" {
		t.Errorf("replies = %v, want [B A]", got)
	}
	reqs := s.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].System != "sys one\nsys two" {
		t.Errorf("system = %q, want joined segments", reqs[0].System)
	}
}

type recorder struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (r *recorder) Helper()                   {}
func (r *recorder) Cleanup(f func())          { r.cleanups = append(r.cleanups, f) }
func (r *recorder) Errorf(f string, a ...any) { r.errors = append(r.errors, fmt.Sprintf(f, a...)) }

func TestUnmatchedRequestFailsLoudly(t *testing.T) {
	rec := &recorder{TB: t}
	s := New(rec, Step{Match: LastUserText("expected"), Reply: Reply{Text: "x"}})
	if _, err := ask(t, s, "surprise title request"); err == nil {
		t.Fatal("unmatched request succeeded, want an HTTP error")
	}
	for i := len(rec.cleanups) - 1; i >= 0; i-- {
		rec.cleanups[i]()
	}
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "surprise title request") || !strings.Contains(rec.errors[0], "sys one") {
		t.Errorf("errors = %q, want one naming the last user text and system prefix", rec.errors)
	}
}

func TestBlockUntilRelease(t *testing.T) {
	s := New(t, Step{Name: "slow", Match: Any(), Reply: Reply{Text: "slow text", Block: true}})
	st, err := ask(t, s, "hi")
	if err != nil {
		t.Fatal(err)
	}
	for {
		ev, err := st.Next()
		if err != nil {
			t.Fatalf("Next before first delta: %v", err)
		}
		if ev.Type == provider.EventTextDelta {
			break
		}
	}
	var released atomic.Bool
	finished := make(chan bool)
	go func() {
		for {
			ev, err := st.Next()
			if err != nil || ev.Type == provider.EventDone {
				finished <- released.Load()
				return
			}
		}
	}()
	released.Store(true)
	s.Release("slow")
	if !<-finished {
		t.Error("stream completed before Release")
	}
}

func TestHTTPErrorReply(t *testing.T) {
	s := New(t, Step{Match: Any(), Reply: Reply{HTTPStatus: 529}})
	_, err := ask(t, s, "hi")
	if err == nil {
		t.Fatal("err = nil, want an HTTP error")
	}
	if _, ok := provider.AsRetryable(err); !ok {
		t.Errorf("AsRetryable(%v) = false, want retryable", err)
	}
}
