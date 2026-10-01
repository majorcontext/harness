package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/provider"
)

func lookupTool() Tool {
	return Tool{
		Def: provider.ToolDef{Name: "lookup", Description: "look up", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Run: func(context.Context, *Session, json.RawMessage) (message.Parts, error) {
			return message.Parts{&message.Text{Text: "ok"}}, nil
		},
	}
}

func allowlistSession(allowed []string, mutate func(*Config)) (*Session, *scriptedProvider, *fakeHooks) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	hooks := &fakeHooks{}
	cfg := Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		Hooks:        hooks,
		Tools:        []Tool{lookupTool()},
		AllowedTools: allowed,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewSession(cfg), prov, hooks
}

func sentToolNames(t *testing.T, prov *scriptedProvider) []string {
	t.Helper()
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(prov.requests))
	}
	names := []string{}
	for _, d := range prov.requests[0].Tools {
		names = append(names, d.Name)
	}
	return names
}

func TestAllowedToolsKeepsOnlyNamed(t *testing.T) {
	s, prov, _ := allowlistSession([]string{"read_file", "glob"}, nil)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got, want := sentToolNames(t, prov), []string{"glob", "read_file"}; !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

func TestAllowedToolsNamesCustomTool(t *testing.T) {
	s, prov, _ := allowlistSession([]string{"lookup"}, nil)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got, want := sentToolNames(t, prov), []string{"lookup"}; !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

func TestAllowedToolsEmptyMeansNone(t *testing.T) {
	s, prov, _ := allowlistSession([]string{}, nil)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := sentToolNames(t, prov); len(got) != 0 {
		t.Errorf("tools = %v, want none", got)
	}

	s, prov, _ = allowlistSession(nil, nil)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := sentToolNames(t, prov); !slices.Contains(got, "bash") || !slices.Contains(got, "lookup") {
		t.Errorf("nil allowlist tools = %v, want bash and lookup present", got)
	}
}

func TestAllowedToolsUnknownNameFailsAtStart(t *testing.T) {
	s, prov, hooks := allowlistSession([]string{"read_file", "nope"}, nil)
	cfgErr := s.ConfigErr()
	if cfgErr == nil || !strings.Contains(cfgErr.Error(), `unknown tool "nope"`) {
		t.Fatalf("ConfigErr = %v, want unknown tool \"nope\"", cfgErr)
	}
	_, err := s.Prompt(context.Background(), "go")
	if err == nil || !strings.Contains(err.Error(), `unknown tool "nope"`) {
		t.Fatalf("Prompt error = %v, want unknown tool \"nope\"", err)
	}
	if len(prov.requests) != 0 {
		t.Errorf("provider received a request, want none")
	}
	var found bool
	for _, ev := range hooks.events {
		if ev.Type != plugin.EventSessionError {
			continue
		}
		var props plugin.SessionErrorProperties
		if err := json.Unmarshal(ev.Properties, &props); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(props.Message, `unknown tool "nope"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no session.error event naming \"nope\": %+v", hooks.events)
	}
}

func TestAllowedToolsConditionalNameAbsent(t *testing.T) {
	s, _, _ := allowlistSession([]string{"process"}, nil)
	if err := s.ConfigErr(); err == nil || !strings.Contains(err.Error(), `unknown tool "process"`) {
		t.Fatalf("ConfigErr = %v, want unknown tool \"process\"", err)
	}
}

func TestAllowedToolsRejectsMCP(t *testing.T) {
	s, _, _ := allowlistSession([]string{}, func(c *Config) { c.MCP = configOnlyMCPRegistry{} })
	err := s.ConfigErr()
	if err == nil || !strings.Contains(err.Error(), "MCP") {
		t.Fatalf("ConfigErr = %v, want an MCP error", err)
	}
}

func TestAllowedToolsSurviveRootAdoption(t *testing.T) {
	s, prov, _ := allowlistSession([]string{}, nil)
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptRoot(s); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := sentToolNames(t, prov); len(got) != 0 {
		t.Errorf("tools after adoption = %v, want none", got)
	}
}

func TestAllowedToolsAdoptionKeepsNamedTool(t *testing.T) {
	s, prov, _ := allowlistSession([]string{"lookup"}, nil)
	mgr := NewSessionManager(context.Background(), 0, 0)
	if err := mgr.AdoptRoot(s); err != nil {
		t.Fatalf("AdoptRoot: %v", err)
	}
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got, want := sentToolNames(t, prov), []string{"lookup"}; !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

func pluginHooks() *fakeHooks {
	return &fakeHooks{pluginTool: &plugin.ToolExecuteResponse{
		Output: message.Parts{&message.Text{Text: "plugin ran"}},
	}}
}

func TestAllowedToolsFilterPluginTools(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"nil sends plugin tool", nil, []string{"upload_file"}},
		{"omitting allowlist drops it", []string{}, []string{}},
		{"naming it keeps it", []string{"upload_file"}, []string{"upload_file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, prov, _ := allowlistSession(tc.allowed, func(c *Config) {
				c.Hooks = pluginHooks()
				c.Tools = nil
			})
			if err := s.ConfigErr(); err != nil {
				t.Fatalf("ConfigErr: %v", err)
			}
			if _, err := s.Prompt(context.Background(), "go"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			got := sentToolNames(t, prov)
			if tc.allowed == nil {
				if !slices.Contains(got, "upload_file") {
					t.Errorf("tools = %v, want upload_file present", got)
				}
				return
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("tools = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAllowedToolsRefusePluginToolCall(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "upload_file", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers:    provider.Registry{"test": prov},
		Model:        message.ModelRef{Provider: "test", Model: "m1"},
		Hooks:        pluginHooks(),
		AllowedTools: []string{},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	var result string
	for _, m := range s.History() {
		for _, p := range m.Parts {
			if tr, ok := p.(*message.ToolResult); ok {
				result = tr.SafeContent().Text()
			}
		}
	}
	if !strings.Contains(result, `unknown tool "upload_file"`) || strings.Contains(result, "plugin ran") {
		t.Errorf("tool result = %q, want the plugin call refused as unknown", result)
	}
}

func TestAllowedToolsLoadSessionDoesNotReaddReadToolResult(t *testing.T) {
	dir := t.TempDir()
	prov := oneToolTurnProvider("bigtool")
	cfg := retainCfg(dir, prov, 512, 0)
	cfg.Tools = []Tool{bigOutputTool("bigtool", linesText(3000))}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	resume := func(allowed []string) *Session {
		t.Helper()
		s2, err := LoadSession(Config{
			Providers:             provider.Registry{prov.name: prov},
			Model:                 message.ModelRef{Provider: prov.name, Model: "m1"},
			SessionDir:            dir,
			ToolResultInlineBytes: 0,
			AllowedTools:          allowed,
		}, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s2
	}
	if _, ok := resume([]string{}).tools[readToolResultToolName]; ok {
		t.Error("read_tool_result re-added despite an empty allowlist")
	}
	if _, ok := resume(nil).tools[readToolResultToolName]; !ok {
		t.Error("read_tool_result missing with a nil allowlist")
	}
}

func TestAllowedToolsRefusedSessionSendsNoPrewarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartupPrewarmProvider("prewarm")
		cfg := startupConfig(p)
		cfg.AllowedTools = []string{"nope"}
		s := NewSession(cfg)
		synctest.Wait()
		if s.ConfigErr() == nil {
			t.Fatal("ConfigErr is nil")
		}
		select {
		case req := <-p.prewarmRequests:
			t.Fatalf("refused session sent a prewarm request with tools %v", req.Tools)
		default:
		}
		if _, err := s.Prompt(context.Background(), "go"); err == nil {
			t.Fatal("Prompt succeeded")
		}
		if n := len(p.streams()); n != 0 {
			t.Errorf("provider streams = %d, want none", n)
		}
	})
}
