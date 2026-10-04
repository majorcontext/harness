package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const askInput = `{"questions":[{"question":"Which database?","options":[{"label":"PostgreSQL"},{"label":"SQLite"}]}]}`

// parker is a backend that owns its loop. A turn whose input is "ask" calls a
// tool and parks it as a question; any other turn reports the Request it got
// and the resolution of the question that it parked.
type parker struct {
	// sibling holds a second open call on the item of the question.
	sibling bool
	// silent never records a result for an answered call.
	silent bool
	runs   chan turn.Request
	// resolved receives the resolution that the backend reads for call c1.
	resolved chan eventlog.RequestResolved
}

func newParker() *parker {
	return &parker{runs: make(chan turn.Request, 8), resolved: make(chan eventlog.RequestResolved, 8)}
}

func (*parker) Capabilities(string) turn.Capabilities { return turn.Capabilities{OwnsLoop: true} }

func (p *parker) Run(_ context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	p.runs <- req
	if len(req.Input) > 0 && req.Input[0].Parts[0].Text == "ask" {
		call := eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{
			{Type: eventlog.PartToolCall, CallID: "c1", Name: "AskUserQuestion", Arguments: json.RawMessage(askInput)}}}
		if p.sibling {
			call.Parts = append(call.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: "c0", Name: "Bash", Arguments: json.RawMessage(`{}`)})
		}
		if err := out.Item(call); err != nil {
			return turn.Result{}, err
		}
		return turn.Result{}, out.Ask("c1", "question", json.RawMessage(askInput))
	}
	if r, ok := out.Resolution("c1"); ok {
		p.resolved <- r
		if r.Resolution == eventlog.ResolutionAnswered && !p.silent {
			got := eventlog.Message{Role: eventlog.RoleTool, Parts: []eventlog.Part{{Type: eventlog.PartToolResult, CallID: "c1", Name: "AskUserQuestion", Text: string(r.Answer)}}}
			if err := out.Item(got); err != nil {
				return turn.Result{}, err
			}
		}
	}
	return turn.Result{}, out.Item(say("done"))
}

func questionRuntime(t *testing.T, st harness.Store, p *parker, ask bool) *harness.Runtime {
	t.Helper()
	r, err := harness.NewWithBackend(harness.Options{Store: st, AskUserQuestion: ask}, p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func parked(t *testing.T, st harness.Store, p *parker) (*harness.Runtime, *harness.Session) {
	t.Helper()
	r := questionRuntime(t, st, p, true)
	s := create(t, r)
	submit(t, s, text("a", "ask"))
	<-p.runs
	return r, s
}

func TestAQuestionEndsTheTurnAndWaitsForAnAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		r, s := parked(t, st, p)
		wantLog(t, st, 2, "input.admitted a", "turn.started a", "item.completed assistant c1", "request.opened", "turn.ended awaiting_input")
		if v := s.View(); v.Status != protocol.StatusWaiting || v.TurnID != "" {
			t.Errorf("View = status %s, turn %q, want waiting with no turn", v.Status, v.TurnID)
		}
		for e, err := range s.Events(bg, 0) {
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind != "request.opened" {
				continue
			}
			var o eventlog.RequestOpened
			if err := json.Unmarshal(e.Data, &o); err != nil || o.RequestID != "c1" || o.RequestKind != "question" || string(o.Payload) != askInput {
				t.Errorf("request.opened = %+v, %v, want request c1 of kind question with the tool input", o, err)
			}
			break
		}
		closeRuntime(t, r)
	})
}

func TestAnAnswerRunsATurnWithNoInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		r, s := parked(t, st, p)
		answer := json.RawMessage(`{"Which database?":"SQLite"}`)
		if err := s.Resolve(bg, "c1", protocol.Resolution{Answer: answer}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		req := <-p.runs
		if len(req.Input) != 0 || len(req.History) != 2 || req.History[1].Role != eventlog.RoleAssistant {
			t.Errorf("Request after the answer = %d inputs, %d history messages, want no input and the asked call last", len(req.Input), len(req.History))
		}
		if got := <-p.resolved; got.Resolution != eventlog.ResolutionAnswered || string(got.Answer) != string(answer) {
			t.Errorf("resolution read by the backend = %+v, want the answer", got)
		}
		wantLog(t, st, 7, "request.resolved", "turn.started", "item.completed tool c1 "+string(answer), "item.completed assistant done", "turn.ended completed")
		if s.View().Status != protocol.StatusIdle {
			t.Errorf("status after the answered turn = %s, want idle", s.View().Status)
		}
		closeRuntime(t, r)
	})
}

func TestADismissalRunsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		r, s := parked(t, st, p)
		if err := s.Resolve(bg, "c1", protocol.Resolution{Dismiss: true}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		wantLog(t, st, 7, "request.resolved")
		if len(p.runs) != 0 || s.View().Status != protocol.StatusIdle {
			t.Errorf("after a dismissal: %d runs, status %s, want none and idle", len(p.runs), s.View().Status)
		}
		closeRuntime(t, r)
	})
}

func TestAnInputDismissesAnOpenQuestionFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		r, s := parked(t, st, p)
		submit(t, s, text("b", "never mind"))
		req := <-p.runs
		if got := <-p.resolved; got.Resolution != eventlog.ResolutionDismissed {
			t.Errorf("resolution read by the backend = %+v, want a dismissal", got)
		}
		if len(req.Input) != 1 || req.Input[0].Parts[0].Text != "never mind" {
			t.Errorf("Request.Input = %+v, want the new input", req.Input)
		}
		wantLog(t, st, 7, "request.resolved", "input.admitted b", "turn.started b", "item.completed assistant done", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestResolveRefusesWhatNoRequestOrBodyAllows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		r, s := parked(t, st, p)
		for _, tc := range []struct {
			name string
			id   string
			res  protocol.Resolution
			want error
		}{
			{"an unknown request", "c2", protocol.Resolution{Dismiss: true}, harness.ErrRequestNotPending},
			{"an answer and a dismissal", "c1", protocol.Resolution{Answer: json.RawMessage(`{"a":"b"}`), Dismiss: true}, harness.ErrInvalidRequest},
			{"neither", "c1", protocol.Resolution{}, harness.ErrInvalidRequest},
			{"an empty answer", "c1", protocol.Resolution{Answer: json.RawMessage(`{}`)}, harness.ErrInvalidRequest},
			{"an empty answer with space", "c1", protocol.Resolution{Answer: json.RawMessage(`{ }`)}, harness.ErrInvalidRequest},
			{"an answer that is not a map", "c1", protocol.Resolution{Answer: json.RawMessage(`["SQLite"]`)}, harness.ErrInvalidRequest},
			{"a choice that is not text", "c1", protocol.Resolution{Answer: json.RawMessage(`{"Which database?":1}`)}, harness.ErrInvalidRequest},
		} {
			if err := s.Resolve(bg, tc.id, tc.res); !errors.Is(err, tc.want) {
				t.Errorf("%s: Resolve = %v, want %v", tc.name, err, tc.want)
			}
		}
		if err := s.Resolve(bg, "c1", protocol.Resolution{Dismiss: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.Resolve(bg, "c1", protocol.Resolution{Dismiss: true}); !errors.Is(err, harness.ErrRequestNotPending) {
			t.Errorf("a second Resolve = %v, want ErrRequestNotPending", err)
		}
		closeRuntime(t, r)
	})
}

func TestOnlyAnEmbedderThatAnswersGetsQuestions(t *testing.T) {
	for _, ask := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			p := newParker()
			r := questionRuntime(t, harness.NewMemStore(), p, ask)
			s := create(t, r)
			submit(t, s, text("a", "hi"))
			if got := (<-p.runs).Questions; got != ask {
				t.Errorf("Request.Questions with AskUserQuestion %t = %t", ask, got)
			}
			closeRuntime(t, r)
		})
	}
}

func TestACallThatTheBackendNeverResultedGetsACutOffResultAfterAnAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		p.silent = true
		r, s := parked(t, st, p)
		if err := s.Resolve(bg, "c1", protocol.Resolution{Answer: json.RawMessage(`{"Which database?":"SQLite"}`)}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		<-p.runs
		wantLog(t, st, 7, "request.resolved", "turn.started", "item.completed assistant done",
			"item.completed tool c1 cut off before a result was recorded; check whether it took effect before running it again", "turn.ended completed")
		closeRuntime(t, r)
	})
}

func TestASiblingOfTheQuestionGetsAResultWhenTheTurnParks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, p := harness.NewMemStore(), newParker()
		p.sibling = true
		r, s := parked(t, st, p)
		wantLog(t, st, 2, "input.admitted a", "turn.started a", "item.completed assistant c1 c0", "request.opened",
			"item.completed tool c0 cut off before a result was recorded; check whether it took effect before running it again", "turn.ended awaiting_input")
		submit(t, s, text("b", "never mind"))
		<-p.runs
		closeRuntime(t, r)
	})
}
