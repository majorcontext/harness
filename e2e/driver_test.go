package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"syscall"
	"testing"
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
}

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
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
		"seq":   d.enqSeq[id],
	}
	d.expect(t, http.StatusAccepted, http.MethodPost, "/session/"+id+"/enqueue", body)
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.p.addr+"/event?from=0", nil)
	if err != nil {
		t.Fatalf("event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /event: %v", err)
	}
	defer resp.Body.Close()
	var events []apiEvent
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			t.Fatalf("event stream ended at %d events before tip %d: %v", len(events), tip.Seq, err)
		}
		var ev apiEvent
		if json.Unmarshal(raw, &ev) != nil || ev.Seq == 0 {
			continue
		}
		events = append(events, ev)
		if ev.Seq >= tip.Seq {
			return events
		}
	}
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

func (p *serveProc) terminate(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited {
		return
	}
	p.waited = true
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Logf("serve exit after SIGTERM: %v", err)
	}
}
