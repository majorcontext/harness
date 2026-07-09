package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// TestGarbageSessionIDsAreNotFound is the RED test for parse-at-boundary: any
// {id} handler must validate with engine.ValidSessionID and return 404, never
// 500, for input that is neither a legacy hex ID nor a TypeID.
func TestGarbageSessionIDsAreNotFound(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)

	garbage := []string{
		"nope",
		"ses_not-hex-and-not-typeid",
		"",
		"ses_",
	}
	for _, id := range garbage {
		for _, ep := range []struct{ method, path string }{
			{"GET", "/session/" + id},
			{"GET", "/session/" + id + "/message"},
			{"POST", "/session/" + id + "/prompt_async"},
			{"POST", "/session/" + id + "/abort"},
			{"DELETE", "/session/" + id + "/goal"},
		} {
			var body any
			if ep.method == "POST" && strings.Contains(ep.path, "prompt_async") {
				body = map[string]any{"parts": []map[string]string{{"type": "text", "text": "hi"}}}
			}
			resp, data := h.do(ep.method, ep.path, body)
			if resp.StatusCode != 404 {
				t.Errorf("%s %s (id=%q) status = %d, want 404: %s", ep.method, ep.path, id, resp.StatusCode, data)
			}
		}
	}
}

// TestPercentEncodedSlashIDIsNotFound is the RED test proving the actual
// exploit ValidSessionID closes: net/http's ServeMux routes on the RAW
// (still-percent-encoded) path when splitting segments, but r.PathValue
// decodes the matched segment — so a single path segment spelled
// "..%2fleaked" matches the "/session/{id}" pattern (no ".." literal for the
// mux's own dot-segment redirect to catch) yet decodes to the traversal
// string "../leaked". Without validation this reads a "session" log outside
// SessionDir; ValidSessionID rejects it (it is neither legacy hex nor a
// TypeID) and the handler must 404, not leak the file's contents.
func TestPercentEncodedSlashIDIsNotFound(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.Mkdir(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newHarnessDir(t, sessionDir, prov)

	// A "session" log that lives OUTSIDE sessionDir, one level up.
	const secretMarker = "top-secret-other-tenant-data"
	leaked := `{"type":"session","id":"leaked","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_0000000000000001","role":"user","parts":[{"type":"text","text":"` + secretMarker + `"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, "leaked.jsonl"), []byte(leaked), 0o644); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("GET", h.ts.URL+"/session/..%2fleaked", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])

	if resp.StatusCode != 404 {
		t.Fatalf("GET /session/..%%2fleaked status = %d, want 404 (body: %s)", resp.StatusCode, body)
	}
	if strings.Contains(body, secretMarker) {
		t.Fatalf("path traversal leaked file contents: %s", body)
	}
}

// TestLegacySessionIDOverHTTP is the RED test for legacy read-compat over the
// HTTP surface: a session log fixture written with a pre-TypeID "ses_" + 16
// hex ID must be gettable, listable, and promptable through the server, not
// just through the engine package directly.
func TestLegacySessionIDOverHTTP(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	dir := t.TempDir()
	h := newHarnessDir(t, dir, prov)

	const legacyID = "ses_0123456789abcdef"
	fixture := `{"type":"session","id":"ses_0123456789abcdef","created_at":"2025-01-02T03:04:05Z"}
{"type":"model","model":"test/m1"}
{"type":"message","message":{"id":"msg_0000000000000001","role":"user","parts":[{"type":"text","text":"hello from the past"}]}}
`
	if err := os.WriteFile(filepath.Join(dir, legacyID+".jsonl"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, data := h.do("GET", "/session/"+legacyID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET legacy session status = %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /session status = %d: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), legacyID) {
		t.Fatalf("GET /session listing missing legacy id %q: %s", legacyID, data)
	}

	resp, data = h.do("POST", "/session/"+legacyID+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "hi again"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("prompt_async on legacy session status = %d: %s", resp.StatusCode, data)
	}
}
