package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// newWorkdirHarness builds a harness with an explicit WorkspaceRoots, so
// explicit-workdir validation is deterministic.
func newWorkdirHarness(t *testing.T, prov provider.Provider, roots []string) *harness {
	t.Helper()
	const token = "secret-run-token"
	dir := t.TempDir()
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.WorkspaceRoots = roots
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: token, srv: srv, ts: ts}
}

func sessionWorkDir(t *testing.T, data []byte) string {
	t.Helper()
	var s struct {
		WorkDir string `json:"workdir"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s.WorkDir
}

// TestCreateSessionDefaultWorkdirIsCwd verifies that an omitted workdir
// defaults to the serve process's current working directory, and that the
// value is reported back in the Session JSON.
func TestCreateSessionDefaultWorkdirIsCwd(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionWorkDir(t, data); got != cwd {
		t.Errorf("workdir = %q, want process cwd %q", got, cwd)
	}
}

// TestCreateSessionExplicitWorkdirUnderRootAccepted verifies that an explicit
// workdir that clean-resolves under a configured workspace root is accepted
// and echoed back.
func TestCreateSessionExplicitWorkdirUnderRootAccepted(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{root})
	resp, data := h.do("POST", "/session", map[string]any{"workdir": sub})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	if got := sessionWorkDir(t, data); got != sub {
		t.Errorf("workdir = %q, want %q", got, sub)
	}
}

// TestCreateSessionExplicitWorkdirOutsideRootsRejected verifies the 400 path:
// a workdir that does not clean-resolve under any configured workspace root
// is rejected, with an error body.
func TestCreateSessionExplicitWorkdirOutsideRootsRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not nested under root
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{root})
	resp, data := h.do("POST", "/session", map[string]any{"workdir": outside})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status %d, want 400: %s", resp.StatusCode, data)
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &e); e.Error == "" {
		t.Errorf("400 body missing error: %s", data)
	}
}

// TestCreateSessionExplicitWorkdirTraversalRejected verifies that a workdir
// expressed with ".." segments cannot escape the workspace root: it must
// clean-resolve, not merely string-match.
func TestCreateSessionExplicitWorkdirTraversalRejected(t *testing.T) {
	root := t.TempDir()
	escaped := filepath.Join(root, "..", "..", "etc")
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{root})
	resp, data := h.do("POST", "/session", map[string]any{"workdir": escaped})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status %d, want 400: %s", resp.StatusCode, data)
	}
}

// TestCreateSessionRelativeWorkspaceRootAccepted verifies that a configured
// workspace root given as a relative path (e.g. `-workspace-root ./work`) is
// absolutized before the containment check, so a workdir nested under it is
// accepted rather than rejected by every request (review finding: only the
// candidate path was made absolute, never the configured roots).
func TestCreateSessionRelativeWorkspaceRootAccepted(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	sub := filepath.Join(base, "work", "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h := newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{"./work"})
	resp, data := h.do("POST", "/session", map[string]any{"workdir": sub})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d, want 201: %s", resp.StatusCode, data)
	}
	if got := sessionWorkDir(t, data); got != sub {
		t.Errorf("workdir = %q, want %q", got, sub)
	}
}

// TestPromptSameWorkdirBusyRejected verifies the core claim rule: with
// session A holding its workdir busy (a channel-blocked scripted provider
// mid-stream), a prompt to session B — which defaults to the very same
// workdir (the process cwd) — is rejected with 409 naming the holder.
func TestPromptSameWorkdirBusyRejected(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	t.Cleanup(prov.releaseAll)

	idA := h.createSession("test/m1")
	idB := h.createSession("test/m1")

	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started // A is now blocked mid-stream, holding its workdir

	resp, data = h.do("POST", "/session/"+idB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("prompt B (same workdir) status %d, want 409: %s", resp.StatusCode, data)
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &e)
	if !strings.Contains(e.Error, idA) {
		t.Errorf("409 error = %q, want it to name holder session %s", e.Error, idA)
	}
}

// TestPromptShareWorkdirBypassesConflict verifies that opting into
// share_workdir on either side bypasses the workdir-busy rejection — here B
// opts in even though the holder A did not.
func TestPromptShareWorkdirBypassesConflict(t *testing.T) {
	prov := newBlockingProvider("test")
	h := newHarness(t, prov)
	t.Cleanup(prov.releaseAll)

	idA := h.createSession("test/m1") // default: no share_workdir
	idB := h.createSessionBody(map[string]any{"model": "test/m1", "share_workdir": true})

	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+idB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt B (share_workdir) status %d, want 202: %s", resp.StatusCode, data)
	}
}

// TestPromptDifferentWorkdirsRunConcurrently verifies that two sessions with
// distinct workdirs never contend: B's prompt is accepted while A's is still
// blocked mid-stream.
func TestPromptDifferentWorkdirsRunConcurrently(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prov := newBlockingProvider("test")
	h := newWorkdirHarness(t, prov, []string{root})
	t.Cleanup(prov.releaseAll)

	idA := h.createSessionBody(map[string]any{"model": "test/m1", "workdir": dirA})
	idB := h.createSessionBody(map[string]any{"model": "test/m1", "workdir": dirB})

	resp, data := h.do("POST", "/session/"+idA+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "first"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt A status %d: %s", resp.StatusCode, data)
	}
	<-prov.started

	resp, data = h.do("POST", "/session/"+idB+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "second"}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt B (different workdir) status %d, want 202: %s", resp.StatusCode, data)
	}
}
