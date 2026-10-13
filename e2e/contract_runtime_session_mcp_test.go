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
		{"a_session_server_reaches_a_backend_that_owns_mcp", rowASessionServerReachesABackendThatOwnsMCP},
		{"a_session_server_may_not_take_the_name_of_a_runtime_server", rowASessionServerMayNotTakeTheNameOfARuntimeServer},
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
	r, fake := sessionMCPRuntime(t, nil, st, config.Config{}, replyText("ok"))
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
	if len(reqs) != 2 || !requestHas(reqs[0], "mcp__weather__forecast") {
		t.Fatalf("requests = %+v", reqs)
	}
	for _, n := range reqs[1].Tools {
		if strings.HasPrefix(n, "mcp") {
			t.Errorf("an Open without the option has the tool %q", n)
		}
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

func rowASessionServerReachesABackendThatOwnsMCP(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mcp-config.jsonl")
	retries := 0
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: config.Config{RequestMaxRetries: &retries, StreamMaxRetries: &retries,
		MCPServers: map[string]config.MCPServerSpec{"runtime": {URL: "http://127.0.0.1:1/runtime"}},
		Providers: map[string]config.Provider{"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: fakeClaudePath(),
			ExtraArgs: fakeEnvArgs(map[string]string{"FAKE_CLAUDE_MODE": "normal", "FAKE_CLAUDE_MCP_CONFIG_LOG": logPath})}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet"},
		harness.WithSessionMCP(map[string]config.MCPServerSpec{"session": {URL: "http://127.0.0.1:1/session"}}))
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
	if f.MCPServers["runtime"]["url"] != "http://127.0.0.1:1/runtime" || f.MCPServers["session"]["url"] != "http://127.0.0.1:1/session" {
		t.Errorf("--mcp-config servers = %v, want the runtime server and the session server", f.MCPServers)
	}
}
