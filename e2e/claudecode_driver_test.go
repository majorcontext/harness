package e2e

import (
	"bufio"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

// fakeClaudePath is the harnesstest/fakeclaude binary that buildHarness
// compiles beside the harness binary.
func fakeClaudePath() string { return filepath.Join(filepath.Dir(harnessBin), "fakeclaude") }

// claudeLane describes one host whose default model is the delegated
// claude-code backend, run against fakeclaude in the given mode.
type claudeLane struct {
	mode string
	ask  bool           // pass --ask-user-question to serve
	mcp  map[string]any // config mcp_servers
}

// claudeLogs are the files where fakeclaude records what it received.
type claudeLogs struct {
	argvLog, stdinLog, mcpLog, stateDir string
}

func (l claudeLogs) env(mode string) map[string]string {
	return map[string]string{
		"FAKE_CLAUDE_MODE":           mode,
		"FAKE_CLAUDE_LOG":            l.argvLog,
		"FAKE_CLAUDE_STDIN_LOG":      l.stdinLog,
		"FAKE_CLAUDE_MCP_CONFIG_LOG": l.mcpLog,
		"FAKE_CLAUDE_STATE":          filepath.Join(l.stateDir, "parked"),
	}
}

// laneDriver is a driver of a claude lane. The methods that a lane action
// needs read the API of the host.
type laneDriver interface {
	driver
	logs() claudeLogs
	awaitAssistantText(t *testing.T, id, text string)
	answerQuestion(t *testing.T, id, callID string, answers map[string]string) callResult
	historyTool(t *testing.T, id string) callResult
	messageParents(t *testing.T, id string) callResult
	journalEvents(t *testing.T, id, prefix string) []any
}

// claudeDriver is an httpDriver whose serve process delegates to fakeclaude.
type claudeDriver struct {
	*httpDriver
	lane claudeLane
	claudeLogs
}

// claudeRuntimeDriver is a runtimeDriver that delegates to fakeclaude. The
// fakeclaude settings are process environment, so its row runs alone.
type claudeRuntimeDriver struct {
	*runtimeDriver
	claudeLogs
}

func (l claudeLane) newDriver(t *testing.T, h host, modelURL string) driver {
	t.Helper()
	cfg := map[string]any{
		"model": "claude-code/sonnet",
		"providers": map[string]any{
			"anthropic":   map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": modelURL},
			"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath()},
		},
	}
	if l.mcp != nil {
		cfg["mcp_servers"] = l.mcp
	}
	stateDir := t.TempDir()
	logs := claudeLogs{stateDir: stateDir, mcpLog: filepath.Join(stateDir, "mcp-config.jsonl"),
		argvLog: filepath.Join(stateDir, "argv.jsonl"), stdinLog: filepath.Join(stateDir, "stdin.jsonl")}
	config := writeGoalConfigWith(t, modelURL, cfg)
	if h.name == runtimeHost.name {
		if l.ask {
			t.Fatal("the runtime has no --ask-user-question; requests come in phase 5")
		}
		for k, v := range logs.env(l.mode) {
			t.Setenv(k, v)
		}
		return &claudeRuntimeDriver{runtimeDriver: newRuntimeDriverIn(t, t.TempDir(), config), claudeLogs: logs}
	}
	d := &claudeDriver{lane: l, claudeLogs: logs}
	d.httpDriver = &httpDriver{sessDir: t.TempDir(), workDir: t.TempDir(), config: config, enqSeq: map[string]int64{}}
	d.p = d.serve(t)
	return d
}

func (d *claudeDriver) serve(t *testing.T) *serveProc {
	t.Helper()
	var args []string
	if d.lane.ask {
		args = append(args, "--ask-user-question")
	}
	env := d.env(d.lane.mode)
	maps.Copy(env, map[string]string{"HARNESS_SESSION_DIR": d.sessDir, "HARNESS_CONFIG": d.config, "ANTHROPIC_API_KEY": "e2e-dummy-key"})
	return startServeProc(t, freeAddr, d.workDir, env, args...)
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

func (l claudeLogs) logs() claudeLogs { return l }

func claudeDriverOf(t *testing.T, r *run) laneDriver {
	t.Helper()
	d, ok := r.drv.(laneDriver)
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
	f, err := os.Open(claudeDriverOf(t, r).logs().argvLog)
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

		"forward_subagent_text": slices.Contains(argv, "--forward-subagent-text"),
		"thinking_display":      value("--thinking-display"),
		"disallowed_tools":      value("--disallowedTools"),
		"strict_mcp_config":     slices.Contains(argv, "--strict-mcp-config"),
	}
}

// claudeMCPConfig records the operator servers of the --mcp-config file of
// each invocation, and whether the file also names the harness tools bridge.
// The bridge entry holds a port and a token, so it is not recorded.
type claudeMCPConfig struct{ as string }

func (a claudeMCPConfig) run(t *testing.T, r *run) {
	data, err := os.ReadFile(claudeDriverOf(t, r).logs().mcpLog)
	if err != nil {
		t.Fatalf("read mcp config log: %v", err)
	}
	out := []any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var f struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("decode mcp config %q: %v", line, err)
		}
		bridge := false
		for name := range f.MCPServers {
			if strings.HasPrefix(name, "harness") {
				bridge = true
				delete(f.MCPServers, name)
			}
		}
		out = append(out, map[string]any{"servers": f.MCPServers, "bridge": bridge})
	}
	r.record(t, "claude_mcp_config", a.as, callResult{Status: http.StatusOK, Body: out})
}

