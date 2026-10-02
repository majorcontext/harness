package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

type driver interface {
	Create(t *testing.T) string
	Submit(t *testing.T, id, text string)
	Enqueue(t *testing.T, id, text string)
	WaitIdle(t *testing.T, id string)
	Interrupt(t *testing.T, id string)
	SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool)
	Messages(t *testing.T, id string) []transcriptMessage
	Events(t *testing.T) []journalEntry
	Restart(t *testing.T, kill bool)
	Queued(t *testing.T, id string) []string
	AwaitMaxTurnsExceeded(t *testing.T)
	Stderr() string
}

// waitBound is a failure bound for a wait on the serve process, not a delay.
// It is far above any real latency; a wait that reaches it fails the test.
var waitBound = 60 * time.Second

// waitMargin is the time a request may run past waitBound for the server to
// honor a timeout_s of waitBound and answer.
var waitMargin = 10 * time.Second

type httpDriver struct {
	sessDir, workDir, config string
	p                        *serveProc
	enqSeq                   map[string]int64
}

func newHTTPDriver(t *testing.T, modelURL string) *httpDriver {
	t.Helper()
	d := &httpDriver{
		sessDir: t.TempDir(),
		workDir: t.TempDir(),
		config:  writeGoalConfig(t, modelURL),
		enqSeq:  map[string]int64{},
	}
	d.p = startServeIn(t, d.sessDir, d.config, d.workDir)
	return d
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

func (d *httpDriver) Enqueue(t *testing.T, id, text string) {
	t.Helper()
	d.enqSeq[id]++
	if status, _ := d.p.enqueue(id, text, d.enqSeq[id]); status != http.StatusAccepted {
		t.Fatalf("enqueue on %s: status %d, want %d\nstderr:\n%s", id, status, http.StatusAccepted, d.Stderr())
	}
}

func (d *httpDriver) Stderr() string { return d.p.stderr.String() }

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

// Events reads the journal from the start up to the tip observed first, so the
// read ends on an event count, not a deadline.
func (d *httpDriver) Events(t *testing.T) []journalEntry {
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
	return journalOf(events)
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
	d.p = startServeIn(t, d.sessDir, d.config, d.workDir)
}

func (d *httpDriver) AwaitMaxTurnsExceeded(t *testing.T) {
	t.Helper()
	err := d.scan(t, func(raw []byte) bool {
		var ev struct{ Type, Outcome string }
		return json.Unmarshal(raw, &ev) == nil && ev.Type == "turn.end" && ev.Outcome == "max_turns_exceeded"
	})
	if err != nil {
		t.Fatalf("no turn ended with max_turns_exceeded: %v\nstderr:\n%s", err, d.Stderr())
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
