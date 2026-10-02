package e2e

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeClaudePath is the harnesstest/fakeclaude binary that buildHarness
// compiles beside the harness binary.
func fakeClaudePath() string { return filepath.Join(filepath.Dir(harnessBin), "fakeclaude") }

// claudeLane describes one serve process whose default model is the
// delegated claude-code backend, run against fakeclaude in the given mode.
type claudeLane struct {
	mode string
	ask  bool // pass --ask-user-question to serve
}

// claudeDriver is an httpDriver whose serve process delegates to fakeclaude.
type claudeDriver struct {
	*httpDriver
	lane     claudeLane
	argvLog  string
	stateDir string
}

func (l claudeLane) newDriver(t *testing.T, modelURL string) driver {
	t.Helper()
	cfg := map[string]any{
		"model": "claude-code/sonnet",
		"providers": map[string]any{
			"anthropic":   map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": modelURL},
			"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()},
		},
	}
	d := &claudeDriver{lane: l, stateDir: t.TempDir()}
	d.argvLog = filepath.Join(d.stateDir, "argv.jsonl")
	d.httpDriver = &httpDriver{
		sessDir: t.TempDir(),
		workDir: t.TempDir(),
		config:  writeGoalConfigWith(t, modelURL, cfg),
		enqSeq:  map[string]int64{},
	}
	d.p = d.serve(t)
	return d
}

func (d *claudeDriver) serve(t *testing.T) *serveProc {
	t.Helper()
	addr := freeAddr(t)
	args := []string{"serve", "-addr", addr}
	if d.lane.ask {
		args = append(args, "--ask-user-question")
	}
	cmd := exec.Command(harnessBin, args...)
	cmd.Dir = d.workDir
	cmd.Env = cleanEnv(map[string]string{
		"HARNESS_RUN_TOKEN":   testToken,
		"HARNESS_SESSION_DIR": d.sessDir,
		"HARNESS_CONFIG":      d.config,
		"ANTHROPIC_API_KEY":   "e2e-dummy-key",
		"FAKE_CLAUDE_MODE":    d.lane.mode,
		"FAKE_CLAUDE_LOG":     d.argvLog,
		"FAKE_CLAUDE_STATE":   filepath.Join(d.stateDir, "parked"),
	})
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	p := &serveProc{procGroup: startGroup(t, cmd), t: t, addr: addr, stderr: stderr}
	p.waitHealthy()
	return p
}

func (d *claudeDriver) Restart(t *testing.T, kill bool) {
	t.Helper()
	if kill {
		d.p.kill()
	} else {
		d.p.terminate(t)
	}
	d.p = d.serve(t)
}

func claudeDriverOf(t *testing.T, r *run) *claudeDriver {
	t.Helper()
	d, ok := r.drv.(*claudeDriver)
	if !ok {
		t.Fatalf("scenario action needs a claude lane driver, got %T", r.drv)
	}
	return d
}

// claudeInvocations records, for each fakeclaude spawn in order, the argv
// facts a delegated turn depends on. The argv carries ports and temp paths,
// so only stable facts are kept.
type claudeInvocations struct{ as string }

func (a claudeInvocations) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	f, err := os.Open(d.argvLog)
	if err != nil {
		t.Fatalf("open argv log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var argv []string
		if err := json.Unmarshal(sc.Bytes(), &argv); err != nil {
			t.Fatalf("decode argv line %q: %v", sc.Text(), err)
		}
		out = append(out, argvFacts(argv))
	}
	r.record(t, "claude_invocations", a.as, callResult{Status: http.StatusOK, Body: out})
}

func argvFacts(argv []string) map[string]any {
	value := func(flag string) any {
		if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
			return argv[i+1]
		}
		return nil
	}
	appended, _ := value("--append-system-prompt").(string)
	return map[string]any{
		"model":              value("--model"),
		"resume":             value("--resume"),
		"history_directive":  strings.Contains(appended, "get_conversation_history"),
		"mcp_config":         slices.Contains(argv, "--mcp-config"),
		"question_tool_open": slices.Contains(argv, "--permission-prompt-tool"),
	}
}

