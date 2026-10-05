package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

type driver interface {
	Create(t *testing.T) string
	// CreateModel creates a session that names model and reports the response.
	CreateModel(t *testing.T, model string) callResult
	// PostInput admits an input with a new id and reports the receipt.
	PostInput(t *testing.T, id, text string) callResult
	// Repeat sends the newest input of the session again, under its id, with text as its body.
	Repeat(t *testing.T, id, text string, typed bool) callResult
	// SteerOtherTurn steers with an expected turn that is not running.
	SteerOtherTurn(t *testing.T, id, text string) callResult
	Models(t *testing.T) callResult
	// AwaitCommands returns once no typed command of the session is still running.
	AwaitCommands(t *testing.T, id string)
	// CommandRecords lists the newest status of each typed command of the session.
	CommandRecords(t *testing.T, id string) callResult
	Submit(t *testing.T, id, text string)
	// Attach submits text with the attachments after it.
	Attach(t *testing.T, id, text string, atts []attachment)
	Enqueue(t *testing.T, id, text string)
	// EnqueueNext enqueues text for the next turn. Serve has one queue, so it
	// is Enqueue there.
	EnqueueNext(t *testing.T, id, text string)
	WaitIdle(t *testing.T, id string)
	Interrupt(t *testing.T, id string)
	SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool)
	Messages(t *testing.T, id string) []transcriptMessage
	// Journals returns each event journal: one for the instance, or one
	// for each session where each session has its own seq.
	Journals(t *testing.T) [][]journalEntry
	Restart(t *testing.T, kill bool)
	Queued(t *testing.T, id string) []string
	AwaitGoalExhausted(t *testing.T)
	Stderr() string
	Workdir() string

	Compact(t *testing.T, id string) callResult
	SetModel(t *testing.T, id, model string) callResult
	SetThinking(t *testing.T, id, level string) callResult
	SetServiceTier(t *testing.T, id, tier string) callResult
	EndSession(t *testing.T, id string) callResult
	Send(t *testing.T, id, text string) callResult
	CancelTree(t *testing.T, id string) callResult
	DeleteQueued(t *testing.T, id string) callResult
	UpdateGoal(t *testing.T, id, condition string) callResult
	ClearGoal(t *testing.T, id string) callResult
	ListSessions(t *testing.T) callResult
	GetSession(t *testing.T, id string) callResult
	SessionStatus(t *testing.T) callResult
	MessagesPage(t *testing.T, id string, beforeSeq, limit int) callResult
	Bootstrap(t *testing.T, id string, limit int) callResult
	JournalPage(t *testing.T, id string, from, limit int) callResult
	SSEResume(t *testing.T, id string, afterSeq int64, header, scoped bool) callResult
	Child(t *testing.T, parentID string, nth int) string
	Command(t *testing.T, id, text string, repeatable bool) callResult
	Commands(t *testing.T) callResult
}

// promptParts sends text and then each attachment as one prompt.
func (p *serveProc) promptParts(id, text string, atts []attachment) {
	p.t.Helper()
	parts := []map[string]any{{"type": "text", "text": text}}
	for _, a := range atts {
		parts = append(parts, map[string]any{"type": "blob", "media_type": a.mediaType, "data": a.data})
	}
	body := map[string]any{"parts": parts}
	resp, data := p.do(http.MethodPost, "/session/"+id+"/prompt_async", body)
	if resp.StatusCode != http.StatusAccepted {
		p.t.Fatalf("prompt_async: status %d body %s", resp.StatusCode, data)
	}
}

// attachment is a file of a prompt.
type attachment struct {
	mediaType string
	data      []byte
}

// callResult is what a driver reports for a call: the status and the decoded
// body. A body that carries a transcript reports it in Messages, in the
// oracle's own vocabulary, and leaves the key out of Body.
type callResult struct {
	Status   int
	Body     any
	Messages []transcriptMessage
}

