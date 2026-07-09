package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestMessageEndpointTruncatedGoalStalledTail is the forensic regression
// guard for GET /session/{id}/message 500ing on the incident session
// (ses_01kx3pvqttfwgbf2n5x1f1y8yh.jsonl): a cold (non-resident) session whose
// on-disk log ends in a truncated goal.stalled record — the shape a crash
// mid-append leaves, per scanLog's documented discipline (engine/store.go) —
// must still be gettable and its messages still servable, unaffected by the
// truncated tail. The endpoint reads through engine.LoadSession (see
// server/handlers.go's lookup), which tolerates exactly this shape.
func TestMessageEndpointTruncatedGoalStalledTail(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	dir := t.TempDir()
	h := newHarnessDir(t, dir, prov)

	const id = "ses_9999999999999999"
	fixture := `{"type":"session","id":"ses_9999999999999999","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"goal.set","goal":{"condition":"ship it"}}
{"type":"message","message":{"id":"msg_0000000000000001","role":"user","parts":[{"type":"text","text":"hi"}]}}
{"type":"goal.stalled","goal":{"reason":"json: error calling MarshalJSON f`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status = %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message status = %d, want 200 (truncated goal.stalled tail must not poison /message): %s", resp.StatusCode, data)
	}
	var msgs []message.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("response body is not valid JSON: %v: %s", err, data)
	}
	if len(msgs) != 1 || msgs[0].ID != "msg_0000000000000001" {
		t.Fatalf("messages = %+v, want exactly the one complete user message", msgs)
	}
}

// TestMessageEndpointCorruptMiddleGoalStalledLine proves the other half:
// a corrupt goal.stalled record anywhere but the final line is a real
// structural corruption (never an in-flight crash), so it is a loud error —
// GET /session/{id} for such a session must 404 (LoadSession fails), never
// serve a poisoned or partial history.
func TestMessageEndpointCorruptMiddleGoalStalledLine(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	dir := t.TempDir()
	h := newHarnessDir(t, dir, prov)

	const id = "ses_8888888888888880"
	fixture := `{"type":"session","id":"ses_8888888888888880","created_at":"2025-01-02T03:04:05Z"}
{"type":"goal.set","goal":{"condition":"ship it"}}
{"type":"goal.stalled","goal":{"reason":"broke
{"type":"goal.cleared","goal":{"reason":"worker turn failed"}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 404 {
		t.Fatalf("GET session status = %d, want 404 for a corrupt-middle-line log: %s", resp.StatusCode, data)
	}
}

// failMarshal implements json.Marshaler by always failing, so writeJSON's
// encode-failure path can be exercised without needing a real poisoned
// json.RawMessage (message.ToolCall no longer produces one — see
// message.TestToolCallEmptyArgumentsMarshal).
type failMarshal struct{}

func (failMarshal) MarshalJSON() ([]byte, error) {
	return nil, errors.New("simulated marshal failure")
}

// TestWriteJSONMarshalFailureIsLoud5xxNotSilentPartial200 is the regression
// guard for writeJSON's header-then-stream ordering bug: it used to call
// w.WriteHeader(code) — unconditionally the *success* code — before
// streaming the encoder, so a marshal failure discovered mid-encode left a
// truncated body behind an already-committed 200 status: exactly what a
// client of GET /session/{id}/message would have seen for a poisoned
// history (empty or truncated body, misleadingly labeled success). writeJSON
// must marshal to a buffer first and only then write a single, consistent
// status-plus-body: on failure that means a real 500 with a real JSON error
// body, never a bare empty response.
func TestWriteJSONMarshalFailureIsLoud5xxNotSilentPartial200(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, failMarshal{})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a marshal failure must never be reported as the requested success code)", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body == "" {
		t.Fatal("body is empty — the resilience path itself failed silently")
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("error body is not valid JSON: %v: %s", err, body)
	}
	if errBody.Error == "" {
		t.Errorf("error body missing a message: %s", body)
	}
}