// claudeInputs records, in order across spawns, each stdin line harness wrote
// to fakeclaude: the frame type with its role and content, or the
// control_response subtype with its interrupt flag.
type claudeInputs struct{ as string }

func (a claudeInputs) run(t *testing.T, r *run) {
	out := []any{}
	if f, err := os.Open(claudeDriverOf(t, r).logs().stdinLog); err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			out = append(out, inputFacts(t, sc.Bytes()))
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("open stdin log: %v", err)
	}
	r.record(t, "claude_inputs", a.as, callResult{Status: http.StatusOK, Body: out})
}

func inputFacts(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"message"`
		Response struct {
			Subtype  string `json:"subtype"`
			Response struct {
				Interrupt bool `json:"interrupt"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode stdin line %q: %v", line, err)
	}
	if m.Type == "control_response" {
		return map[string]any{"type": m.Type, "subtype": m.Response.Subtype, "interrupt": m.Response.Response.Interrupt}
	}
	return map[string]any{"type": m.Type, "role": m.Message.Role, "content": m.Message.Content}
}

// claudeAwaitText blocks on the event stream until the session journals an
// assistant message holding text.
type claudeAwaitText struct{ as, text string }

func (a claudeAwaitText) run(t *testing.T, r *run) {
	t.Helper()
	claudeDriverOf(t, r).awaitAssistantText(t, r.id(t, a.as), a.text)
}

func (d *claudeDriver) awaitAssistantText(t *testing.T, id, text string) {
	t.Helper()
	err := d.scan(t, func(raw []byte) bool {
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
			if strings.Contains(p.Text, text) {
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("no assistant message containing %q: %v\nstderr:\n%s", text, err, d.Stderr())
	}
}

// claudeAnswer posts an answer to a parked question. The answer starts a
// turn, so the scenario waits for idle afterwards.
type claudeAnswer struct {
	as, callID string
	answers    map[string]string
}

func (a claudeAnswer) run(t *testing.T, r *run) {
	r.record(t, "answer_question", a.as, claudeDriverOf(t, r).answerQuestion(t, r.id(t, a.as), a.callID, a.answers))
}

func (d *claudeDriver) answerQuestion(t *testing.T, id, callID string, answers map[string]string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/session/"+id+"/question/"+callID+"/answer", map[string]any{"answers": answers})
}

// claudeHistoryTool calls get_conversation_history on the session's hosted MCP
// endpoint, the tool a resumed delegated turn reads prior history through.
type claudeHistoryTool struct{ as string }

func (a claudeHistoryTool) run(t *testing.T, r *run) {
	r.record(t, "history_tool", a.as, claudeDriverOf(t, r).historyTool(t, r.id(t, a.as)))
}

func (d *claudeDriver) historyTool(t *testing.T, id string) callResult {
	t.Helper()
	rpc := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "get_conversation_history", "arguments": map[string]any{}},
	}
	return d.call(t, http.MethodPost, "/session/"+id+"/mcp", rpc)
}

