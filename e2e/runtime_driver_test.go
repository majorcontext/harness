package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// runtimeDriver drives harness.Runtime in process over Runtime.Handler, on
// a DiskStore, with the config that serve would read.
type runtimeDriver struct {
	store, workDir string
	cfg            config.Config
	rt             *harness.Runtime
	srv            *httptest.Server
	inputs         int
	created        int
}

// runtimeKey gives the in-process runtime the model key that startServeIn
// gives serve.
var runtimeKey = sync.OnceFunc(func() { _ = os.Setenv("ANTHROPIC_API_KEY", codexAPIKey) })

func newRuntimeDriver(t *testing.T, modelURL string, extra map[string]any) *runtimeDriver {
	t.Helper()
	cfg := map[string]any{"context_window_tokens": 1_000_000}
	maps.Copy(cfg, extra)
	return newRuntimeDriverIn(t, t.TempDir(), writeGoalConfigWith(t, modelURL, cfg))
}

func newRuntimeDriverIn(t *testing.T, workDir, configPath string) *runtimeDriver {
	t.Helper()
	runtimeKey()
	c, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d := &runtimeDriver{store: t.TempDir(), workDir: workDir, cfg: *c}
	d.start(t)
	t.Cleanup(func() { d.stop(t, context.Background()) })
	return d
}

func (d *runtimeDriver) start(t *testing.T) {
	t.Helper()
	rt, err := harness.New(harness.Options{Store: harness.NewDiskStore(d.store), Config: d.cfg, WorkDir: d.workDir})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	d.rt, d.srv = rt, httptest.NewServer(rt.Handler())
}

// stop closes the runtime first, which ends its event streams, then the server.
func (d *runtimeDriver) stop(t *testing.T, ctx context.Context) {
	t.Helper()
	if d.rt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, waitBound)
	defer cancel()
	err := d.rt.Close(ctx)
	d.srv.Close()
	d.rt, d.srv = nil, nil
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("close runtime: %v", err)
	}
}

// Restart with kill closes the runtime under an ended context, so it stops
// each session with no further append, as a killed process would.
func (d *runtimeDriver) Restart(t *testing.T, kill bool) {
	t.Helper()
	ctx := context.Background()
	if kill {
		c, cancel := context.WithCancel(ctx)
		cancel()
		ctx = c
	}
	d.stop(t, ctx)
	d.start(t)
}

func (d *runtimeDriver) Stderr() string { return "(the runtime runs in the test process)" }

func (d *runtimeDriver) Workdir() string { return d.workDir }

func (d *runtimeDriver) send(t *testing.T, ctx context.Context, method, path string, body any, header http.Header) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	maps.Copy(req.Header, header)
	resp, err := d.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (d *runtimeDriver) do(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound+waitMargin)
	defer cancel()
	resp := d.send(t, ctx, method, path, body, nil)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, data
}

func (d *runtimeDriver) expect(t *testing.T, want int, method, path string, body, out any) {
	t.Helper()
	status, data := d.do(t, method, path, body)
	if status != want {
		t.Fatalf("%s %s: status %d, want %d, body %s", method, path, status, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
}

func (d *runtimeDriver) call(t *testing.T, method, path string, body any) callResult {
	t.Helper()
	status, data := d.do(t, method, path, body)
	return callResult{Status: status, Body: decodeBody(t, method+" "+path, data)}
}

// deleted is the result of a call whose route the spec deletes. The runtime
// driver never sends it.
func deleted(route string) callResult {
	return callResult{Body: map[string]any{"deleted_by_design": route}}
}

func (d *runtimeDriver) view(t *testing.T, id string) protocol.Session {
	t.Helper()
	var v protocol.Session
	d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id, nil, &v)
	return v
}

// Create mints the session ID in creation order, so GET /sessions, which
// lists in ID order, lists in creation order as serve did.
func (d *runtimeDriver) Create(t *testing.T) string {
	t.Helper()
	d.created++
	var v protocol.Session
	d.expect(t, http.StatusCreated, http.MethodPost, "/sessions", map[string]any{"id": fmt.Sprintf("ses_%04d", d.created)}, &v)
	return v.ID
}

// input returns the route and body of an input. serve ran a prompt that
// arrived during a turn at the next tool boundary, which is a steer input.
// An enqueued input waits for the next turn.
func (d *runtimeDriver) input(id, text, delivery, source string) (string, map[string]any) {
	d.inputs++
	body := map[string]any{"id": fmt.Sprintf("in%d", d.inputs), "delivery": delivery,
		"parts": []protocol.Part{{Type: protocol.PartText, Text: text}}}
	if source != "" {
		body["source"] = source
	}
	return "/sessions/" + id + "/inputs", body
}

func (d *runtimeDriver) Submit(t *testing.T, id, text string) {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliverySteer, "")
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) Enqueue(t *testing.T, id, text string) {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliveryQueue, "")
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) Send(t *testing.T, id, text string) callResult {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliverySteer, "")
	return d.call(t, http.MethodPost, path, body)
}

