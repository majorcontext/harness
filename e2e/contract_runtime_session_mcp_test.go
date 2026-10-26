package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

const (
	sessionMCPSecretEnv    = "sess-env-secret-7f3a"
	sessionMCPSecretHeader = "sess-header-secret-9c1d"
)

func TestContractRuntimeSessionMCP(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a_session_stdio_server_reaches_the_model", rowASessionStdioServerReachesTheModel},
		{"a_session_stdio_server_stops_at_release", func(t *testing.T) { rowASessionStdioServerStops(t, "release") }},
		{"a_session_stdio_server_stops_at_end", func(t *testing.T) { rowASessionStdioServerStops(t, "end") }},
		{"a_session_stdio_server_stops_at_close", func(t *testing.T) { rowASessionStdioServerStops(t, "close") }},
		{"a_session_server_that_fails_to_connect_parks", rowASessionServerThatFailsToConnectParks},
		{"an_open_without_the_option_has_no_session_tools", rowAnOpenWithoutTheOptionHasNoSessionTools},
		{"no_secret_of_a_session_server_is_in_the_log", rowNoSecretOfASessionServerIsInTheLog},
		{"a_session_tool_reaches_the_cli_through_the_harness_endpoint", rowASessionToolReachesTheCLIThroughTheHarnessEndpoint},
		{"runtime_and_session_servers_answer_in_one_turn_and_status_lists_both", rowRuntimeAndSessionServersAnswerInOneTurn},
		{"no_secret_of_a_connected_session_server_is_in_the_log_or_a_tool_error", rowNoSecretOfAConnectedSessionServerIsInTheLogOrAToolError},
		{"a_session_server_may_not_share_a_name_with_an_embedder_tool", rowASessionServerMayNotShareANameWithAnEmbedderTool},
		{"a_session_server_may_not_share_a_name_with_a_plugin_tool", rowASessionServerMayNotShareANameWithAPluginTool},
		{"a_model_change_keeps_a_session_tool_allowed", rowAModelChangeKeepsASessionToolAllowed},
		{"an_embedder_tool_may_not_take_the_name_of_the_cli_tool_search", rowAnEmbedderToolMayNotTakeTheNameOfTheCLIToolSearch},
		{"a_backend_that_owns_the_loop_allows_a_session_tool_by_name", rowABackendThatOwnsTheLoopAllowsASessionToolByName},
		{"a_session_server_may_not_take_the_name_of_a_runtime_server", rowASessionServerMayNotTakeTheNameOfARuntimeServer},
		{"a_skill_of_a_session_server_stays_held_after_release_and_open", rowASkillOfASessionServerStaysHeldAfterReleaseAndOpen},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func TestContractServeCreateTakesNoMCPServers(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("the_create_body_takes_no_server_field", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("ok"))
			res := d.call(t, http.MethodPost, "/sessions", map[string]any{"mcp_servers": map[string]any{"x": map[string]any{"command": []string{"true"}}}})
			if res.Status != http.StatusBadRequest || errorCode(t, res) != "invalid_request" {
				t.Errorf("response = %d %v, want 400 invalid_request", res.Status, res.Body)
			}
		})
	})
}

type syncLog struct {
	mu     sync.Mutex
	text   strings.Builder
	parked chan struct{}
}

func newSyncLog() *syncLog { return &syncLog{parked: make(chan struct{}, 16)} }

func (l *syncLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *syncLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *syncLog) WithGroup(string) slog.Handler            { return l }

