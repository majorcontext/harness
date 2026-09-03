package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// fakeClaudeBinForServer is the compiled engine/testdata/fakeclaude stand-in
// (see engine/claude_code_backend_test.go's buildFakeClaude for the
// original), rebuilt here because it is a package-private helper of the
// engine test binary and this package needs its own copy to drive a real
// `claude`-shaped child process through the SERVER's own residency/
// SessionManager bracket, not just engine.Session in isolation.
var (
	fakeClaudeBinForServer     string
	fakeClaudeBinForServerOnce sync.Once
	fakeClaudeBinForServerErr  error
)

func buildFakeClaudeForServer(t *testing.T) string {
	t.Helper()
	fakeClaudeBinForServerOnce.Do(func() {
		dir, err := os.MkdirTemp("", "harness-fakeclaude-server")
		if err != nil {
			fakeClaudeBinForServerErr = err
			return
		}
		bin := filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", bin, "../engine/testdata/fakeclaude")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeBinForServerErr = fmt.Errorf("go build fakeclaude: %v\n%s", err, out)
			return
		}
		fakeClaudeBinForServer = bin
	})
	if fakeClaudeBinForServerErr != nil {
		t.Fatalf("buildFakeClaudeForServer: %v", fakeClaudeBinForServerErr)
	}
	return fakeClaudeBinForServer
}

// claudeCodeSwitchHarness is multiProviderHarnessInDir's claude-code-aware
// twin: a session's default model routes to the delegated claude-code
// backend (claudeCode.BinaryPath, the fakeclaude stand-in), while nativeProv
// is registered as an ordinary native provider a later POST
// /session/{id}/model call can switch to — reproducing the live incident's
// exact provider shape (a claude-code/opus session switched mid-session to
// codex/gpt-5.6-sol), which neither server_test.go's newServer (one native
// provider only) nor multiProviderHarnessInDir (no ClaudeCode config seam)
// can build.
func claudeCodeSwitchHarness(t *testing.T, claudeModel message.ModelRef, claudeCode engine.ClaudeCodeConfig, nativeProv provider.Provider) *harness {
	t.Helper()
	dir := t.TempDir()
	reg := provider.Registry{nativeProv.Name(): nativeProv}
	opts := Options{
		SessionDir: dir,
		RunToken:   "secret-run-token",
		Version:    "9.9.9",
		NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
			if m.IsZero() {
				m = claudeModel
			}
			return engine.NewSession(engine.Config{
				Providers:     reg,
				Model:         m,
				WorkDir:       workDir,
				ParentSession: parentSession,
				SessionDir:    dir,
				ClaudeCode:    claudeCode,
			}), nil
		},
		LoadSession: func(id string) (*engine.Session, error) {
			return engine.LoadSession(engine.Config{
				Providers:  reg,
				Model:      claudeModel,
				SessionDir: dir,
				ClaudeCode: claudeCode,
			}, id)
		},
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}
}

// waitIdleClaudeCode polls GET /session/{id}/wait?until=idle, same as
// enqueue_test.go's waitIdle, duplicated here because that helper is
// unexported to its own file's test scope only by convention, not by any
// real package boundary — kept local to avoid coupling this file's churn to
// that one's.
func waitIdleClaudeCode(t *testing.T, h *harness, id string) {
	t.Helper()
	resp, data := h.do("GET", "/session/"+id+"/wait?until=idle&timeout_s=10", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("wait until=idle status %d: %s", resp.StatusCode, data)
	}
}

