package e2e

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/internal/testpoll"
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
	// extra adds top-level keys to the config, for example plugins.
	extra map[string]any
	// historyTool makes each turn of the CLI call get_conversation_history on
	// the hosted MCP server after the result, for a host with no route to it.
	historyTool bool
	// callTool makes each turn of the CLI call this tool of the hosted MCP
	// server, with callArgs as its arguments, after the result.
	callTool string
	callArgs map[string]any
	// listTools makes each turn of the CLI list the tools of the hosted MCP
	// server after the result.
	listTools bool
	// initTools is the JSON list of tools in the init frame of the CLI, which a
	// session with an allowed list checks.
	initTools string
	// mirror sets session_mirror on the claude-code provider.
	mirror bool
	// signals makes the CLI handle a SIGINT as the real one does, by printing
	// the frames of its mode and exiting.
	signals bool
	// spawnModes runs the CLI in another mode in the spawns that it lists,
	// as "n=mode,...", counting spawns from 1.
	spawnModes string
	// env adds FAKE_CLAUDE_* and FAKECLAUDE_* variables to the environment of the CLI.
	env map[string]string
}

// claudeLogs are the files where fakeclaude records what it received.
type claudeLogs struct {
	argvLog, stdinLog, mcpLog, toolLog, cwdLog, seenLog, signalLog, stateDir string
}

func (l claudeLogs) env(lane claudeLane) map[string]string {
	env := map[string]string{
		"FAKE_CLAUDE_MODE":           lane.mode,
		"FAKE_CLAUDE_LOG":            l.argvLog,
		"FAKE_CLAUDE_STDIN_LOG":      l.stdinLog,
		"FAKE_CLAUDE_MCP_CONFIG_LOG": l.mcpLog,
		"FAKE_CLAUDE_STATE":          filepath.Join(l.stateDir, "parked"),
		"FAKE_CLAUDE_CWD_LOG":        l.cwdLog,
		"FAKE_CLAUDE_MIRROR_SEEN":    l.seenLog,
	}
	if lane.signals {
		env["FAKE_CLAUDE_SIGNAL_LOG"] = l.signalLog
	}
	if lane.spawnModes != "" {
		env["FAKE_CLAUDE_SPAWN_MODES"] = lane.spawnModes
	}
	maps.Copy(env, lane.env)
	if lane.historyTool {
		env["FAKE_CLAUDE_CALL_TOOL"], env["FAKE_CLAUDE_TOOL_LOG"] = "get_conversation_history", l.toolLog
	}
	if lane.callTool != "" {
		args, _ := json.Marshal(lane.callArgs)
		env["FAKE_CLAUDE_CALL_TOOL"], env["FAKE_CLAUDE_TOOL_LOG"], env["FAKE_CLAUDE_CALL_ARGS"] = lane.callTool, l.toolLog, string(args)
	}
	if lane.listTools {
		env["FAKE_CLAUDE_LIST_TOOLS"] = l.toolLog
	}
	if lane.initTools != "" {
		env["FAKE_CLAUDE_INIT_TOOLS"] = lane.initTools
	}
	return env
}

// claudeDriver is the driver of a host that delegates to fakeclaude.
type claudeDriver struct {
	laneHost
	claudeLogs
}

func (l claudeLane) newDriver(t *testing.T, h host, modelURL string) driver {
	t.Helper()
	cfg := map[string]any{
		"model": "claude-code/sonnet",
		"providers": map[string]any{
			"anthropic":   map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": modelURL},
			"claude-code": map[string]any{"type": "claude-code-cli", "binary_path": fakeClaudePath(), "session_mirror": l.mirror},
		},
	}
	if l.mcp != nil {
		cfg["mcp_servers"] = l.mcp
	}
	maps.Copy(cfg, l.extra)
	stateDir := t.TempDir()
	logs := claudeLogs{stateDir: stateDir, mcpLog: filepath.Join(stateDir, "mcp-config.jsonl"), toolLog: filepath.Join(stateDir, "tool.jsonl"),
		argvLog: filepath.Join(stateDir, "argv.jsonl"), stdinLog: filepath.Join(stateDir, "stdin.jsonl"),
		cwdLog: filepath.Join(stateDir, "cwd"), seenLog: filepath.Join(stateDir, "seen.jsonl"), signalLog: filepath.Join(stateDir, "signals")}
	var args []string
	if l.ask {
		args = append(args, "--ask-user-question")
	}
	return &claudeDriver{laneHost: h.open(t, writeGoalConfigWith(t, modelURL, cfg), logs.env(l), args...), claudeLogs: logs}
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
	f, err := os.Open(claudeDriverOf(t, r).argvLog)
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
	facts := map[string]any{
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
	settings, _ := value("--settings").(string)
	if strings.Contains(settings, "defer") {
		facts["defer_hook"] = true
		if call := deferredCall.FindString(settings); call != "" {
			facts["deferred_call"] = call
		}
	}
	return facts
}

// deferredCall matches the id of the tool call that a resumed run lets pass
// its defer hook.
var deferredCall = regexp.MustCompile(`toolu_\w+`)

// claudeSystemPrompt records, for each fakeclaude spawn in order, whether the
// system prompt that harness appended holds text.
type claudeSystemPrompt struct{ as, contains string }

func (a claudeSystemPrompt) run(t *testing.T, r *run) {
	data, err := os.ReadFile(claudeDriverOf(t, r).argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	out := []any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("decode argv line %q: %v", line, err)
		}
		i := slices.Index(argv, "--append-system-prompt")
		out = append(out, i >= 0 && i+1 < len(argv) && strings.Contains(argv[i+1], a.contains))
	}
	r.record(t, "claude_system_prompt", a.as, callResult{Status: http.StatusOK, Body: out})
}