// waitBound is a failure bound for a wait on the serve process, not a delay.
// It is far above any real latency; a wait that reaches it fails the test.
var waitBound = 60 * time.Second

// waitMargin is the time a request may run past waitBound for the server to
// honor a timeout_s of waitBound and answer.
var waitMargin = 10 * time.Second

type httpDriver struct {
	sessDir, workDir, config string
	env                      map[string]string
	args                     []string
	p                        *serveProc
	enqSeq                   map[string]int64
	typedSeq                 map[string]int64
}

func newHTTPDriver(t *testing.T, modelURL string) *httpDriver {
	t.Helper()
	return newHTTPDriverWith(t, modelURL, nil)
}

func newHTTPDriverWith(t *testing.T, modelURL string, extra map[string]any) *httpDriver {
	t.Helper()
	return newHTTPDriverAt(t, writeGoalConfigWith(t, modelURL, scenarioConfig(extra)), nil)
}

// scenarioConfig is the served config of a scenario: extra over the defaults.
func scenarioConfig(extra map[string]any) map[string]any {
	cfg := map[string]any{"context_window_tokens": 1_000_000} // the modelmeta table is bot-refreshed
	maps.Copy(cfg, extra)
	return cfg
}

// newHTTPDriverAt starts serve on configPath with env added to its
// environment and args after its flags.
func newHTTPDriverAt(t *testing.T, configPath string, env map[string]string, args ...string) *httpDriver {
	t.Helper()
	d := &httpDriver{sessDir: t.TempDir(), workDir: t.TempDir(), config: configPath, env: env, args: args, enqSeq: map[string]int64{}, typedSeq: map[string]int64{}}
	d.p = d.serve(t)
	return d
}

func (d *httpDriver) serve(t *testing.T) *serveProc {
	t.Helper()
	env := map[string]string{"HARNESS_SESSION_DIR": d.sessDir, "HARNESS_CONFIG": d.config, "ANTHROPIC_API_KEY": "e2e-dummy-key"}
	maps.Copy(env, d.env)
	return startServeProc(t, freeAddr, d.workDir, env, d.args...)
}

func (d *httpDriver) expect(t *testing.T, want int, method, path string, body any) []byte {
	t.Helper()
	resp, data := d.p.do(method, path, body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d, body %s\nstderr:\n%s", method, path, resp.StatusCode, want, data, d.p.stderr.String())
	}
	return data
}

func (d *httpDriver) Create(t *testing.T) string {
	t.Helper()
	return d.p.createSession()
}

func (d *httpDriver) Submit(t *testing.T, id, text string) {
	t.Helper()
	d.p.prompt(id, text)
}

func (d *httpDriver) Attach(t *testing.T, id, text string, atts []attachment) {
	t.Helper()
	d.p.promptParts(id, text, atts)
}

func (d *httpDriver) Enqueue(t *testing.T, id, text string) {
	t.Helper()
	d.enqSeq[id]++
	if status, _ := d.p.enqueue(id, text, d.enqSeq[id]); status != http.StatusAccepted {
		t.Fatalf("enqueue on %s: status %d, want %d\nstderr:\n%s", id, status, http.StatusAccepted, d.Stderr())
	}
}

func (d *httpDriver) CreateModel(t *testing.T, model string) callResult {
	t.Helper()
	body := map[string]any{}
	if model != "" {
		body["model"] = model
	}
	return withoutSeq(d.call(t, http.MethodPost, "/session", body))
}

func (d *httpDriver) enqueueCall(t *testing.T, id, text string, seq int64, typed bool) callResult {
	t.Helper()
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}, "seq": seq}
	if typed {
		body["source"] = "typed"
	}
	return d.call(t, http.MethodPost, "/session/"+id+"/enqueue", body)
}

func (d *httpDriver) PostInput(t *testing.T, id, text string) callResult {
	t.Helper()
	d.enqSeq[id]++
	return d.enqueueCall(t, id, text, d.enqSeq[id], false)
}

