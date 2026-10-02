package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/fakemodel"
)

type driver interface {
	Create(t *testing.T) string
	Submit(t *testing.T, id, text string)
	Enqueue(t *testing.T, id, text string)
	WaitIdle(t *testing.T, id string)
	Interrupt(t *testing.T, id string)
	SetGoal(t *testing.T, id, condition string, maxTurns int, deferred bool)
	Messages(t *testing.T, id string) []apiMessage
	Events(t *testing.T) []apiEvent
	Restart(t *testing.T, kill bool)
	Queued(t *testing.T, id string) []string
	AwaitTurnEnd(t *testing.T, outcome string)
	Stderr() string
}

// waitBound is a failure bound for a wait on the serve process, not a delay.
// It is far above any real latency; a wait that reaches it fails the test.
var waitBound = 60 * time.Second

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
	data := d.expect(t, http.StatusOK, http.MethodGet, "/session/"+id+"/wait?until=idle&timeout_s=300", nil)
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

func (d *httpDriver) Messages(t *testing.T, id string) []apiMessage {
	t.Helper()
	return d.p.messages(id)
}

// Events reads the journal from the start up to the tip observed first, so the
// read ends on an event count, not a deadline.
func (d *httpDriver) Events(t *testing.T) []apiEvent {
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
	return events
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

func (d *httpDriver) AwaitTurnEnd(t *testing.T, outcome string) {
	t.Helper()
	err := d.scan(t, func(raw []byte) bool {
		var ev struct{ Type, Outcome string }
		return json.Unmarshal(raw, &ev) == nil && ev.Type == "turn.end" && ev.Outcome == outcome
	})
	if err != nil {
		t.Fatalf("no turn ended with %q: %v\nstderr:\n%s", outcome, err, d.Stderr())
	}
}

func TestWaitsFailAtTheirBound(t *testing.T) {
	skipShort(t)
	old := waitBound
	waitBound = 100 * time.Millisecond
	t.Cleanup(func() { waitBound = old })

	t.Run("model requests", func(t *testing.T) {
		r := &run{reqs: make(chan int, 4)}
		r.reqs <- 1
		if r.waitForRequests(2, waitBound) {
			t.Fatal("waitForRequests(2) = true with one request seen, want false at the bound")
		}
		r.reqs <- 2
		if !r.waitForRequests(2, waitBound) {
			t.Fatal("waitForRequests(2) = false with two requests seen")
		}
	})
	t.Run("event stream", func(t *testing.T) {
		fake := fakemodel.New(t)
		d := newHTTPDriver(t, fake.URL())
		err := d.scan(t, func([]byte) bool { return false })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scan of an idle stream = %v, want context deadline exceeded", err)
		}
	})
}