// claudeMCPConfig records the operator servers of the --mcp-config file of
// each invocation, and whether the file also names the harness tools bridge.
// The bridge entry holds a port and a token, so it is not recorded.
type claudeMCPConfig struct{ as string }

func (a claudeMCPConfig) run(t *testing.T, r *run) {
	data, err := os.ReadFile(claudeDriverOf(t, r).mcpLog)
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
	if f, err := os.Open(claudeDriverOf(t, r).stdinLog); err == nil {
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

// resolution is the body of a call that resolves an open question: the JSON
// text of an answer, with an empty text for none, or a dismissal.
type resolution struct {
	answer  string
	dismiss bool
}

// claudeAnswer posts an answer to a parked question. The answer starts a
// turn, so the scenario waits for idle afterwards.
type claudeAnswer struct {
	as, callID string
	answers    map[string]string
}

func (a claudeAnswer) run(t *testing.T, r *run) {
	body, err := json.Marshal(a.answers)
	if err != nil {
		t.Fatal(err)
	}
	r.record(t, "answer_question", a.as, claudeDriverOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{answer: string(body)}))
}

// claudeRawAnswer posts the JSON text answer as the answer of a parked
// question, whatever its shape. An empty answer sends no answer field.
// A dismiss flag sends the dismissal in the same body.
type claudeRawAnswer struct {
	as, callID, answer string
	dismiss            bool
}

func (a claudeRawAnswer) run(t *testing.T, r *run) {
	r.record(t, "answer_question", a.as, claudeDriverOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{answer: a.answer, dismiss: a.dismiss}))
}

// claudeDismiss dismisses a parked question.
type claudeDismiss struct{ as, callID string }

func (a claudeDismiss) run(t *testing.T, r *run) {
	r.record(t, "dismiss_question", a.as, claudeDriverOf(t, r).resolveQuestion(t, r.id(t, a.as), a.callID, resolution{dismiss: true}))
}

// claudeHistoryTool calls get_conversation_history on the session's hosted MCP
// endpoint, the tool a resumed delegated turn reads prior history through.
type claudeHistoryTool struct{ as string }

func (a claudeHistoryTool) run(t *testing.T, r *run) {
	r.record(t, "history_tool", a.as, claudeDriverOf(t, r).lastToolCall(t))
}