func (d *httpDriver) Repeat(t *testing.T, id, text string, typed bool) callResult {
	t.Helper()
	if typed {
		if d.typedSeq[id] == 0 {
			return notInEngine("an input id that a typed command holds")
		}
		return d.enqueueCall(t, id, text, d.typedSeq[id], true)
	}
	if d.enqSeq[id] == 0 {
		return notInEngine("an input id that another input holds")
	}
	return d.enqueueCall(t, id, text, d.enqSeq[id], false)
}

func (d *httpDriver) SteerOtherTurn(t *testing.T, id, text string) callResult {
	t.Helper()
	return notInEngine("steer with expected_turn_id")
}

// commandList reads the typed commands of a session as the bootstrap lists
// them, and reports whether one still runs.
func (d *httpDriver) commandList(t *testing.T, id string) (recs []any, running bool) {
	t.Helper()
	body, _ := d.Bootstrap(t, id, 0).Body.(map[string]any)
	list, _ := body["commands"].([]any)
	recs = []any{}
	for _, c := range list {
		m, _ := c.(map[string]any)
		rec := map[string]any{}
		for _, k := range []string{"line", "name", "status", "text", "result"} {
			if v, ok := m[k]; ok {
				rec[k] = v
			}
		}
		running = running || m["status"] == "accepted"
		recs = append(recs, rec)
	}
	return recs, running
}

func (d *httpDriver) AwaitCommands(t *testing.T, id string) {
	t.Helper()
	if !testpoll.UntilNoT(waitBound, func() bool { _, running := d.commandList(t, id); return !running }, 15*time.Millisecond) {
		t.Fatalf("a typed command of %s still runs\nstderr:\n%s", id, d.Stderr())
	}
}

func (d *httpDriver) CommandRecords(t *testing.T, id string) callResult {
	t.Helper()
	recs, _ := d.commandList(t, id)
	return callResult{Status: http.StatusOK, Body: recs}
}

func (d *httpDriver) Models(t *testing.T) callResult {
	t.Helper()
	return notInEngine("GET /models")
}

func (d *httpDriver) EnqueueNext(t *testing.T, id, text string) {
	t.Helper()
	d.Enqueue(t, id, text)
}

func (d *httpDriver) Stderr() string { return d.p.stderr.String() }

func (d *httpDriver) Workdir() string { return d.workDir }

func (d *httpDriver) scan(t *testing.T, visit func(raw []byte) bool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	return d.p.scanEvents(ctx, visit)
}

func (d *httpDriver) WaitIdle(t *testing.T, id string) {
	t.Helper()
	timeoutS := max(1, min(int(waitBound/time.Second), 300))
	data := d.expect(t, http.StatusOK, http.MethodGet, fmt.Sprintf("/session/%s/wait?until=idle&timeout_s=%d", id, timeoutS), nil)
	var w struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("decode wait: %v (%s)", err, data)
	}
	if w.State != "idle" {
		t.Fatalf("session %s wait returned state %q, want idle\nstderr:\n%s", id, w.State, d.p.stderr.String())
	}
}

func (d *httpDriver) Interrupt(t *testing.T, id string) {
	t.Helper()
	d.expect(t, http.StatusNoContent, http.MethodPost, "/session/"+id+"/abort", nil)
}

func (d *httpDriver) SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool) {
	t.Helper()
	body := map[string]any{"condition": condition, "max_turns": maxTurns, "defer": deferred}
	d.expect(t, http.StatusAccepted, http.MethodPost, "/session/"+id+"/goal", body)
}

func (d *httpDriver) Messages(t *testing.T, id string) []transcriptMessage {
	t.Helper()
	return transcriptOf(d.p.messages(id))
}

// apiContent is one item of a tool result: text, or a blob with its bytes.
type apiContent struct {
	Type, Text, Data string
	MediaType        string `json:"media_type"`
}