func (d *runtimeDriver) Command(t *testing.T, id, text string) callResult {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliveryQueue, protocol.SourceTyped)
	return d.call(t, http.MethodPost, path, body)
}

func (d *runtimeDriver) Interrupt(t *testing.T, id string) {
	t.Helper()
	d.expect(t, http.StatusNoContent, http.MethodPost, "/sessions/"+id+"/interrupt", map[string]any{}, nil)
}

func (d *runtimeDriver) CancelTree(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/sessions/"+id+"/interrupt", map[string]any{"tree": true})
}

// SetGoal fails a deferred goal: the spec deletes deferred goals, so a row
// that sets one is not run on the runtime.
func (d *runtimeDriver) SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool) {
	t.Helper()
	if deferred {
		t.Fatal("a deferred goal is deleted by design")
	}
	d.expect(t, http.StatusOK, http.MethodPut, "/sessions/"+id+"/goal", protocol.Goal{Condition: condition, MaxTurns: maxTurns}, nil)
}

func (d *runtimeDriver) UpdateGoal(t *testing.T, id, condition string) callResult {
	t.Helper()
	return d.call(t, http.MethodPut, "/sessions/"+id+"/goal", protocol.Goal{Condition: condition})
}

func (d *runtimeDriver) ClearGoal(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/sessions/"+id+"/goal", nil)
}

func (d *runtimeDriver) Compact(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/sessions/"+id+"/compact", map[string]any{})
}

func (d *runtimeDriver) patch(t *testing.T, id, key, value string) callResult {
	t.Helper()
	return d.call(t, http.MethodPatch, "/sessions/"+id, map[string]any{key: value})
}

func (d *runtimeDriver) SetModel(t *testing.T, id, model string) callResult {
	t.Helper()
	return d.patch(t, id, "model", model)
}

func (d *runtimeDriver) SetThinking(t *testing.T, id, level string) callResult {
	t.Helper()
	return d.patch(t, id, "effort", level)
}

func (d *runtimeDriver) SetServiceTier(t *testing.T, id, tier string) callResult {
	t.Helper()
	return d.patch(t, id, "service_tier", tier)
}

func (d *runtimeDriver) EndSession(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/sessions/"+id, nil)
}

// DeleteQueued withdraws each queued input and reports the last answer.
func (d *runtimeDriver) DeleteQueued(t *testing.T, id string) callResult {
	t.Helper()
	res := callResult{Status: http.StatusNoContent}
	for _, in := range d.view(t, id).Queued {
		res = d.call(t, http.MethodDelete, "/sessions/"+id+"/inputs/"+in, nil)
	}
	return res
}

func (d *runtimeDriver) GetSession(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/sessions/"+id, nil)
}

func (d *runtimeDriver) ListSessions(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/sessions", nil)
}

func (d *runtimeDriver) SessionStatus(t *testing.T) callResult {
	t.Helper()
	return deleted("GET /session/status")
}

func (d *runtimeDriver) MessagesPage(t *testing.T, id string, beforeSeq, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/sessions/"+id+"/messages", "before", beforeSeq, "limit", limit), nil)
}

func (d *runtimeDriver) Bootstrap(t *testing.T, id string, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/sessions/"+id+"/messages", "limit", limit), nil)
}

// JournalPage reads the events from seq from on: the page after from-1.
func (d *runtimeDriver) JournalPage(t *testing.T, id string, from, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/sessions/"+id+"/events", "after", max(from-1, 0), "limit", limit), nil)
}

func (d *runtimeDriver) Commands(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/commands", nil)
}

// events reads every durable event of session id, page by page.
func (d *runtimeDriver) events(t *testing.T, id string) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for after := uint64(0); ; {
		var page protocol.EventPage
		d.expect(t, http.StatusOK, http.MethodGet, fmt.Sprintf("/sessions/%s/events?after=%d&limit=1000", id, after), nil, &page)
		out = append(out, page.Events...)
		if page.Next == 0 {
			return out
		}
		after = page.Next
	}
}

func (d *runtimeDriver) Messages(t *testing.T, id string) []transcriptMessage {
	t.Helper()
	return transcriptOfLog(t, d.events(t, id))
}

func (d *runtimeDriver) sessionIDs(t *testing.T) []string {
	t.Helper()
	var ids []string
	for after := ""; ; {
		var page protocol.SessionPage
		d.expect(t, http.StatusOK, http.MethodGet, "/sessions?limit=1000&after="+url.QueryEscape(after), nil, &page)
		for _, s := range page.Sessions {
			ids = append(ids, s.ID)
		}
		if page.Next == "" {
			return ids
		}
		after = page.Next
	}
}