// lastToolCall is the response that the CLI of the newest run got from the
// history tool, which a run reads after its result, once harness has recorded
// every item of the turn.
func (d *claudeDriver) lastToolCall(t *testing.T) callResult {
	t.Helper()
	data, err := os.ReadFile(d.toolLog)
	if err != nil {
		t.Fatalf("read tool log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return callResult{Status: http.StatusOK, Body: decodeBody(t, "tool call", []byte(lines[len(lines)-1]))}
}

// claudeOfferedTools records the sorted names of the tools that the hosted
// MCP server offered the CLI after its newest run.
type claudeOfferedTools struct{ as string }

func (a claudeOfferedTools) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	names := []string{}
	for _, tool := range list {
		m, _ := tool.(map[string]any)
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	slices.Sort(names)
	r.record(t, "offered_tools", a.as, callResult{Status: http.StatusOK, Body: names})
}

// claudeOfferedModelTool records the description and the input schema of the
// model tool that the hosted MCP server offered the CLI after its newest run.
type claudeOfferedModelTool struct{ as string }

func (a claudeOfferedModelTool) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).lastToolCall(t).Body.(map[string]any)
	result, _ := body["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	var entry any
	for _, tool := range list {
		if m, _ := tool.(map[string]any); m["name"] == "model" {
			entry = map[string]any{"description": m["description"], "inputSchema": m["inputSchema"]}
		}
	}
	r.record(t, "offered_model_tool", a.as, callResult{Status: http.StatusOK, Body: entry})
}

// claudeToolCall records the response that the CLI of the newest run got from
// the tool that the lane makes it call.
type claudeToolCall struct{ as string }

func (a claudeToolCall) run(t *testing.T, r *run) {
	r.record(t, "tool_call", a.as, claudeDriverOf(t, r).lastToolCall(t))
}

var fakeBinPath = regexp.MustCompile(`"[^"\s]*fakeclaude"`)

// claudeSession records GET /session/{id} without the journal seq, with the
// wall-clock capture time of the subscription usage replaced by a marker, and
// with the path of the fakeclaude binary in last_turn.error masked.
type claudeSession struct{ as string }

func (a claudeSession) run(t *testing.T, r *run) {
	res := claudeDriverOf(t, r).GetSession(t, r.id(t, a.as))
	if body, ok := res.Body.(map[string]any); ok {
		if sub, ok := body["subscription_usage"].(map[string]any); ok && sub["captured_at"] != json.Number("0") {
			sub["captured_at"] = "<time>"
		}
		if last, ok := body["last_turn"].(map[string]any); ok {
			if msg, ok := last["error"].(string); ok {
				last["error"] = fakeBinPath.ReplaceAllString(msg, "<fakeclaude>")
			}
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

// claudeJournalEvents records the type and compaction fields of the session's
// journal events whose type starts with prefix, in journal order.
type claudeJournalEvents struct{ as, prefix string }

func (a claudeJournalEvents) run(t *testing.T, r *run) {
	events := claudeDriverOf(t, r).journalEvents(t, r.id(t, a.as), a.prefix)
	r.record(t, "journal_events", a.as, callResult{Status: http.StatusOK, Body: events})
}

var _ laneHost = (*runtimeDriver)(nil)

// awaitAssistantText also takes the streamed text: the CLI can wait for
// input before its message completes. A message that completes right after its
// text is recorded by then, so a kill that follows loses nothing: the wait for
// the record is bounded, because a message that waits for input never completes.
func (d *runtimeDriver) awaitAssistantText(t *testing.T, id, text string) {
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
	testpoll.UntilNoT(recordGrace, func() bool {
		for _, ev := range d.events(t, id) {
			if ev.Kind == "item.completed" {
				if it := decodeEvent[logItem](t, ev); it.Message.Role == "assistant" && strings.Contains(partsText(it.Message.Parts), text) {
					return true
				}
			}
		}
		return false
	}, 20*time.Millisecond)
}

// recordGrace bounds the wait of awaitAssistantText for the record of a message.
const recordGrace = 2 * time.Second

func (d *runtimeDriver) resolveQuestion(t *testing.T, id, callID string, res resolution) callResult {
	t.Helper()
	var fields []string
	if res.dismiss {
		fields = append(fields, `"dismiss":true`)
	}
	if res.answer != "" {
		fields = append(fields, `"answer":`+res.answer)
	}
	return d.call(t, http.MethodPost, "/sessions/"+id+"/requests/"+callID, rawBody("{"+strings.Join(fields, ",")+"}"))
}

// journalEvents lists the durable events of session id whose kind starts
// with prefix, with the fields of a compaction.
func (d *runtimeDriver) journalEvents(t *testing.T, id, prefix string) []any {
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

// messageParents lists the role and the parent tool call of each message of
// session id, in transcript order: an input and an item.
func (d *runtimeDriver) messageParents(t *testing.T, id string) callResult {
	t.Helper()
	var out []any
	add := func(role, parent string) {
		out = append(out, map[string]any{"role": role, "parent_tool_use_id": parent})
	}
	for _, ev := range d.events(t, id) {
		switch ev.Kind {
		case "turn.started":
			for range decodeEvent[struct {
				InputIDs []string `json:"input_ids"`
			}](t, ev).InputIDs {
				add("user", "")
			}
		case "input.promoted":
			add("user", "")
		case "item.completed":
			it := decodeEvent[struct {
				Message struct {
					Role   string `json:"role"`
					Parent string `json:"parent_call_id"`
				} `json:"message"`
			}](t, ev)
			add(it.Message.Role, it.Message.Parent)
		}
	}
	return callResult{Status: http.StatusOK, Body: out}
}

// claudeMirror records, for each fakeclaude spawn in order, the session that
// it resumed, whether it ran with the session mirror, and a digest of each
// transcript file that it found in its config dir at start.
type claudeMirror struct{ as string }

func (a claudeMirror) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	var seen []struct {
		Files map[string]string `json:"files"`
	}
	for _, line := range fileLines(t, d.seenLog) {
		var s struct {
			Files map[string]string `json:"files"`
		}
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			t.Fatalf("decode seen line %q: %v", line, err)
		}
		seen = append(seen, s)
	}
	out := []any{}
	for i, line := range fileLines(t, d.argvLog) {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("decode argv line %q: %v", line, err)
		}
		transcripts := map[string]string{}
		if i < len(seen) {
			for name, content := range seen[i].Files {
				sum := sha256.Sum256([]byte(content))
				transcripts[name] = fmt.Sprintf("%d lines, sha256 %s", strings.Count(content, "\n"), hex.EncodeToString(sum[:6]))
			}
		}
		out = append(out, map[string]any{
			"resume":         argvFacts(argv)["resume"],
			"session_mirror": slices.Contains(argv, "--session-mirror"),
			"transcripts":    transcripts,
		})
	}
	r.record(t, "claude_mirror", a.as, callResult{Status: http.StatusOK, Body: out})
}

// fileLines reads the lines of a log that fakeclaude appends to. A log that
// no spawn wrote is empty.
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// claudeUsage records the input and output tokens that the session has used,
// the one part of the session view that serve and the runtime share.
type claudeUsage struct{ as string }

func (a claudeUsage) run(t *testing.T, r *run) {
	body, _ := claudeDriverOf(t, r).GetSession(t, r.id(t, a.as)).Body.(map[string]any)
	usage, _ := body["usage"].(map[string]any)
	r.record(t, "claude_usage", a.as, callResult{Status: http.StatusOK, Body: map[string]any{
		"input_tokens": usage["input_tokens"], "output_tokens": usage["output_tokens"],
	}})
}

// claudeSignals records how many SIGINT the CLI handled, which only a lane
// with signals set counts.
type claudeSignals struct{ as string }

func (a claudeSignals) run(t *testing.T, r *run) {
	n := len(fileLines(t, claudeDriverOf(t, r).signalLog))
	r.record(t, "claude_signals", a.as, callResult{Status: http.StatusOK, Body: map[string]any{"sigint": n}})
}

// claudeWorkDir records whether each fakeclaude spawn ran in the work dir of
// the host.
type claudeWorkDir struct{ as string }

func (a claudeWorkDir) run(t *testing.T, r *run) {
	d := claudeDriverOf(t, r)
	want, err := filepath.EvalSymlinks(d.Workdir())
	if err != nil {
		t.Fatal(err)
	}
	out := []any{}
	for _, line := range fileLines(t, d.cwdLog) {
		got, err := filepath.EvalSymlinks(line)
		out = append(out, err == nil && got == want)
	}
	r.record(t, "claude_work_dir", a.as, callResult{Status: http.StatusOK, Body: out})
}

// claudeBackendStates records the number of distinct blobs that the
// backend.state events of the session name.
type claudeBackendStates struct{ as string }

func (a claudeBackendStates) run(t *testing.T, r *run) {
	r.record(t, "backend_states", a.as, claudeDriverOf(t, r).backendStateKeys(t, r.id(t, a.as)))
}

func (d *runtimeDriver) backendStateKeys(t *testing.T, id string) callResult {
	t.Helper()
	keys := map[string]bool{}
	for _, ev := range d.events(t, id) {
		if ev.Kind == "backend.state" {
			keys[decodeEvent[struct {
				BlobKey string `json:"blob_key"`
			}](t, ev).BlobKey] = true
		}
	}
	return callResult{Status: http.StatusOK, Body: map[string]any{"blobs": len(keys)}}
}

// claudeCompactKeeping asks the session to compact and keep its newest turns.
type claudeCompactKeeping struct {
	as   string
	keep int
}

func (a claudeCompactKeeping) run(t *testing.T, r *run) {
	r.record(t, "compact_keeping", a.as, claudeDriverOf(t, r).compactKeeping(t, r.id(t, a.as), a.keep))
}

func (d *runtimeDriver) compactKeeping(t *testing.T, id string, keep int) callResult {
	t.Helper()
	return d.call(t, http.MethodPost, "/sessions/"+id+"/compact", map[string]any{"keep_turns": keep})
}
