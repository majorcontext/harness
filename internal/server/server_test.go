package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

// hold is an embedder tool that reports each call on started and returns
// when its ctx ends.
type hold chan struct{}

func (hold) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "hold", Description: "hold", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (h hold) Run(ctx context.Context, _ protocol.ToolCall) (protocol.ToolResult, error) {
	h <- struct{}{}
	<-ctx.Done()
	return protocol.ToolResult{}, context.Cause(ctx)
}

// serve serves the handler of a Runtime on a MemStore whose codex and
// openai providers are a scripted Codex server, and creates session s1.
func serve(t *testing.T, tools []harness.Tool, steps ...harnesstest.Step) (*harness.Runtime, string) {
	t.Helper()
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: o.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Tools: tools,
		Config: config.Config{PromptRetries: &retries, GoalEvaluatorModel: "codex/gpt-5", Providers: map[string]config.Provider{"codex": p, "openai": p}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(func() {
		_ = r.Close(context.Background())
		srv.Close()
	})
	if code := call(t, "POST", srv.URL+"/sessions", `{"id":"s1","model":"codex/gpt-6-sol"}`, nil); code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	return r, srv.URL + "/sessions/s1"
}

// call sends body and decodes the reply into out when out is not nil.
func call(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

type stream struct{ sc *bufio.Scanner }

func subscribe(t *testing.T, url, lastID string) stream {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("subscribe = %d %q", resp.StatusCode, ct)
	}
	return stream{bufio.NewScanner(resp.Body)}
}

// until renders each frame through the first of kind: "<id> <kind>" for a
// durable frame, "~<id><seq> <kind> <type> <text>" for an ephemeral one.
func (s stream) until(t *testing.T, kind string) []string {
	t.Helper()
	var out []string
	id := ""
	for s.sc.Scan() {
		line := s.sc.Text()
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			id = v
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if line == "" {
			id = ""
		}
		if !ok {
			continue
		}
		var e protocol.Event
		var f protocol.ItemFrame
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(e.Data, &f)
		switch {
		case e.Ephemeral:
			out = append(out, strings.TrimSpace(fmt.Sprintf("~%s%d %s %s %s", id, e.Seq, e.Kind, f.Type, f.Text)))
		case id != strconv.FormatUint(e.Seq, 10):
			out = append(out, fmt.Sprintf("id %q for seq %d", id, e.Seq))
		default:
			out = append(out, id+" "+e.Kind)
		}
		if e.Kind == kind {
			return out
		}
	}
	t.Fatalf("stream ended: %v after %q", s.sc.Err(), out)
	return nil
}

func want(t *testing.T, what string, got any, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %+v, want %+v", what, got, want)
	}
}

func TestSessionOverHTTP(t *testing.T) {
	_, s1 := serve(t, nil, harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
	live := subscribe(t, s1+"/events", "")
	want(t, "first frames", live.until(t, "owner.acquired"), []string{"1 session.created", "2 owner.acquired"})
	in := `{"id":"a","parts":[{"type":"text","text":"hi"}]}`
	for _, code := range []int{http.StatusCreated, http.StatusOK} {
		var got protocol.Admitted
		want(t, "submit status", call(t, "POST", s1+"/inputs", in, &got), code)
		want(t, "receipt", got, protocol.Admitted{InputID: "a", Seq: 3})
	}
	want(t, "turn frames", live.until(t, "turn.ended"),
		[]string{"3 input.admitted", "4 turn.started", "~4 item.started", "~4 item.delta text hello", "5 context.measured", "6 item.completed", "7 turn.ended"})

	for _, tc := range []struct{ query, lastID string }{{"?after=4", ""}, {"", "4"}, {"?after=1", "4"}} {
		want(t, "resume "+tc.query+" "+tc.lastID, subscribe(t, s1+"/events"+tc.query, tc.lastID).until(t, "turn.ended"),
			[]string{"5 context.measured", "6 item.completed", "7 turn.ended"})
	}
	for _, tc := range []struct {
		query string
		kinds []string
		next  uint64
	}{{"?after=2&limit=2", []string{"input.admitted", "turn.started"}, 4}, {"?after=4", []string{"context.measured", "item.completed", "turn.ended"}, 0}} {
		var page protocol.EventPage
		call(t, "GET", s1+"/events"+tc.query, "", &page)
		var got []string
		for _, e := range page.Events {
			got = append(got, e.Kind)
		}
		want(t, "page "+tc.query, [2]any{got, page.Next}, [2]any{tc.kinds, tc.next})
	}

	var v protocol.Session
	want(t, "patch status", call(t, "PATCH", s1, `{"model":"openai/gpt-6-sol","effort":"high"}`, &v), http.StatusOK)
	want(t, "patched", [3]any{v.Model, v.Effort, v.HeadSeq}, [3]any{"openai/gpt-6-sol", "high", uint64(8)})
	var page protocol.SessionPage
	call(t, "GET", strings.TrimSuffix(s1, "/s1"), "", &page)
	call(t, "GET", s1, "", &v)
	want(t, "list", page.Sessions, []protocol.Session{v})
	want(t, "view", [2]string{v.Status, v.Model}, [2]string{protocol.StatusIdle, "openai/gpt-6-sol"})
}

func TestInterruptOverHTTP(t *testing.T) {
	started := make(hold)
	_, s1 := serve(t, []harness.Tool{started}, harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("run"),
		Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "hold"}}}})
	call(t, "POST", s1+"/inputs", `{"id":"a","parts":[{"type":"text","text":"run"}]}`, nil)
	<-started
	var busy protocol.ErrorBody
	want(t, "compact while a turn runs", [2]any{call(t, "POST", s1+"/compact", "", &busy), busy.Error.Code},
		[2]any{http.StatusConflict, protocol.CodeSessionBusy})
	want(t, "interrupt status", call(t, "POST", s1+"/interrupt", "", nil), http.StatusNoContent)
	want(t, "compact status", call(t, "POST", s1+"/compact", "", nil), http.StatusNoContent)
	var page protocol.EventPage
	call(t, "GET", s1+"/events?after=6", "", &page)
	want(t, "events after the interrupt", len(page.Events), 2)
	if last := page.Events[len(page.Events)-1]; last.Kind != "turn.ended" || !bytes.Contains(last.Data, []byte(`"stop_reason":"interrupted"`)) {
		t.Errorf("last event = %s %s, want an interrupted turn.ended", last.Kind, last.Data)
	}
}