func transcriptOf(msgs []apiMessage) []transcriptMessage {
	out := make([]transcriptMessage, len(msgs))
	for i, m := range msgs {
		out[i] = transcriptMessage{ID: m.ID, Role: m.Role}
		for _, p := range m.Parts {
			tp := transcriptPart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, IsError: p.IsError}
			if len(p.Arguments) > 0 {
				_ = json.Unmarshal(p.Arguments, &tp.Arguments)
			}
			var content []string
			for _, c := range p.Content {
				if c.Type == "blob" {
					content = append(content, "[blob "+c.MediaType+" "+c.Data+"]")
					continue
				}
				content = append(content, c.Text)
			}
			tp.Content = strings.Join(content, "\n")
			out[i].Parts = append(out[i].Parts, tp)
		}
	}
	return out
}

func journalOf(events []apiEvent) []journalEntry {
	out := make([]journalEntry, len(events))
	for i, ev := range events {
		out[i] = journalEntry{Seq: ev.Seq}
		if ev.Type == "message" && ev.Message != nil {
			out[i].IsMessage, out[i].MessageID = true, ev.Message.ID
		}
	}
	return out
}

// Journals reads the journal from the start up to the tip observed first, so
// the read ends on an event count, not a deadline.
func (d *httpDriver) Journals(t *testing.T) [][]journalEntry {
	t.Helper()
	var tip struct {
		Seq int64 `json:"seq"`
	}
	data := d.expect(t, http.StatusOK, http.MethodGet, "/event/tip", nil)
	if err := json.Unmarshal(data, &tip); err != nil {
		t.Fatalf("decode tip: %v (%s)", err, data)
	}
	if tip.Seq == 0 {
		return nil
	}
	var events []apiEvent
	err := d.scan(t, func(raw []byte) bool {
		var ev apiEvent
		if json.Unmarshal(raw, &ev) != nil || ev.Seq == 0 {
			return false
		}
		events = append(events, ev)
		return ev.Seq >= tip.Seq
	})
	if err != nil {
		t.Fatalf("event stream ended at %d events before tip %d: %v\nstderr:\n%s", len(events), tip.Seq, err, d.Stderr())
	}
	return [][]journalEntry{journalOf(events)}
}

func (d *httpDriver) Queued(t *testing.T, id string) []string {
	t.Helper()
	var texts []string
	for _, item := range d.p.queueGet(id).Queued {
		texts = append(texts, item.Text)
	}
	return texts
}

func (d *httpDriver) Restart(t *testing.T, kill bool) {
	t.Helper()
	if kill {
		d.p.kill()
	} else {
		d.p.terminate(t)
	}
	d.p = d.serve(t)
}

func (d *httpDriver) call(t *testing.T, method, path string, body any) callResult {
	t.Helper()
	resp, data := d.p.do(method, path, body)
	res := callResult{Status: resp.StatusCode}
	v := decodeBody(t, method+" "+path, data)
	if v == nil {
		return res
	}
	if obj, ok := v.(map[string]any); ok {
		if list, ok := obj["messages"].([]any); ok {
			raw, _ := json.Marshal(list)
			var msgs []apiMessage
			if err := json.Unmarshal(raw, &msgs); err != nil {
				t.Fatalf("%s %s: decode messages: %v (%s)", method, path, err, raw)
			}
			res.Messages = transcriptOf(msgs)
			delete(obj, "messages")
		}
	}
	res.Body = v
	return res
}