// claudeSession records GET /session/{id} without the journal seq, and with
// the wall-clock capture time of the subscription usage replaced by a marker.
type claudeSession struct{ as string }

func (a claudeSession) run(t *testing.T, r *run) {
	res := claudeDriverOf(t, r).GetSession(t, r.id(t, a.as))
	if body, ok := res.Body.(map[string]any); ok {
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
	r.record(t, "message_parents", a.as, claudeDriverOf(t, r).messageParents(t, r.id(t, a.as)))
}

func (d *claudeDriver) messageParents(t *testing.T, id string) callResult {
	t.Helper()
	resp, data := d.p.do(http.MethodGet, "/session/"+id+"/message", nil)
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
	return callResult{Status: resp.StatusCode, Body: out}
}

// claudeJournalEvents records the type and compaction fields of the session's
// journal events whose type starts with prefix, in journal order.
type claudeJournalEvents struct{ as, prefix string }

func (a claudeJournalEvents) run(t *testing.T, r *run) {
	events := claudeDriverOf(t, r).journalEvents(t, r.id(t, a.as), a.prefix)
	r.record(t, "journal_events", a.as, callResult{Status: http.StatusOK, Body: events})
}

func (d *claudeDriver) journalEvents(t *testing.T, id, prefix string) []any {
	t.Helper()
	var tip struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(d.expect(t, http.StatusOK, http.MethodGet, "/event/tip", nil), &tip); err != nil {
		t.Fatalf("decode tip: %v", err)
	}
	events := []any{}
	err := d.scan(t, func(raw []byte) bool {
		var ev struct {
			Type       string `json:"type"`
			SessionID  string `json:"session_id"`
			Seq        int64  `json:"seq"`
			Trigger    string `json:"trigger"`
			PreTokens  int    `json:"pre_tokens"`
			PostTokens int    `json:"post_tokens"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Seq == 0 {
			return false
		}
		if ev.SessionID == id && strings.HasPrefix(ev.Type, prefix) {
			events = append(events, map[string]any{
				"type": ev.Type, "trigger": ev.Trigger, "pre_tokens": ev.PreTokens, "post_tokens": ev.PostTokens,
			})
		}
		return ev.Seq >= tip.Seq
	})
	if err != nil {
		t.Fatalf("journal scan: %v\nstderr:\n%s", err, d.Stderr())
	}
	return events
}

var (
	_ laneDriver = (*claudeDriver)(nil)
	_ laneDriver = (*claudeRuntimeDriver)(nil)
)

// awaitAssistantText also takes the streamed text: the CLI can wait for
// input before its message completes.
func (d *claudeRuntimeDriver) awaitAssistantText(t *testing.T, id, text string) {
	t.Helper()
	var streamed strings.Builder
	d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
		switch ev.Kind {
		case protocol.KindItemDelta:
			streamed.WriteString(decodeEvent[protocol.ItemFrame](t, ev).Text)
			return strings.Contains(streamed.String(), text)
		case "item.completed":
			it := decodeEvent[logItem](t, ev)
			return it.Message.Role == "assistant" && strings.Contains(partsText(it.Message.Parts), text)
		}
		return false
	})
}

func (d *claudeRuntimeDriver) answerQuestion(t *testing.T, id, callID string, answers map[string]string) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/sessions/"+id+"/requests/"+callID, map[string]any{"answer": answers})
}

func (d *claudeRuntimeDriver) historyTool(t *testing.T, _ string) callResult {
	t.Helper()
	t.Fatal("the runtime serves no history tool")
	return callResult{}
}

func (d *claudeRuntimeDriver) messageParents(t *testing.T, _ string) callResult {
	t.Helper()
	t.Fatal("the runtime log records no parent tool use")
	return callResult{}
}

// journalEvents lists the durable events of session id whose kind starts
// with prefix, with the fields of a compaction.
func (d *claudeRuntimeDriver) journalEvents(t *testing.T, id, prefix string) []any {
	t.Helper()
	out := []any{}
	for _, ev := range d.events(t, id) {
		if strings.HasPrefix(ev.Kind, prefix) {
			c := decodeEvent[struct {
				ByBackend bool `json:"by_backend"`
			}](t, ev)
			out = append(out, map[string]any{"type": ev.Kind, "by_backend": c.ByBackend})
		}
	}
	return out
}
