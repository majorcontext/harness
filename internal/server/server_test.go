package server_test

import (
	"bufio"
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

// serve serves the handler of a Runtime on a MemStore whose codex and
// openai providers are a scripted Codex server, and creates session s1.
func serve(t *testing.T, steps ...harnesstest.Step) (*harness.Runtime, string) {
	t.Helper()
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...)
	t.Setenv("HARNESS_TEST_CODEX_KEY", "k")
	retries := 0
	p := config.Provider{Type: config.TypeOpenAI, APIKeyEnv: "HARNESS_TEST_CODEX_KEY", BaseURL: o.URL() + "/backend-api/codex",
		ResponsesPath: "/responses", OmitResponseParams: []string{"max_output_tokens"}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(),
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

// TestLiveFramesCarryNoSSEID pins spec HTTP > Events: only the SSE frames of
// durable records carry id: <seq>, and a live frame carries the last durable
// seq. A golden holds no live frame, so no contract row reaches this.
func TestLiveFramesCarryNoSSEID(t *testing.T) {
	_, s1 := serve(t, harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
	live := subscribe(t, s1+"/events", "")
	want(t, "first frames", live.until(t, "owner.acquired"), []string{"1 session.created", "2 owner.acquired"})
	var got protocol.Admitted
	want(t, "submit status", call(t, "POST", s1+"/inputs", `{"id":"a","parts":[{"type":"text","text":"hi"}]}`, &got), http.StatusCreated)
	want(t, "turn frames", live.until(t, "turn.ended"),
		[]string{"3 input.admitted", "4 turn.started", "~4 item.started", "~4 item.delta text hello", "5 context.measured", "6 item.completed", "7 turn.ended"})
}

func TestErrorsOverHTTP(t *testing.T) {
	r, s1 := serve(t, harnesstest.Step{Name: "hi", Match: harnesstest.LastUserText("hi"), Reply: harnesstest.Reply{Text: "hello"}})
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
		{"POST", base, `{"model":"nope/no-such-model"}`, 409, protocol.CodeModelUnavailable},
		{"POST", base, `{"model":`, 400, protocol.CodeInvalidRequest},
		{"POST", base, `{"model":"codex/gpt-6-sol","modle":"x"}`, 400, protocol.CodeInvalidRequest},
		{"GET", base + "?limit=x", "", 400, protocol.CodeInvalidRequest},
		{"GET", s1 + "/events?after=x", "", 400, protocol.CodeInvalidRequest},
		{"POST", base, `{"id":"s1","model":"codex/gpt-6-sol"}`, 409, protocol.CodeSessionExists},
		{"GET", base + "/s2", "", 404, protocol.CodeSessionNotFound},
		{"GET", strings.TrimSuffix(base, "/sessions") + "/nope", "", 404, protocol.CodeInvalidRequest},
		{"PUT", s1, "", 405, protocol.CodeInvalidRequest},
		{"PATCH", s1, `{"model":"codex/no-such-model"}`, 409, protocol.CodeModelUnavailable},
		{"POST", s1 + "/inputs", `{"id":"a","parts":[{"type":"text","text":"other"}]}`, 409, protocol.CodeInputConflict},
		{"POST", s1 + "/interrupt", `{"turn_id":"turn_x"}`, 409, protocol.CodeTurnMismatch},
		{"POST", s1 + "/inputs", `{"id":"b","parts":[{"type":"text","text":"` + strings.Repeat("x", 33<<20) + `"}]}`, 413, protocol.CodePayloadTooLarge},
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
	receipt protocol.Admitted
}

func (stub) Resolve(context.Context, string, protocol.Resolution) (protocol.Resolved, error) {
	return protocol.Resolved{}, nil
}

func (s stub) Create(context.Context, protocol.CreateSession) (stub, error) { return s, nil }
func (s stub) Open(context.Context, string) (stub, error)                   { return s, s.openErr }
func (stub) End(context.Context, string) error                              { return nil }
func (s stub) Read(context.Context, string) (server.Reader, error)          { return reader{s}, s.openErr }
func (stub) List(context.Context, protocol.ListSessions) (protocol.SessionPage, error) {
	return protocol.SessionPage{}, nil
}
func (stub) Models() []protocol.Model                            { return nil }
func (stub) Commands() (protocol.Commands, error)                { return protocol.Commands{}, nil }
func (s stub) Withdraw(context.Context, string) error            { return nil }
func (stub) View() protocol.Session                              { return protocol.Session{} }
func (stub) Interrupt(context.Context, protocol.Interrupt) error { return nil }
func (stub) Compact(context.Context, protocol.Compact) (protocol.Compacted, error) {
	return protocol.Compacted{}, nil
}
func (stub) SetGoal(context.Context, protocol.Goal) error { return nil }
func (stub) ClearGoal(context.Context) error              { return nil }
func (s stub) Update(context.Context, protocol.SettingsPatch) (protocol.Session, error) {
	return s.View(), nil
}
func (stub) Events(context.Context, uint64) iter.Seq2[protocol.Event, error] { return nil }
func (s stub) Submit(context.Context, protocol.Input) (protocol.Admitted, error) {
	return s.receipt, nil
}

// reader is a stub as a server.Reader.
type reader struct{ stub }

func (r reader) Session() protocol.Session { return r.View() }

func (reader) Messages(context.Context, uint64, int) (protocol.MessagePage, error) {
	return protocol.MessagePage{}, nil
}

func TestInternalErrorHidesItsCause(t *testing.T) {
	srv := httptest.NewServer(server.New(stub{openErr: errors.New("dial postgres://user:hunter2@db")}, server.Options{}))
	t.Cleanup(srv.Close)
	var got protocol.ErrorBody
	want(t, "status", call(t, "GET", srv.URL+"/sessions/s1", "", &got), http.StatusInternalServerError)
	if got.Error.Code != protocol.CodeInternal || strings.Contains(got.Error.Message, "hunter2") {
		t.Errorf("error = %+v, want code internal and no cause", got.Error)
	}
}

func TestAnInputBodyMayHoldAnAttachmentOfTheAdmissionLimit(t *testing.T) {
	srv := httptest.NewServer(server.New(stub{receipt: protocol.Admitted{InputID: "a", Seq: 5}}, server.Options{}))
	t.Cleanup(srv.Close)
	body := `{"id":"a","parts":[{"type":"blob","media_type":"application/pdf","data":"` + strings.Repeat("QUJD", 12<<18) + `"}]}`
	want(t, "an input of 12 MiB", call(t, "POST", srv.URL+"/sessions/s1/inputs", body, nil), http.StatusCreated)
	want(t, "another body of 12 MiB", call(t, "PATCH", srv.URL+"/sessions/s1", `{"model":"`+strings.Repeat("x", 12<<20)+`"}`, nil), http.StatusRequestEntityTooLarge)
}