func withQuery(path string, kv ...any) string {
	q := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		if n := kv[i+1].(int); n != 0 {
			q.Set(kv[i].(string), strconv.Itoa(n))
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func (d *httpDriver) Compact(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/compact", map[string]any{})
}

func (d *httpDriver) SetModel(t *testing.T, id, model string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/model", map[string]any{"model": model})
}

func (d *httpDriver) SetThinking(t *testing.T, id, level string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/thinking", map[string]any{"effort": level})
}

func (d *httpDriver) SetServiceTier(t *testing.T, id, tier string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/service-tier", map[string]any{"service_tier": tier})
}

func (d *httpDriver) EndSession(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/session/"+id, nil)
}

func (d *httpDriver) Send(t *testing.T, id, text string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/send", map[string]any{"text": text})
}

func (d *httpDriver) CancelTree(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/session/"+id+"/cancel_tree", nil)
}

func (d *httpDriver) DeleteQueued(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/session/"+id+"/queue", nil)
}

func (d *httpDriver) UpdateGoal(t *testing.T, id, condition string) callResult {
	t.Helper()
	return withoutSeq(d.call(t, http.MethodPost, "/session/"+id+"/goal", map[string]any{"condition": condition}))
}

func (d *httpDriver) ClearGoal(t *testing.T, id string) callResult {
	t.Helper()
	return d.call(t, http.MethodDelete, "/session/"+id+"/goal", nil)
}

func (d *httpDriver) ListSessions(t *testing.T) callResult {
	t.Helper()
	return withoutSeq(d.call(t, http.MethodGet, "/session", nil))
}

// withoutSeq drops the instance-wide event cursor from session objects. It
// moves with events of other sessions, so it is not stable across runs.
func withoutSeq(res callResult) callResult {
	objs, _ := res.Body.([]any)
	if obj, ok := res.Body.(map[string]any); ok {
		objs = []any{obj}
	}
	for _, o := range objs {
		if obj, ok := o.(map[string]any); ok {
			delete(obj, "seq")
		}
	}
	return res
}

func (d *httpDriver) GetSession(t *testing.T, id string) callResult {
	t.Helper()
	return withoutSeq(d.call(t, http.MethodGet, "/session/"+id, nil))
}

func (d *httpDriver) SessionStatus(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/session/status", nil)
}

func (d *httpDriver) MessagesPage(t *testing.T, id string, beforeSeq, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/session/"+id+"/message", "before_seq", beforeSeq, "limit", limit), nil)
}

func (d *httpDriver) Bootstrap(t *testing.T, id string, limit int) callResult {
	t.Helper()
	path := "/session/" + id + "/message?stream_from=1"
	if limit != 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}
	return d.call(t, http.MethodGet, path, nil)
}

func (d *httpDriver) JournalPage(t *testing.T, id string, from, limit int) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, withQuery("/session/"+id+"/journal", "from", from, "limit", limit), nil)
}

func (d *httpDriver) Command(t *testing.T, id, text string, repeatable bool) callResult {
	t.Helper()
	if repeatable {
		d.enqSeq[id]++
		d.typedSeq[id] = d.enqSeq[id]
		return d.enqueueCall(t, id, text, d.enqSeq[id], true)
	}
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}, "source": "typed"}
	return withoutSeq(d.call(t, http.MethodPost, "/session/"+id+"/prompt_async", body))
}

func (d *httpDriver) Commands(t *testing.T) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/commands", nil)
}