func (l *syncLog) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.text.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		l.text.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	l.text.WriteString("\n")
	l.mu.Unlock()
	if strings.Contains(r.Message, "parked") {
		l.parked <- struct{}{}
	}
	return nil
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func sessionMCPRuntime(t *testing.T, log *slog.Logger, st harness.Store, cfg config.Config, steps ...harnesstest.Step) (*harness.Runtime, *harnesstest.Server) {
	t.Helper()
	fake := harnesstest.NewChat(t, steps...)
	cfg.ContextWindowTokens = 100000
	cfg.Providers = map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "SESSION_MCP_KEY"}}
	if st == nil {
		st = store.NewMemStore()
	}
	r, err := harness.New(harness.Options{Store: st, Config: cfg, Logger: log, Getenv: func(string) string { return "k" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, fake
}

func sessionMCPTurn(t *testing.T, s *harness.Session, id, text string) {
	t.Helper()
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func sessionMCPCreate(t *testing.T, r *harness.Runtime, servers map[string]config.MCPServerSpec) *harness.Session {
	t.Helper()
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}, harness.WithSessionMCP(servers))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func weatherServer(t *testing.T) map[string]config.MCPServerSpec {
	t.Helper()
	return map[string]config.MCPServerSpec{"weather": {Command: []string{mcpStubBin(t)}, Env: mcpWeather("").StdioEnv()}}
}

func requestHas(r harnesstest.Request, name string) bool {
	for _, n := range r.Tools {
		if n == name {
			return true
		}
	}
	return false
}

func requestText(r harnesstest.Request) string {
	var b strings.Builder
	b.WriteString(r.System)
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			b.WriteString("\n" + p.Text)
		}
	}
	return b.String()
}

func rowASessionStdioServerReachesTheModel(t *testing.T) {
	r, fake := sessionMCPRuntime(t, nil, nil, config.Config{},
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "mcp__weather__forecast", Input: map[string]any{"city": "Oslo"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	s := sessionMCPCreate(t, r, weatherServer(t))
	sessionMCPTurn(t, s, "a", "forecast")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	if !requestHas(reqs[0], "mcp__weather__forecast") || !requestHas(reqs[0], "mcp") {
		t.Errorf("tools of the first request = %v, want the session tools", reqs[0].Tools)
	}
	if got := requestText(reqs[1]); !strings.Contains(got, "Oslo: 3C, snow") {
		t.Errorf("the second request does not hold the tool result: %s", got)
	}
}

func rowASessionStdioServerStops(t *testing.T, how string) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	bin := mcpStubBin(t)
	servers := map[string]config.MCPServerSpec{"weather": {
		Command: []string{"sh", "-c", `echo $$ > "$1"; exec "$2"`, "sh", pidFile, bin},
		Env:     mcpWeather("").StdioEnv(),
	}}
	r, _ := sessionMCPRuntime(t, nil, nil, config.Config{}, replyText("ok"))
	s := sessionMCPCreate(t, r, servers)
	sessionMCPTurn(t, s, "a", "go")
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the turn did not start the server: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the server is not running before the stop: %v", err)
	}
	switch how {
	case "release":
		err = s.Release(t.Context())
	case "end":
		err = r.End(t.Context(), "s1")
	default:
		err = r.Close(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("after %s, kill(pid, 0) = %v, want ESRCH: the session server still runs", how, err)
	}
}

func rowASessionServerThatFailsToConnectParks(t *testing.T) {
	log := newSyncLog()
	r, fake := sessionMCPRuntime(t, slog.New(log), nil, config.Config{}, replyText("ok"))
	s := sessionMCPCreate(t, r, map[string]config.MCPServerSpec{"weather": {Command: []string{filepath.Join(t.TempDir(), "no-such-server")}}})
	sessionMCPTurn(t, s, "a", "go")
	<-log.parked
	sessionMCPTurn(t, s, "b", "again")
	reqs := fake.Requests()
	if got := requestText(reqs[len(reqs)-1]); !strings.Contains(got, "[mcp: unavailable") || !strings.Contains(got, "weather") || !strings.Contains(got, `"connect"`) {
		t.Errorf("the last request does not name the parked server: %s", got)
	}
}

func rowAnOpenWithoutTheOptionHasNoSessionTools(t *testing.T) {
	st := store.NewMemStore()
	r, fake := sessionMCPRuntime(t, nil, st, config.Config{},
		harnesstest.Step{Name: "first", Reply: harnesstest.Reply{Text: "ok"}},
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "mcp__weather__forecast", Input: map[string]any{"city": "Oslo"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "done"}})
	s := sessionMCPCreate(t, r, weatherServer(t))
	sessionMCPTurn(t, s, "a", "go")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := r.Open(t.Context(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "b", "again")
	reqs := fake.Requests()
	if len(reqs) != 3 || !requestHas(reqs[0], "mcp__weather__forecast") {
		t.Fatalf("requests = %+v", reqs)
	}
	for _, n := range reqs[1].Tools {
		if strings.HasPrefix(n, "mcp") {
			t.Errorf("an Open without the option has the tool %q", n)
		}
	}
	if got := requestText(reqs[2]); !strings.Contains(got, "no such tool available") {
		t.Errorf("the result of a session tool call after an Open without the option = %s, want no such tool available", got)
	}
}

func rowNoSecretOfASessionServerIsInTheLog(t *testing.T) {
	log := newSyncLog()
	r, _ := sessionMCPRuntime(t, slog.New(log), nil, config.Config{}, replyText("ok"))
	s := sessionMCPCreate(t, r, map[string]config.MCPServerSpec{
		"local":  {Command: []string{filepath.Join(t.TempDir(), "no-such-server"), "--token=" + sessionMCPSecretEnv}, Env: []string{"API_TOKEN=" + sessionMCPSecretEnv}},
		"remote": {URL: "http://127.0.0.1:1/mcp?key=" + sessionMCPSecretHeader, Headers: map[string]string{"Authorization": "Bearer " + sessionMCPSecretHeader}},
	})
	sessionMCPTurn(t, s, "a", "go")
	<-log.parked
	<-log.parked
	var events []string
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, string(b))
		if e.Kind == "turn.ended" {
			break
		}
	}
	for _, secret := range []string{sessionMCPSecretEnv, sessionMCPSecretHeader} {
		if strings.Contains(log.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, log.String())
		}
		if strings.Contains(strings.Join(events, "\n"), secret) {
			t.Errorf("the session log holds %q", secret)
		}
	}
	if !strings.Contains(log.String(), "local") || !strings.Contains(log.String(), "remote") {
		t.Errorf("the log names no session server:\n%s", log.String())
	}
}

