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
	"path/filepath"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// runtimeDriver drives harness.Runtime over its HTTP routes, on a DiskStore:
// in process, over Runtime.Handler with the config that serve would read, or
// through the serve binary.
type runtimeDriver struct {
	store, workDir string
	ask            bool
	cfg            config.Config
	rt             *harness.Runtime
	srv            *httptest.Server
	// serve is true when the driver runs the serve binary, as proc, which it
	// restarts on the same store.
	serve      bool
	proc       *serveProc
	configPath string
	env        map[string]string
	args       []string
	client     *http.Client
	inputs     int
	lastInput  map[string]string
	lastTyped  map[string]string
}

// runtimeVersion is the build version that serve reports in its engine banner.
const runtimeVersion = "0.1.0-dev"

// runtimeKey gives the in-process runtime the model key that startServeIn
// gives serve.
var runtimeKey = sync.OnceFunc(func() { _ = os.Setenv("ANTHROPIC_API_KEY", codexAPIKey) })

// newRuntimeDriverIn runs the runtime in workDir. An empty workDir gives it no WorkDir.
// A .harness.json in workDir joins the config as the project layer, as an
// embedder loads it with config.LoadProject; the row then runs alone, because
// the load reads the user config path from the process environment.
func newRuntimeDriverIn(t *testing.T, configPath string, ask bool, workDir string) *runtimeDriver {
	t.Helper()
	runtimeKey()
	c, err := config.Load(configPath)
	if _, statErr := os.Stat(filepath.Join(workDir, ".harness.json")); workDir != "" && statErr == nil {
		t.Setenv("HARNESS_CONFIG", configPath)
		c, err = config.LoadProject(workDir)
	}
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	d := &runtimeDriver{store: t.TempDir(), workDir: workDir, cfg: *c, ask: ask, lastInput: map[string]string{}, lastTyped: map[string]string{}}
	d.start(t)
	t.Cleanup(func() { d.stop(t, context.Background()) })
	return d
}

// newServeDriverIn runs the serve binary in workDir, on the config at
// configPath, with env added to its environment and args after its flags.
func newServeDriverIn(t *testing.T, configPath string, env map[string]string, workDir string, args ...string) *runtimeDriver {
	t.Helper()
	d := &runtimeDriver{store: t.TempDir(), workDir: workDir, serve: true, configPath: configPath, env: env, args: args, client: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		lastInput: map[string]string{}, lastTyped: map[string]string{}}
	d.start(t)
	return d
}

func (d *runtimeDriver) startProc(t *testing.T) {
	t.Helper()
	env := map[string]string{"HARNESS_SESSION_DIR": d.store, "HARNESS_CONFIG": d.configPath, "ANTHROPIC_API_KEY": "e2e-dummy-key"}
	maps.Copy(env, d.env)
	d.proc = startServeProc(t, freeAddr, d.workDir, env, d.args...)
}

func (d *runtimeDriver) start(t *testing.T) {
	t.Helper()
	if d.serve {
		d.startProc(t)
		return
	}
	rt, err := harness.New(harness.Options{Store: harness.NewDiskStore(d.store), Config: d.cfg, WorkDir: d.workDir, Version: runtimeVersion, AskUserQuestion: d.ask})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	d.rt, d.srv = rt, httptest.NewServer(rt.Handler())
	if err := rt.CatchUp(t.Context()); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	d.openAll(t)
}

