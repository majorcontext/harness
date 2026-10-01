package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// workerLog records the last user text of every worker (tool-bearing) request.
type workerLog struct {
	*goalProv
	mu    sync.Mutex
	texts []string
}

func (p *workerLog) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if len(req.Tools) != 0 {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == message.RoleUser {
				p.mu.Lock()
				p.texts = append(p.texts, req.Messages[i].Parts.Text())
				p.mu.Unlock()
				break
			}
		}
	}
	return p.goalProv.Stream(ctx, req)
}

func (p *workerLog) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.texts...)
}

// A deferred goal on an idle session must not send a turn when it is
// registered, and its loop must evaluate the next prompt's turn before it
// posts the condition.
func TestGoalDeferOnIdleSessionEvaluatesBeforePosting(t *testing.T) {
	const cond = "write a summary"
	tests := []struct {
		name      string
		eval      [][]provider.Event
		worker    [][]provider.Event
		wantSent  int
		wantGuide string
	}{
		{
			name:     "met sends only the prompt",
			eval:     [][]provider.Event{asstTurn("MET: done")},
			worker:   [][]provider.Event{asstTurn("prompt done")},
			wantSent: 1,
		},
		{
			name:      "not met sends guidance, not the condition",
			eval:      [][]provider.Event{asstTurn("NOT MET: needs a summary"), asstTurn("MET: done")},
			worker:    [][]provider.Event{asstTurn("prompt done"), asstTurn("more")},
			wantSent:  2,
			wantGuide: "needs a summary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &workerLog{goalProv: &goalProv{name: "test", worker: tc.worker, eval: tc.eval}}
			h := newGoalHarness(t, prov)
			id := h.createSession("test/m1")
			sse := h.openSSE("?from=0", "")

			resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": cond, "defer": true})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("POST goal status %d: %s", resp.StatusCode, data)
			}
			if got := string(data); !strings.Contains(got, `"status":"armed"`) {
				t.Fatalf("response = %s, want status armed", got)
			}
			if n := len(prov.sent()); n != 0 {
				t.Fatalf("turns sent after deferred registration = %d, want 0", n)
			}
			if view := h.getGoalSummary(id); view.Goal == nil || !view.Goal.Active {
				t.Fatalf("goal = %+v, want active", view.Goal)
			}

			resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
				"parts": []map[string]string{{"type": "text", "text": "hello"}},
			})
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
			}
			sse.collectUntilIdle(t)
			sse.collectUntilIdle(t)

			sent := prov.sent()
			if len(sent) != tc.wantSent {
				t.Fatalf("turns sent = %q, want %d", sent, tc.wantSent)
			}
			for _, s := range sent {
				if s == cond {
					t.Errorf("condition posted as a turn: %q", sent)
				}
			}
			if tc.wantGuide != "" && !strings.Contains(sent[1], tc.wantGuide) {
				t.Errorf("guidance = %q, want it to contain %q", sent[1], tc.wantGuide)
			}
		})
	}
}

// Without defer, an idle session still posts the condition as turn one.
func TestGoalWithoutDeferPostsCondition(t *testing.T) {
	prov := &workerLog{goalProv: &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("done")},
		eval:   [][]provider.Event{asstTurn("MET: done")},
	}}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")
	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": "c"})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(data), `"status":"started"`) {
		t.Fatalf("POST goal = %d %s, want 202 started", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)
	if sent := prov.sent(); len(sent) != 1 || sent[0] != "c" {
		t.Fatalf("turns sent = %q, want [c]", sent)
	}
}

// A deferred POST for a goal left active and idle must defer it too, not
// resume it with the condition posted.
func TestGoalDeferOnActiveIdleGoalSkipsConditionTurn(t *testing.T) {
	const cond = "write a summary"
	prov := &workerLog{goalProv: &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("prompt done")},
		eval:   [][]provider.Event{asstTurn("MET: done")},
	}}
	h := newGoalHarness(t, prov)
	id := h.createSession("test/m1")
	sse := h.openSSE("?from=0", "")
	if err := h.srv.residentSession(id).RegisterGoal(cond); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("POST", "/session/"+id+"/goal", map[string]any{"condition": cond, "defer": true})
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(data), `"status":"armed"`) {
		t.Fatalf("POST goal = %d %s, want 202 armed", resp.StatusCode, data)
	}
	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hello"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt_async status %d: %s", resp.StatusCode, data)
	}
	sse.collectUntilIdle(t)
	sse.collectUntilIdle(t)
	if sent := prov.sent(); len(sent) != 1 || sent[0] == cond {
		t.Fatalf("turns sent = %q, want only the prompt", sent)
	}
}