func rowASessionServerMayNotTakeTheNameOfARuntimeServer(t *testing.T) {
	r, _ := sessionMCPRuntime(t, nil, nil, config.Config{MCPServers: weatherServer(t)}, replyText("ok"))
	_, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}, harness.WithSessionMCP(weatherServer(t)))
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Create error = %v, want ErrInvalidRequest", err)
	}
	_, err = r.Create(t.Context(), protocol.CreateSession{ID: "s2", Model: "bifrost/gpt-test"}, harness.WithSessionMCP(map[string]config.MCPServerSpec{"a__b": {Command: []string{"x"}}}))
	if !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Create error for a bad name = %v, want ErrInvalidRequest", err)
	}
	if _, err := r.Open(t.Context(), "s9", harness.WithSessionMCP(map[string]config.MCPServerSpec{"x": {}})); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Open error for an empty spec = %v, want ErrInvalidRequest", err)
	}
}

func rowASessionToolReachesTheCLIThroughTheHarnessEndpoint(t *testing.T) {
	dir := t.TempDir()
	logPath, toolLog := filepath.Join(dir, "mcp-config.jsonl"), filepath.Join(dir, "tool.jsonl")
	retries := 0
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), WorkDir: dir, Config: config.Config{RequestMaxRetries: &retries, StreamMaxRetries: &retries,
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath(),
			ExtraArgs: fakeEnvArgs(map[string]string{"FAKE_CLAUDE_MODE": "normal", "FAKE_CLAUDE_MCP_CONFIG_LOG": logPath,
				"FAKE_CLAUDE_CALL_TOOL": "mcp__weather__forecast", "FAKE_CLAUDE_TOOL_LOG": toolLog})}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"}, harness.WithSessionMCP(map[string]config.MCPServerSpec{"weather": {URL: harnesstest.NewMCPServer(t, mcpWeather("")).URL()}}))
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "a", "go")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &f); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	if _, ok := f.MCPServers["harness"]; !ok || len(f.MCPServers) != 2 || f.MCPServers["weather"] == nil {
		t.Errorf("--mcp-config servers = %v, want the harness endpoint and weather", f.MCPServers)
	}
	if got, err := os.ReadFile(toolLog); err != nil || !strings.Contains(string(got), "Oslo: 3C, snow") {
		t.Errorf("tool call response = %s (%v), want the forecast of the session server", got, err)
	}
}