func (d *runtimeDriver) Journals(t *testing.T) [][]journalEntry {
	t.Helper()
	var out [][]journalEntry
	for _, id := range d.sessionIDs(t) {
		out = append(out, journalOfLog(t, d.events(t, id)))
	}
	return out
}

func (d *runtimeDriver) Queued(t *testing.T, id string) []string {
	t.Helper()
	texts := map[string]string{}
	for _, ev := range d.events(t, id) {
		if ev.Kind == "input.admitted" {
			in := decodeEvent[logInput](t, ev)
			texts[in.InputID] = partsText(in.Parts)
		}
	}
	var out []string
	for _, in := range d.view(t, id).Queued {
		out = append(out, texts[in])
	}
	return out
}

func decodeBody(t *testing.T, label string, data []byte) any {
	t.Helper()
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: decode body: %v (%s)", label, err, data)
	}
	return v
}

func decodeEvent[T any](t *testing.T, ev protocol.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(ev.Data, &v); err != nil {
		t.Fatalf("decode %s event %d: %v (%s)", ev.Kind, ev.Seq, err, ev.Data)
	}
	return v
}

// stream reads the SSE events of session id after seq after, and passes
// each to visit until it returns true. The read fails at waitBound.
func (d *runtimeDriver) stream(t *testing.T, id string, after uint64, header bool, visit func(sseID string, ev protocol.Event) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	h := http.Header{"Accept": {"text/event-stream"}}
	path := "/sessions/" + id + "/events"
	if header {
		h.Set("Last-Event-ID", strconv.FormatUint(after, 10))
	} else {
		path += "?after=" + strconv.FormatUint(after, 10)
	}
	resp := d.send(t, ctx, http.MethodGet, path, nil, h)
	defer func() { _ = resp.Body.Close() }()
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			t.Fatalf("event stream of %s ended: %v", id, err)
		}
		var ev protocol.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode frame %s: %v", raw, err)
		}
		if visit(sc.id, ev) {
			return
		}
	}
}

// settled reports whether a session runs nothing and has nothing to run.
func settled(v protocol.Session) bool {
	return v.Status == protocol.StatusIdle && len(v.Queued) == 0 && (v.Goal == nil || v.Goal.State != "active")
}

// WaitIdle reads the view again on each frame of the session, from the head
// that the first view saw, until the session has settled.
func (d *runtimeDriver) WaitIdle(t *testing.T, id string) {
	t.Helper()
	v := d.view(t, id)
	if settled(v) {
		return
	}
	d.stream(t, id, v.HeadSeq, false, func(string, protocol.Event) bool { return settled(d.view(t, id)) })
}

func (d *runtimeDriver) AwaitGoalExhausted(t *testing.T) {
	t.Helper()
	for _, id := range d.sessionIDs(t) {
		if g := d.view(t, id).Goal; g == nil {
			continue
		}
		d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
			return ev.Kind == "goal.changed" && decodeEvent[struct{ State string }](t, ev).State == "exhausted"
		})
		return
	}
	t.Fatal("no session has a goal")
}

// Child waits on the events of the parent until it has spawned more than
// nth children, and returns the nth in spawn order.
func (d *runtimeDriver) Child(t *testing.T, parentID string, nth int) string {
	t.Helper()
	var ids []string
	d.stream(t, parentID, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind == "child.spawned" && !ev.Ephemeral {
			ids = append(ids, decodeEvent[struct {
				ChildID string `json:"child_id"`
			}](t, ev).ChildID)
		}
		return len(ids) > nth
	})
	return ids[nth]
}

// SSEResume reads the durable frames of one session after afterSeq, up to
// its head on entry. A read of every session is the deleted box-global stream.
func (d *runtimeDriver) SSEResume(t *testing.T, id string, afterSeq int64, header, scoped bool) callResult {
	t.Helper()
	if id == "" {
		return deleted("GET /event")
	}
	head := d.view(t, id).HeadSeq
	body := []any{}
	if uint64(afterSeq) < head {
		d.stream(t, id, uint64(afterSeq), header, func(sseID string, ev protocol.Event) bool {
			if ev.Ephemeral {
				return false
			}
			body = append(body, map[string]any{"id": sseID, "type": ev.Kind, "seq": ev.Seq})
			return ev.Seq >= head
		})
	}
	return callResult{Status: http.StatusOK, Body: map[string]any{"frames": body}}
}

func partsText(parts []logPart) string {
	var texts []string
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
}

var _ driver = (*runtimeDriver)(nil)
