package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
)

// errorAnthropic streams a single terminal provider error on its first (and
// every) request, so the harness turn ends outcome=error -- the exact shape
// the boxes console-bootstrap pass-through (ConsoleLastTurnView) forwards for
// a stalled/failed turn.
type errorAnthropic struct{ msg string }

func (f *errorAnthropic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flusher", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, sse("error", fmt.Sprintf(
		`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, f.msg)))
	flusher.Flush()
}

// TestLastTurnErrorSurfacesInSessionGet proves that a REAL harness binary,
// driven to a turn that ends outcome=error, reports it on GET /session/{id}'s
// last_turn field with the provider error text -- the durable signal the
// boxes console reads (issue: a failed turn used to be a silent stall). This
// is the real-binary counterpart to internal/api's fake-harness
// TestConsoleBootstrap_CarriesLastTurnError.
func TestLastTurnErrorSurfacesInSessionGet(t *testing.T) {
	skipShort(t)

	const wantMsg = "e2e: forced terminal provider error"
	srv := httptest.NewServer(&errorAnthropic{msg: wantMsg})
	t.Cleanup(srv.Close)

	cfgPath := writeConfig(t, srv.URL)
	p := startServe(t, t.TempDir(), cfgPath)

	id := p.createSession()
	p.prompt(id, "a prompt whose turn will fail upstream")

	var lt *lastTurnView
	if !testpoll.UntilNoT(10*time.Second, func() bool {
		lt = p.lastTurn(id)
		return lt != nil
	}, 20*time.Millisecond) {
		t.Fatalf("session %s never reported last_turn after a failed turn\nstderr:\n%s", id, p.stderr.String())
	}

	if lt.Outcome != "error" {
		t.Errorf("last_turn.outcome = %q, want %q", lt.Outcome, "error")
	}
	// The point of the passthrough is that the provider's OWN message text
	// reaches the console, not merely that some non-empty string does. The
	// harness wraps it ("[permanent] anthropic: <msg> (invalid_request_error)"),
	// so assert the upstream message survives verbatim inside that wrap.
	if !strings.Contains(lt.Error, wantMsg) {
		t.Errorf("last_turn.error = %q, want it to contain the provider text %q", lt.Error, wantMsg)
	}
}

type lastTurnView struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
}

// lastTurn reads GET /session/{id} and returns its last_turn, or nil until a
// turn has finished in this process.
func (p *serveProc) lastTurn(id string) *lastTurnView {
	p.t.Helper()
	resp, data := p.do(http.MethodGet, "/session/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		p.t.Fatalf("get session: status %d body %s", resp.StatusCode, data)
	}
	var s struct {
		LastTurn *lastTurnView `json:"last_turn"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		p.t.Fatalf("decode session: %v (%s)", err, data)
	}
	return s.LastTurn
}