func rowABackendThatOwnsTheLoopAllowsASessionToolByName(t *testing.T) {
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: config.Config{
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath()}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	req := protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet", AllowedTools: []string{"mcp__session__x"}}
	if _, err := r.Create(t.Context(), req); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Create without session servers = %v, want ErrInvalidRequest", err)
	}
	if _, err := r.Create(t.Context(), req, harness.WithSessionMCP(map[string]config.MCPServerSpec{"session": {URL: "http://127.0.0.1:1/s"}})); err != nil {
		t.Errorf("Create with the session server = %v, want nil", err)
	}
}

func rowRuntimeAndSessionServersAnswerInOneTurn(t *testing.T) {
	runtime := map[string]config.MCPServerSpec{"weather": {Command: []string{mcpStubBin(t)}, Env: mcpWeather("").StdioEnv()}}
	session := map[string]config.MCPServerSpec{"docs": {Command: []string{mcpStubBin(t)}, Env: mcpDocs().StdioEnv()}}
	r, fake := sessionMCPRuntime(t, nil, nil, config.Config{MCPServers: runtime},
		harnesstest.Step{Name: "calls", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{
			{ID: "c1", Name: "mcp__weather__forecast", Input: map[string]any{"city": "Oslo"}},
			{ID: "c2", Name: "mcp__docs__search", Input: map[string]any{}},
			{ID: "c3", Name: "mcp", Input: map[string]any{"action": "status"}}}}},
		harnesstest.Step{Name: "done", Reply: harnesstest.Reply{Text: "ok"}})
	s := sessionMCPCreate(t, r, session)
	sessionMCPTurn(t, s, "a", "go")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model requests = %d, want 2", len(reqs))
	}
	got := requestText(reqs[1])
	for _, want := range []string{"Oslo: 3C, snow", "no hits", `"name":"weather"`, `"name":"docs"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the second request lacks %q: %s", want, got)
		}
	}
}

func rowNoSecretOfAConnectedSessionServerIsInTheLogOrAToolError(t *testing.T) {
	log := newSyncLog()
	remote := harnesstest.NewMCPServer(t, mcpWeather(""))
	remote.RequireAuthorization("Bearer " + sessionMCPSecretHeader)
	call := func(id, name string) harnesstest.Step {
		return harnesstest.Step{Name: id, Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: name, Input: map[string]any{"city": "Oslo"}}}}}
	}
	text := func(id string) harnesstest.Step {
		return harnesstest.Step{Name: id, Reply: harnesstest.Reply{Text: "ok"}}
	}
	r, fake := sessionMCPRuntime(t, slog.New(log), nil, config.Config{}, call("c1", "mcp__remote__forecast"), text("t1"),
		call("c2", "mcp__remote__forecast"), text("t2"), call("c3", "mcp__local__forecast"), text("t3"))
	s := sessionMCPCreate(t, r, map[string]config.MCPServerSpec{
		"remote": {URL: remote.URL() + "?key=" + sessionMCPSecretHeader, Headers: map[string]string{"Authorization": "Bearer " + sessionMCPSecretHeader}},
		"local":  {Command: []string{mcpStubBin(t)}, Env: append(mcpWeather("").StdioEnv(), "API_TOKEN="+sessionMCPSecretEnv)},
	})
	sessionMCPTurn(t, s, "a", "one")
	remote.SetAvailable(false)
	sessionMCPTurn(t, s, "b", "two")
	sessionMCPTurn(t, s, "c", "three")
	var all []string
	ended := 0
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, string(b))
		if e.Kind == "turn.ended" {
			if ended++; ended == 3 {
				break
			}
		}
	}
	for _, r := range fake.Requests() {
		all = append(all, requestText(r))
	}
	all = append(all, log.String())
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, "call failed") {
		t.Fatalf("the unavailable server did not fail a call: %s", joined)
	}
	for _, secret := range []string{sessionMCPSecretEnv, sessionMCPSecretHeader} {
		if strings.Contains(joined, secret) {
			t.Errorf("the log, an event, or a request holds %q", secret)
		}
	}
}

type namedTool string

func (n namedTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: string(n), Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (namedTool) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	return protocol.ToolResult{Text: "x"}, nil
}

func sessionMCPCollision(t *testing.T, opts harness.Options) {
	t.Helper()
	fake := harnesstest.NewChat(t, replyText("ok"))
	opts.Store = store.NewMemStore()
	opts.Config.Providers = map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL()}}
	r, err := harness.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	req := protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"}
	if _, err := r.Create(t.Context(), req, harness.WithSessionMCP(weatherServer(t))); !errors.Is(err, harness.ErrInvalidRequest) {
		t.Errorf("Create with session servers = %v, want ErrInvalidRequest", err)
	}
	if _, err := r.Create(t.Context(), protocol.CreateSession{ID: "s2", Model: "bifrost/gpt-test"}); err != nil {
		t.Errorf("Create without session servers = %v, want nil", err)
	}
}

func rowASessionServerMayNotShareANameWithAnEmbedderTool(t *testing.T) {
	sessionMCPCollision(t, harness.Options{Tools: []harness.Tool{namedTool("mcp")}})
}

func rowASessionServerMayNotShareANameWithAPluginTool(t *testing.T) {
	sessionMCPCollision(t, harness.Options{Plugins: []*plugin.Plugin{{Name: "p", Tools: []plugin.Tool{{
		Def:     plugin.ToolDef{Name: "mcp__weather__forecast", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Execute: func(context.Context, *plugin.Client, json.RawMessage) ([]plugin.Part, error) { return nil, nil }}}}}})
}

func rowAModelChangeKeepsASessionToolAllowed(t *testing.T) {
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: config.Config{
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath()}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet", AllowedTools: []string{"mcp__session__x"}},
		harness.WithSessionMCP(map[string]config.MCPServerSpec{"session": {URL: "http://127.0.0.1:1/s"}}))
	if err != nil {
		t.Fatal(err)
	}
	opus := "claude-code/opus"
	if _, err := s.Update(t.Context(), protocol.SettingsPatch{Model: &opus}); err != nil {
		t.Errorf("Update to another model = %v, want nil", err)
	}
}

func rowAnEmbedderToolMayNotTakeTheNameOfTheCLIToolSearch(t *testing.T) {
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Tools: []harness.Tool{namedTool("ToolSearch")}, Config: config.Config{
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath()}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	_, err = r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"})
	if !errors.Is(err, harness.ErrInvalidRequest) || !strings.Contains(err.Error(), "has the name of a built-in tool") {
		t.Errorf("Create = %v, want ErrInvalidRequest: has the name of a built-in tool", err)
	}
}

func rowASkillOfASessionServerStaysHeldAfterReleaseAndOpen(t *testing.T) {
	spec := harnesstest.MCPSpec{Name: "docs", Skills: []harnesstest.MCPSkill{refundsSkill()}}
	servers := map[string]config.MCPServerSpec{"docs": {Command: []string{mcpStubBin(t)}, Env: spec.StdioEnv()}}
	call := func(id, name string, in map[string]any) harnesstest.Step {
		return harnesstest.Step{Name: id, Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: id, Name: name, Input: in}}}}
	}
	text := func(id string) harnesstest.Step {
		return harnesstest.Step{Name: id, Reply: harnesstest.Reply{Text: id}}
	}
	r, fake := sessionMCPRuntime(t, nil, nil, config.Config{},
		call("c1", "load_mcp_skill", map[string]any{"server": "docs", "uri": refundsURI}), text("loaded"),
		call("c2", "read_mcp_resource", map[string]any{"server": "docs", "uri": "skill://acme/billing/refunds/templates/fax.md"}), text("read"))
	s := sessionMCPCreate(t, r, servers)
	sessionMCPTurn(t, s, "a", "load it")
	if err := s.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	s, err := r.Open(t.Context(), "s1", harness.WithSessionMCP(servers))
	if err != nil {
		t.Fatal(err)
	}
	sessionMCPTurn(t, s, "b", "read it")
	reqs := fake.Requests()
	if got := requestText(reqs[len(reqs)-1]); !strings.Contains(got, "is not in the entry of the skill "+refundsURI) {
		t.Errorf("the read of a file that the held entry does not list after Release and Open = %s, want the held-entry refusal", got)
	}
}