// stop closes the runtime first, which ends its event streams, then the server.
func (d *runtimeDriver) stop(t *testing.T, ctx context.Context) {
	t.Helper()
	if d.serve {
		return
	}
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

// Restart closes the runtime and opens a new one on the same store. A kill
// copies the store first, then closes the old runtime on its own directory
// under an ended context, and opens the new runtime on the copy, so nothing
// that the old runtime does after the kill point reaches the next owner, as
// with SIGKILL of serve.
func (d *runtimeDriver) Restart(t *testing.T, kill bool) {
	t.Helper()
	if d.serve {
		if kill {
			d.proc.kill()
		} else {
			d.proc.terminate(t)
		}
		d.startProc(t)
		return
	}
	if !kill {
		d.stop(t, context.Background())
		d.start(t)
		return
	}
	next := t.TempDir()
	if err := os.CopyFS(next, os.DirFS(d.store)); err != nil {
		t.Fatalf("copy store: %v", err)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	d.stop(t, ended)
	d.store = next
	d.start(t)
}

func (d *runtimeDriver) Stderr() string {
	if d.serve {
		return d.proc.stderr.String()
	}
	return "(the runtime runs in the test process)"
}

func (d *runtimeDriver) Workdir() string { return d.workDir }

// endpoint is the base URL and client of the host.
func (d *runtimeDriver) endpoint() (string, *http.Client) {
	if d.serve {
		return "http://" + d.proc.addr, d.client
	}
	return d.srv.URL, d.srv.Client()
}

// authorize adds the run token that serve requires.
func (d *runtimeDriver) authorize(req *http.Request) {
	if d.serve {
		req.Header.Set("Authorization", "Bearer "+d.proc.token)
	}
}

func (d *runtimeDriver) send(t *testing.T, ctx context.Context, method, path string, body any, header http.Header) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := encodeBody(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	base, client := d.endpoint()
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	d.authorize(req)
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v\nstderr:\n%s", method, path, err, d.Stderr())
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
	res := callResult{Status: status, Body: decodeBody(t, method+" "+path, data)}
	if v, ok := res.Body.(map[string]any); ok {
		if sub, ok := v["subscription_usage"].(map[string]any); ok && sub["captured_at"] != json.Number("0") {
			sub["captured_at"] = "<time>"
		}
	}
	return res
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

func (d *runtimeDriver) Create(t *testing.T) string {
	t.Helper()
	var v protocol.Session
	d.expect(t, http.StatusCreated, http.MethodPost, "/sessions", map[string]any{}, &v)
	return v.ID
}

// input returns the route and body of an input. An empty delivery leaves the
// field out, so the default applies: serve ran a prompt that arrived during a
// turn at the next tool boundary, and so does a steer input.
func (d *runtimeDriver) input(id, text, delivery, source string) (string, map[string]any) {
	d.inputs++
	d.lastInput[id] = fmt.Sprintf("in%d", d.inputs)
	if source == protocol.SourceTyped {
		d.lastTyped[id] = d.lastInput[id]
	}
	return d.inputAs(id, d.lastInput[id], text, delivery, source)
}

// inputAs is input with the id of the input given.
func (d *runtimeDriver) inputAs(id, inputID, text, delivery, source string) (string, map[string]any) {
	body := map[string]any{"id": inputID,
		"parts": []protocol.Part{{Type: protocol.PartText, Text: text}}}
	if delivery != "" {
		body["delivery"] = delivery
	}
	if source != "" {
		body["source"] = source
	}
	return "/sessions/" + id + "/inputs", body
}

func (d *runtimeDriver) Submit(t *testing.T, id, text string) {
	t.Helper()
	path, body := d.input(id, text, "", "")
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) Attach(t *testing.T, id, text string, atts []attachment) {
	t.Helper()
	path, body := d.input(id, text, "", "")
	parts := body["parts"].([]protocol.Part)
	for _, a := range atts {
		parts = append(parts, protocol.Part{Type: protocol.PartBlob, MediaType: a.mediaType, Data: a.data})
	}
	body["parts"] = parts
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) Enqueue(t *testing.T, id, text string) {
	t.Helper()
	path, body := d.input(id, text, "", "")
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) EnqueueNext(t *testing.T, id, text string) {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliveryQueue, "")
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
}

func (d *runtimeDriver) Send(t *testing.T, id, text string) callResult {
	t.Helper()
	path, body := d.input(id, text, "", "")
	return d.call(t, http.MethodPost, path, body)
}

func (d *runtimeDriver) Command(t *testing.T, id, text string, _ bool) callResult {
	t.Helper()
	path, body := d.input(id, text, protocol.DeliveryQueue, protocol.SourceTyped)
	return d.call(t, http.MethodPost, path, body)
}

func (d *runtimeDriver) CreateModel(t *testing.T, model string) callResult {
	t.Helper()
	body := map[string]any{}
	if model != "" {
		body["model"] = model
	}
	return d.call(t, http.MethodPost, "/sessions", body)
}

func (d *runtimeDriver) PostInput(t *testing.T, id, text string) callResult {
	t.Helper()
	return d.Send(t, id, text)
}

func (d *runtimeDriver) Repeat(t *testing.T, id, text string, typed bool) callResult {
	t.Helper()
	delivery, source, inputID := "", "", d.lastInput[id]
	if typed {
		delivery, source, inputID = protocol.DeliveryQueue, protocol.SourceTyped, d.lastTyped[id]
	}
	path, body := d.inputAs(id, inputID, text, delivery, source)
	return d.call(t, http.MethodPost, path, body)
}

func (d *runtimeDriver) SteerOtherTurn(t *testing.T, id, text string) callResult {
	t.Helper()
	d.inputs++
	path, body := d.inputAs(id, fmt.Sprintf("in%d", d.inputs), text, protocol.DeliverySteer, "")
	body["expected_turn_id"] = "turn_other"
	return d.call(t, http.MethodPost, path, body)
}

// commandList reads the newest status of each typed command of a session from
// its log, in the order of the first record of each, with the head seq that it
// read, and reports whether one still runs.
func (d *runtimeDriver) commandList(t *testing.T, id string) (recs []any, head uint64, running bool) {
	t.Helper()
	type record struct {
		InputID string          `json:"input_id"`
		Line    string          `json:"line"`
		Name    string          `json:"name"`
		Status  string          `json:"status"`
		Text    string          `json:"text"`
		Result  json.RawMessage `json:"result"`
	}
	var order []string
	newest := map[string]record{}
	for _, ev := range d.events(t, id) {
		head = ev.Seq
		if ev.Kind != "command.recorded" {
			continue
		}
		r := decodeEvent[record](t, ev)
		if _, ok := newest[r.InputID]; !ok {
			order = append(order, r.InputID)
		}
		newest[r.InputID] = r
	}
	recs = []any{}
	for _, in := range order {
		r := newest[in]
		rec := map[string]any{"line": r.Line, "name": r.Name, "status": r.Status}
		if r.Text != "" {
			rec["text"] = r.Text
		}
		if len(r.Result) > 0 {
			rec["result"] = decodeBody(t, "command result", r.Result)
		}
		running = running || r.Status == protocol.CommandAccepted
		recs = append(recs, rec)
	}
	return recs, head, running
}

func (d *runtimeDriver) AwaitCommands(t *testing.T, id string) {
	t.Helper()
	for {
		_, head, running := d.commandList(t, id)
		if !running {
			return
		}
		d.stream(t, id, head, false, func(_ string, ev protocol.Event) bool { return ev.Kind == "command.recorded" })
	}
}

func (d *runtimeDriver) CommandRecords(t *testing.T, id string) callResult {
	t.Helper()
	recs, _, _ := d.commandList(t, id)
	return callResult{Status: http.StatusOK, Body: recs}
}

func (d *runtimeDriver) Models(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/models", nil)
}

func (d *runtimeDriver) AwaitTurnEnd(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitBound)
	defer cancel()
	base, client := d.endpoint()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sessions/"+id+"/events?after=0", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	d.authorize(req)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			return fmt.Errorf("event stream of %s ended: %w", id, err)
		}
		var ev protocol.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("decode frame %s: %w", raw, err)
		}
		if ev.Kind == "turn.ended" {
			return nil
		}
	}
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