// claudeAwaitText blocks on the event stream until the session journals an
// assistant message holding text.
type claudeAwaitText struct{ as, text string }

func (a claudeAwaitText) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	err := claudeDriverOf(t, r).scan(t, func(raw []byte) bool {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Message   *struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Type != "message" || ev.SessionID != id || ev.Message == nil || ev.Message.Role != "assistant" {
			return false
		}
		for _, p := range ev.Message.Parts {
			if strings.Contains(p.Text, a.text) {
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("no assistant message containing %q: %v\nstderr:\n%s", a.text, err, r.drv.Stderr())
	}
}

// claudeAnswer posts an answer to a parked question. The answer starts a
// turn, so the scenario waits for idle afterwards.
type claudeAnswer struct {
	as, callID string
	answers    map[string]string
}

func (a claudeAnswer) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	path := "/session/" + r.id(t, a.as) + "/question/" + a.callID + "/answer"
	r.record(t, "answer_question", a.as, d.call(t, http.MethodPost, path, map[string]any{"answers": a.answers}))
}

// claudeHistoryTool calls get_conversation_history on the session's hosted MCP
// endpoint, the tool a resumed delegated turn reads prior history through.
type claudeHistoryTool struct{ as string }

func (a claudeHistoryTool) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	rpc := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_conversation_history", "arguments": map[string]any{}},
	}
	r.record(t, "history_tool", a.as, d.call(t, http.MethodPost, "/session/"+r.id(t, a.as)+"/mcp", rpc))
}

// claudeSession records GET /session/{id} without the journal seq, and with
// the wall-clock capture time of the subscription usage replaced by a marker.
type claudeSession struct{ as string }

func (a claudeSession) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	res := d.call(t, http.MethodGet, "/session/"+r.id(t, a.as), nil)
	if body, ok := res.Body.(map[string]any); ok {
		delete(body, "seq")
		if sub, ok := body["subscription_usage"].(map[string]any); ok && sub["captured_at"] != json.Number("0") {
			sub["captured_at"] = "<time>"
		}
	}
	r.record(t, "get_session", a.as, res)
}

// claudeMessageParents records the parent_tool_use_id of each transcript
// message, which the oracle's transcript vocabulary leaves out.
type claudeMessageParents struct{ as string }

func (a claudeMessageParents) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	resp, data := d.p.do(http.MethodGet, "/session/"+r.id(t, a.as)+"/message", nil)
	var msgs []struct {
		Role   string `json:"role"`
		Parent string `json:"parent_tool_use_id"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatalf("decode messages: %v (%s)", err, data)
	}
	var out []any
	for _, m := range msgs {
		out = append(out, map[string]any{"role": m.Role, "parent_tool_use_id": m.Parent})
	}
	r.record(t, "message_parents", a.as, callResult{Status: resp.StatusCode, Body: out})
}

// claudeJournalTypes records the types of the session's journal events that start
// with prefix, in journal order.
type claudeJournalTypes struct{ as, prefix string }

func (a claudeJournalTypes) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	id := r.id(t, a.as)
	var tip struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(d.expect(t, http.StatusOK, http.MethodGet, "/event/tip", nil), &tip); err != nil {
		t.Fatalf("decode tip: %v", err)
	}
	types := []any{}
	err := d.scan(t, func(raw []byte) bool {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Seq       int64  `json:"seq"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Seq == 0 {
			return false
		}
		if ev.SessionID == id && strings.HasPrefix(ev.Type, a.prefix) {
			types = append(types, ev.Type)
		}
		return ev.Seq >= tip.Seq
	})
	if err != nil {
		t.Fatalf("journal scan: %v\nstderr:\n%s", err, d.Stderr())
	}
	r.record(t, "journal_types", a.as, callResult{Status: http.StatusOK, Body: types})
}