// TestClaudeCodeModelSwitchAfterRetryableErrorsEndsIdle reproduces the
// literal shape of the live incident on session
// ses_01m1ht79e5fgfbx2cjx4cf4xm8: several claude-code/opus turns end in a
// retryable overloaded error (turn end outcome:error), an operator then
// switches the session's model to a native provider (POST
// /session/{id}/model, mirroring "reason=model_switch" in the harness log),
// and a turn on the new model completes cleanly. The session must end
// status idle, state idle, lineage.status not "running", and queued 0 — not
// stranded busy/running the way the live session was.
func TestClaudeCodeModelSwitchAfterRetryableErrorsEndsIdle(t *testing.T) {
	bin := buildFakeClaudeForServer(t)
	t.Setenv("FAKE_CLAUDE_MODE", "rate_limit_error")
	t.Setenv("FAKE_CLAUDE_LOG", filepath.Join(t.TempDir(), "invocations.jsonl"))

	claudeModel := message.ModelRef{Provider: engine.ClaudeCodeProviderFamily, Model: "sonnet"}
	nativeProv := &scriptedProvider{name: "codex", turns: [][]provider.Event{asstTurn("done on codex")}}

	h := claudeCodeSwitchHarness(t, claudeModel, engine.ClaudeCodeConfig{BinaryPath: bin}, nativeProv)
	id := h.createSession("")

	// Several claude-code turns, each its own separate prompt_async call
	// (claude-code has no internal retry outside a goal loop — see
	// engine/claude_code_backend.go's package doc), each ending in a
	// retryable error and returning the session to idle.
	for i := 0; i < 3; i++ {
		resp, data := h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
			"parts": []map[string]string{{"type": "text", "text": "go"}},
		})
		if resp.StatusCode != 202 {
			t.Fatalf("prompt_async #%d status %d: %s", i, resp.StatusCode, data)
		}
		waitIdleClaudeCode(t, h, id)
	}

	resp, data := h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET session status %d: %s", resp.StatusCode, data)
	}
	var mid struct {
		Status   string               `json:"status"`
		State    string               `json:"state"`
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
		Lineage  map[string]any       `json:"lineage"`
	}
	mustUnmarshal(t, data, &mid)
	if mid.Status != "idle" {
		t.Fatalf("after 3 failed claude-code turns, status = %q, want idle", mid.Status)
	}
	if mid.LastTurn == nil || mid.LastTurn.Outcome != "error" {
		t.Fatalf("after 3 failed claude-code turns, last_turn = %+v, want outcome error", mid.LastTurn)
	}

	// The operator-driven model switch, decoupled from prompting exactly
	// like handleSetModel's own doc comment describes.
	resp, data = h.do("POST", "/session/"+id+"/model", map[string]string{"model": "codex/gpt-5.6-sol"})
	if resp.StatusCode != 200 {
		t.Fatalf("set model status %d: %s", resp.StatusCode, data)
	}

	resp, data = h.do("POST", "/session/"+id+"/prompt_async", map[string]any{
		"parts": []map[string]string{{"type": "text", "text": "go"}},
	})
	if resp.StatusCode != 202 {
		t.Fatalf("final prompt_async status %d: %s", resp.StatusCode, data)
	}
	waitIdleClaudeCode(t, h, id)
	waitForLineageStatus(t, h, id, "idle", 5*time.Second)

	resp, data = h.do("GET", "/session/"+id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("final GET session status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		Status   string               `json:"status"`
		State    string               `json:"state"`
		Queued   int                  `json:"queued"`
		LastTurn *lastTurnJSONForTest `json:"last_turn"`
		Lineage  map[string]any       `json:"lineage"`
	}
	mustUnmarshal(t, data, &got)
	if got.Status != "idle" || got.State != "idle" {
		t.Errorf("final status=%q state=%q, want idle/idle (stranded busy is the bug)", got.Status, got.State)
	}
	if got.Lineage["status"] == "running" {
		t.Errorf("final lineage.status = %v, want not running (stranded lineage is the bug)", got.Lineage["status"])
	}
	if got.Queued != 0 {
		t.Errorf("final queued = %d, want 0", got.Queued)
	}
	if got.LastTurn == nil || got.LastTurn.Outcome != "completed" {
		t.Errorf("final last_turn = %+v, want outcome completed", got.LastTurn)
	}
}