func TestGoalOverHTTP(t *testing.T) {
	judge := harnesstest.SystemContains("MET: <one short sentence")
	_, s1 := serve(t, nil, harnesstest.Step{Name: "work", Match: harnesstest.LastUserText("say done"), Reply: harnesstest.Reply{Text: "done"}},
		harnesstest.Step{Name: "judge", Match: judge, Reply: harnesstest.Reply{Text: "MET: said done"}})
	live := subscribe(t, s1+"/events", "")
	var v protocol.Session
	want(t, "put status", call(t, "PUT", s1+"/goal", `{"condition":"say done","max_turns":3}`, &v), http.StatusOK)
	want(t, "put goal", *v.Goal, protocol.GoalView{Goal: protocol.Goal{Condition: "say done", MaxTurns: 3}, State: "active"})
	live.until(t, "goal.changed")
	call(t, "GET", s1, "", &v)
	want(t, "achieved", [2]any{v.Goal.State, v.Goal.Turns}, [2]any{"achieved", 1})
	want(t, "delete status", call(t, "DELETE", s1+"/goal", "", nil), http.StatusNoContent)
	call(t, "GET", s1, "", &v)
	want(t, "cleared", v.Goal.State, "cleared")
	var bad protocol.ErrorBody
	want(t, "empty condition", [2]any{call(t, "PUT", s1+"/goal", `{"condition":" "}`, &bad), bad.Error.Code}, [2]any{http.StatusBadRequest, protocol.CodeInvalidRequest})
}

func TestModelsOverHTTP(t *testing.T) {
	r, s1 := serve(t, nil)
	var got []protocol.Model
	want(t, "status", call(t, "GET", strings.TrimSuffix(s1, "/sessions/s1")+"/models", "", &got), http.StatusOK)
	want(t, "models", got, r.Models())
	want(t, "health", call(t, "GET", strings.TrimSuffix(s1, "/sessions/s1")+"/health", "", nil), http.StatusOK)
}

