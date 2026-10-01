package engine

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

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