// notInEngine is the result of a call whose route or field only the runtime
// has. Serve never receives it.
func notInEngine(what string) callResult {
	return callResult{Body: map[string]any{"not_in_engine": what}}
}

// notServed fails a row that reaches a route that Runtime.Handler does not
// serve yet, so no row runs a guess at a route that its phase has not built.
func notServed(t *testing.T, route, phase string) callResult {
	t.Helper()
	t.Fatalf("Runtime.Handler does not serve %s until %s", route, phase)
	return callResult{}
}

func (d *runtimeDriver) EndSession(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/sessions/"+id, nil)
}

// DeleteQueued withdraws each queued input, as serve cleared the queue in one
// call, and records the status of the last call.
func (d *runtimeDriver) DeleteQueued(t *testing.T, id string) callResult {
	t.Helper()
	var queued []string
	d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id+"/inputs", nil, &queued)
	res := callResult{Status: http.StatusNoContent}
	for _, in := range queued {
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

// JournalPage reads the page after the cursor from, as serve read the page
// at its next_cursor.
func (d *runtimeDriver) JournalPage(t *testing.T, id string, from, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/sessions/"+id+"/events", "after", from, "limit", limit), nil)
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
	if dec.More() {
		return string(bytes.TrimSpace(data))
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

var _ driver = (*runtimeDriver)(nil)