func TestErrorsOverHTTP(t *testing.T) {
	r, s1 := serve(t, nil, harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
	base := strings.TrimSuffix(s1, "/s1")
	if code := call(t, "POST", s1+"/inputs", `{"id":"a","parts":[{"type":"text","text":"hi"}]}`, nil); code != http.StatusCreated {
		t.Fatalf("submit = %d", code)
	}
	subscribe(t, s1+"/events", "").until(t, "turn.ended")
	for _, tc := range []struct {
		method, url, body string
		status            int
		code              string
	}{
		{"POST", base, `{"model":""}`, 400, protocol.CodeInvalidRequest},
		{"POST", base, `{"model":`, 400, protocol.CodeInvalidRequest},
		{"POST", base, `{"model":"codex/gpt-6-sol","modle":"x"}`, 400, protocol.CodeInvalidRequest},
		{"GET", base + "?limit=x", "", 400, protocol.CodeInvalidRequest},
		{"GET", s1 + "/events?after=x", "", 400, protocol.CodeInvalidRequest},
		{"POST", base, `{"id":"s1","model":"codex/gpt-6-sol"}`, 409, protocol.CodeSessionExists},
		{"POST", base, `{"model":"nope/x"}`, 409, protocol.CodeModelUnavailable},
		{"GET", base + "/s2", "", 404, protocol.CodeSessionNotFound},
		{"GET", strings.TrimSuffix(base, "/sessions") + "/nope", "", 404, protocol.CodeInvalidRequest},
		{"DELETE", s1, "", 405, protocol.CodeInvalidRequest},
		{"PATCH", s1, `{"model":"codex/no-such-model"}`, 409, protocol.CodeModelUnavailable},
		{"POST", s1 + "/inputs", `{"id":"a","parts":[{"type":"text","text":"other"}]}`, 409, protocol.CodeInputConflict},
		{"POST", s1 + "/interrupt", `{"turn_id":"turn_x"}`, 409, protocol.CodeTurnMismatch},
		{"POST", s1 + "/inputs", `{"id":"b","parts":[{"type":"text","text":"` + strings.Repeat("x", 9<<20) + `"}]}`, 413, protocol.CodePayloadTooLarge},
		{"CLOSE", base, `{"model":"codex/gpt-6-sol"}`, 503, protocol.CodeDraining},
	} {
		if tc.method == "CLOSE" {
			if err := r.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			tc.method = "POST"
		}
		var got protocol.ErrorBody
		status := call(t, tc.method, tc.url, tc.body, &got)
		if status != tc.status || got.Error.Code != tc.code || got.Error.Message == "" || got.Error.Details == nil {
			t.Errorf("%s %s %.40s = %d %+v, want %d %s", tc.method, tc.url, tc.body, status, got, tc.status, tc.code)
		}
	}
}

// stub is a Runtime and a Session with scripted results.
type stub struct {
	openErr error
	head    uint64
	receipt protocol.Admitted
	repeat  bool
}

func (s stub) Create(context.Context, protocol.CreateSession) (stub, error) { return s, nil }
func (s stub) Open(context.Context, string) (stub, error)                   { return s, s.openErr }
func (stub) List(context.Context, protocol.ListSessions) (protocol.SessionPage, error) {
	return protocol.SessionPage{}, nil
}
func (stub) Models() []protocol.Model                            { return nil }
func (s stub) View() protocol.Session                            { return protocol.Session{HeadSeq: s.head} }
func (stub) Interrupt(context.Context, protocol.Interrupt) error { return nil }
func (stub) Compact(context.Context) error                       { return nil }
func (stub) SetGoal(context.Context, protocol.Goal) error        { return nil }
func (stub) ClearGoal(context.Context) error                     { return nil }
func (s stub) Update(context.Context, protocol.SettingsPatch) (protocol.Session, error) {
	return s.View(), nil
}
func (stub) Events(context.Context, uint64) iter.Seq2[protocol.Event, error] { return nil }
func (s stub) Admit(context.Context, protocol.Input) (protocol.Admitted, bool, error) {
	return s.receipt, s.repeat, nil
}

func TestInternalErrorHidesItsCause(t *testing.T) {
	srv := httptest.NewServer(server.New(stub{openErr: errors.New("dial postgres://user:hunter2@db")}, nil))
	t.Cleanup(srv.Close)
	var got protocol.ErrorBody
	want(t, "status", call(t, "GET", srv.URL+"/sessions/s1", "", &got), http.StatusInternalServerError)
	if got.Error.Code != protocol.CodeInternal || strings.Contains(got.Error.Message, "hunter2") {
		t.Errorf("error = %+v, want code internal and no cause", got.Error)
	}
}

func TestSubmitStatusFollowsTheSessionVerdict(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		status := http.StatusCreated
		if repeat {
			status = http.StatusOK
		}
		srv := httptest.NewServer(server.New(stub{head: 9, receipt: protocol.Admitted{InputID: "a", Seq: 5}, repeat: repeat}, nil))
		t.Cleanup(srv.Close)
		want(t, fmt.Sprintf("status with repeat=%v", repeat),
			call(t, "POST", srv.URL+"/sessions/s1/inputs", `{"id":"a","parts":[{"type":"text","text":"hi"}]}`, nil), status)
	}
}

func TestEventsAcceptNegotiation(t *testing.T) {
	_, s1 := serve(t, nil)
	for accept, ct := range map[string]string{
		"text/event-stream":                        "text/event-stream",
		"TEXT/Event-Stream":                        "text/event-stream",
		"application/json, text/event-stream":      "text/event-stream",
		"text/event-streaming":                     "application/json",
		"text/event-stream;q=0":                    "application/json",
		"text/event-stream; q=0, application/json": "application/json",
		"*/*": "application/json",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, "GET", s1+"/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Accept", accept)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		want(t, "content type for Accept "+accept, resp.Header.Get("Content-Type"), ct)
		cancel()
		_ = resp.Body.Close()
	}
}