type sseFrame struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Status    string `json:"status,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	seq       int64
}

// frames reads /event after seq `after` until a frame reaches stop. The read
// ends on that frame, not on a deadline, so stop must be past after.
func (d *httpDriver) frames(t *testing.T, after int64, header bool, session string, stop int64) []sseFrame {
	t.Helper()
	var out []sseFrame
	if stop <= after {
		t.Fatalf("event stream after seq %d never reaches stop seq %d", after, stop)
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	err := d.p.scanEventsFrom(ctx, after, header, session, func(id string, raw []byte) bool {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Status    string `json:"status"`
			Seq       int64  `json:"seq"`
			Message   *struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Seq == 0 {
			return false
		}
		f := sseFrame{ID: id, Type: ev.Type, SessionID: ev.SessionID, Status: ev.Status, seq: ev.Seq}
		if ev.Message != nil {
			f.MessageID = ev.Message.ID
		}
		out = append(out, f)
		return ev.Seq >= stop
	})
	if err != nil {
		t.Fatalf("event stream ended after %d frames before seq %d: %v\nstderr:\n%s", len(out), stop, err, d.Stderr())
	}
	return out
}

func (d *httpDriver) SSEResume(t *testing.T, id string, afterSeq int64, header, scoped bool) callResult {
	t.Helper()
	var tip struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(d.expect(t, http.StatusOK, http.MethodGet, "/event/tip", nil), &tip); err != nil {
		t.Fatalf("decode tip: %v", err)
	}
	all := d.frames(t, afterSeq, header, "", tip.Seq)
	got := all
	if id != "" {
		got = nil
		for _, f := range all {
			if f.SessionID == id {
				got = append(got, f)
			}
		}
	}
	if scoped {
		if len(got) == 0 {
			got = nil
		} else {
			got = d.frames(t, afterSeq, header, id, got[len(got)-1].seq)
		}
	}
	body := make([]any, len(got))
	for i, f := range got {
		raw, _ := json.Marshal(f)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		body[i] = m
	}
	return callResult{Status: http.StatusOK, Body: map[string]any{"frames": body}}
}

func (d *httpDriver) Child(t *testing.T, parentID string, nth int) string {
	t.Helper()
	var ids []string
	ok := testpoll.UntilNoT(waitBound, func() bool {
		ids = ids[:0]
		var list []struct {
			ID      string `json:"id"`
			Lineage *struct {
				ParentID string `json:"parent_id"`
			} `json:"lineage"`
		}
		if json.Unmarshal(d.expect(t, http.StatusOK, http.MethodGet, "/session", nil), &list) != nil {
			return false
		}
		for _, s := range list {
			if s.Lineage != nil && s.Lineage.ParentID == parentID {
				ids = append(ids, s.ID)
			}
		}
		return len(ids) > nth
	}, 15*time.Millisecond)
	if !ok {
		t.Fatalf("session %s has %d children, want more than %d\nstderr:\n%s", parentID, len(ids), nth, d.Stderr())
	}
	return ids[nth]
}

func (d *httpDriver) AwaitGoalExhausted(t *testing.T) {
	t.Helper()
	exhausted := false
	err := d.scan(t, func(raw []byte) bool {
		var ev struct {
			Type, Outcome string
			GoalReason    string `json:"goal_reason"`
		}
		if json.Unmarshal(raw, &ev) != nil {
			return false
		}
		switch ev.Type {
		case "goal.cleared":
			exhausted = exhausted || ev.GoalReason == "goal exhausted max_turns (2)"
		case "turn.end":
			return exhausted && ev.Outcome == "max_turns_exceeded"
		}
		return false
	})
	if err != nil {
		t.Fatalf("no goal.cleared with reason %q before turn.end max_turns_exceeded: %v\nstderr:\n%s", "goal exhausted max_turns (2)", err, d.Stderr())
	}
}

func TestWaitsFailAtTheirBound(t *testing.T) {
	skipShort(t)
	old := waitBound
	oldMargin := waitMargin
	waitBound, waitMargin = 100*time.Millisecond, 0
	t.Cleanup(func() { waitBound, waitMargin = old, oldMargin })

	t.Run("request", func(t *testing.T) {
		stuck := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		t.Cleanup(stuck.Close)
		p := &serveProc{t: t, addr: strings.TrimPrefix(stuck.URL, "http://")}
		if _, _, err := p.send(http.MethodGet, "/health", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send to a server that never answers = %v, want context deadline exceeded", err)
		}
	})

	t.Run("event stream", func(t *testing.T) {
		fake := harnesstest.New(t)
		d := newHTTPDriver(t, fake.URL())
		err := d.scan(t, func([]byte) bool { return false })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scan of an idle stream = %v, want context deadline exceeded", err)
		}
	})
}
