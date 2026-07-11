package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/process"
)

func newProcessHarness(t *testing.T, defs map[string]process.Def) (*harness, *process.Manager) {
	t.Helper()
	dir := t.TempDir()
	mgr := process.NewManager(dir, defs)
	const token = "secret-run-token"
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.Processes = mgr
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}, mgr
}

func TestHandleProcessList_NotConfigured(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, body := h.do(http.MethodGet, "/process", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var infos []process.Info
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(infos) != 0 {
		t.Errorf("infos = %+v, want empty", infos)
	}
}

func TestHandleProcessList(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	resp, body := h.do(http.MethodGet, "/process", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var infos []process.Info
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(infos) != 1 || infos[0].Name != "dev" {
		t.Fatalf("infos = %+v, want [dev]", infos)
	}
}

func TestHandleProcessListIncludesPorts(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}, Ports: []int{3000, 3001}},
	})
	resp, body := h.do(http.MethodGet, "/process", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var infos []process.Info
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(infos) != 1 || len(infos[0].Ports) != 2 || infos[0].Ports[0] != 3000 || infos[0].Ports[1] != 3001 {
		t.Fatalf("infos = %+v, want dev with Ports [3000 3001]", infos)
	}
}

func TestHandleProcessStartStopRestart(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {
			Command:      []string{"sh", "-c", `echo "Ready in 5ms"; sleep 100`},
			ReadyRegex:   "Ready in .*ms",
			ReadyTimeout: 5 * time.Second,
		},
	})

	resp, body := h.do(http.MethodPost, "/process/dev/start", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", resp.StatusCode, body)
	}
	var st process.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.State != process.StateReady {
		t.Fatalf("start status = %+v, want ready", st)
	}

	resp, body = h.do(http.MethodPost, "/process/dev/restart", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d, body = %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.State != process.StateReady {
		t.Fatalf("restart status = %+v, want ready", st)
	}

	resp, body = h.do(http.MethodPost, "/process/dev/stop", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, body = %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if st.State != process.StateStopped {
		t.Fatalf("stop status = %+v, want stopped", st)
	}
}

func TestHandleProcessUnknownName404(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	for _, action := range []string{"start", "stop", "restart"} {
		resp, body := h.do(http.MethodPost, "/process/nope/"+action, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, body = %s, want 404", action, resp.StatusCode, body)
		}
	}
}

// TestHandleProcessListNeverLeaksEnvValues is the HTTP-layer counterpart
// of process.TestDeclareAndUndeclare's "env names never expose values"
// case: GET /process is the one endpoint an orchestrator (or a curious
// model, via the same data the `process` tool's `list` action returns)
// can use to read back a process's configuration, and a secret-bearing
// env value (an API key, a DB password) must never round-trip through it
// — only the "K" half of each "K=V" entry.
func TestHandleProcessListNeverLeaksEnvValues(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}, Env: []string{"API_KEY=super-secret-value"}},
	})
	resp, body := h.do(http.MethodGet, "/process", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "super-secret-value") {
		t.Fatalf("GET /process response leaked an env VALUE: %s", body)
	}
	var infos []process.Info
	if err := json.Unmarshal(body, &infos); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(infos) != 1 || len(infos[0].EnvNames) != 1 || infos[0].EnvNames[0] != "API_KEY" {
		t.Fatalf("infos = %+v, want EnvNames = [API_KEY] and nothing else", infos)
	}
}

func TestHandleProcessRequiresAuth(t *testing.T) {
	h, _ := newProcessHarness(t, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/process", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
