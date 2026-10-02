package fakemodel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
)

func ask(t testing.TB, s *Server, text string) (provider.Stream, error) {
	t.Helper()
	return askCtx(t, context.Background(), s, text)
}

func askCtx(t testing.TB, ctx context.Context, s *Server, text string) (provider.Stream, error) {
	t.Helper()
	c := &anthropic.Client{APIKey: "k", BaseURL: s.URL()}
	return c.Stream(ctx, &provider.Request{
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

func (r *recorder) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func TestUnmatchedRequestFailsLoudly(t *testing.T) {
	rec := &recorder{TB: t}
	s := New(rec, Step{Match: LastUserText("expected"), Reply: Reply{Text: "x"}})
	if _, err := ask(t, s, "surprise title request"); err == nil {
		t.Fatal("unmatched request succeeded, want an HTTP error")
	}
	rec.runCleanups()
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "surprise title request") || !strings.Contains(rec.errors[0], "sys one") {
		t.Errorf("errors = %q, want one naming the last user text and system prefix", rec.errors)
	}
}

// blockedRun streams reply through a bare Server inside a synctest bubble, so
// every "has not happened yet" assertion is exact: Wait returns only once the
// handler goroutine can make no further progress.
type blockedRun struct {
	s    *Server
	w    *httptest.ResponseRecorder
	done chan struct{}
}

func startBlocked(ctx context.Context, reply Reply) *blockedRun {
	s := &Server{closing: make(chan struct{}), releases: map[string]chan struct{}{}, blocked: map[string]chan struct{}{}}
	br := &blockedRun{s: s, w: httptest.NewRecorder(), done: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	go func() {
		defer close(br.done)
		s.stream(br.w, r, 1, "slow", reply)
	}()
	synctest.Wait()
	return br
}

func (br *blockedRun) finished() bool {
	select {
	case <-br.done:
		return true
	default:
		return false
	}
}

func TestBlockUntilRelease(t *testing.T) {
	tests := []struct {
		name      string
		reply     Reply
		wantDelta string
	}{
		{"text", Reply{Text: "slow text", Block: true}, `"text":"slow text"`},
		{"tool call only", Reply{Block: true, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "bash"}}}, `"partial_json"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				br := startBlocked(context.Background(), tc.reply)
				select {
				case <-br.s.Blocked("slow"):
				default:
					t.Fatal("handler is not waiting on Release")
				}
				if body := br.w.Body.String(); !strings.Contains(body, tc.wantDelta) {
					t.Errorf("body before Release lacks the first delta %s:\n%s", tc.wantDelta, body)
				}
				if br.finished() || strings.Contains(br.w.Body.String(), "message_stop") {
					t.Fatalf("stream completed before Release:\n%s", br.w.Body.String())
				}
				br.s.Release("slow")
				synctest.Wait()
				if !br.finished() || !strings.Contains(br.w.Body.String(), "message_stop") {
					t.Errorf("stream did not complete after Release:\n%s", br.w.Body.String())
				}
			})
		})
	}
}

func TestBlockedStreamExits(t *testing.T) {
	tests := []struct {
		name string
		end  func(cancel context.CancelFunc, s *Server)
	}{
		{"client cancel", func(cancel context.CancelFunc, _ *Server) { cancel() }},
		{"server close", func(_ context.CancelFunc, s *Server) { close(s.closing) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				br := startBlocked(ctx, Reply{Text: "x", Block: true})
				if br.finished() {
					t.Fatal("handler returned before cancel")
				}
				tc.end(cancel, br.s)
				synctest.Wait()
				if !br.finished() {
					t.Error("handler still blocked after the stream ended")
				}
				if strings.Contains(br.w.Body.String(), "message_stop") {
					t.Errorf("an aborted stream completed:\n%s", br.w.Body.String())
				}
			})
		})
	}
}

func TestUndecodableRequestFailsLoudly(t *testing.T) {
	rec := &recorder{TB: t}
	s := New(rec, Step{Match: Any(), Reply: Reply{Text: "x"}})
	resp, err := http.Post(s.URL()+"/v1/messages", "application/json", strings.NewReader(`{"messages": [`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if got := len(s.Requests()); got != 0 {
		t.Errorf("recorded %d requests, want 0 for an undecodable body", got)
	}
	rec.runCleanups()
	if len(rec.errors) != 1 || !strings.Contains(rec.errors[0], "undecodable request body") || !strings.Contains(rec.errors[0], `{\"messages\": [`) {
		t.Errorf("errors = %q, want one naming the undecodable body", rec.errors)
	}
}

func TestStepConsumption(t *testing.T) {
	tests := []struct {
		name         string
		repeat       bool
		wantReplies  []int
		wantUnmatchd int
	}{
		{"consumed once", false, []int{http.StatusOK, http.StatusInternalServerError}, 1},
		{"repeat", true, []int{http.StatusOK, http.StatusOK, http.StatusOK}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{TB: t}
			s := New(rec, Step{Match: Any(), Repeat: tc.repeat, Reply: Reply{Text: "x"}})
			var got []int
			for range tc.wantReplies {
				resp, err := http.Post(s.URL()+"/v1/messages", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"q"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				got = append(got, resp.StatusCode)
			}
			rec.runCleanups()
			if !slices.Equal(got, tc.wantReplies) {
				t.Errorf("statuses = %v, want %v", got, tc.wantReplies)
			}
			if len(rec.errors) != tc.wantUnmatchd {
				t.Errorf("unmatched errors = %q, want %d", rec.errors, tc.wantUnmatchd)
			}
		})
	}
}

func TestRequestDecodeAndMatchers(t *testing.T) {
	const body = `{
	  "system": [{"type":"text","text":"sys a"},{"type":"text","text":"sys b"}],
	  "tools": [{"name":"zeta"},{"name":"alpha"}],
	  "messages": [
	    {"role":"user","content":"start"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","is_error":true,"content":[{"type":"text","text":"boom"}]}]}
	  ]}`
	var seen []int
	s := New(t,
		Step{Name: "no", Match: And(LastToolResult("bash"), SystemContains("absent")), Reply: Reply{Text: "no"}},
		Step{Name: "yes", Match: And(LastToolResult("bash"), SystemContains("sys b")), Reply: Reply{Text: "yes"}},
	)
	s.OnRequest(func(n int) { seen = append(seen, n) })
	resp, err := http.Post(s.URL()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"text":"yes"`) || strings.Contains(string(raw), `"text":"no"`) {
		t.Errorf("response %d %s, want only the second step's reply", resp.StatusCode, raw)
	}
	want := Request{
		System: "sys a\nsys b",
		Tools:  []string{"alpha", "zeta"},
		Messages: []Message{
			{Role: "user", Parts: []Part{{Kind: "text", Text: "start"}}},
			{Role: "assistant", Parts: []Part{{Kind: "tool_use", ToolName: "bash", ToolInput: map[string]any{"command": "ls"}, ToolUseID: "tu_1"}}},
			{Role: "user", Parts: []Part{{Kind: "tool_result", Text: "boom", ToolName: "bash", ToolUseID: "tu_1", IsError: true}}},
		},
	}
	if got := s.Requests(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("requests = %+v, want [%+v]", got, want)
	}
	if !slices.Equal(seen, []int{1}) {
		t.Errorf("OnRequest calls = %v, want [1]", seen)
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
