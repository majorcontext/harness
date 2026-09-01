package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/process"
)

// TestRunToolDispatchesToRegisteredTool proves RunTool actually drives a
// real native tool (process) rather than merely validating its arguments:
// a "start" call through RunTool must leave the process running in the
// SAME Manager a native-loop call would have used.
func TestRunToolDispatchesToRegisteredTool(t *testing.T) {
	dir := t.TempDir()
	s, mgr := newProcessSession(t, dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", `echo "Ready in 5ms"; sleep 100`}, ReadyRegex: "Ready in .*ms"},
	})

	parts, err := s.RunTool(context.Background(), processToolName, json.RawMessage(`{"action":"start","name":"dev"}`))
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("RunTool result is not text: %#v", parts[0])
	}
	var res processResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("RunTool result not valid JSON: %v (%s)", err, text.Text)
	}
	if res.State != string(process.StateReady) {
		t.Fatalf("RunTool start result = %+v, want ready", res)
	}

	// The SAME Manager sees it running — RunTool went through the real
	// process tool, not a stand-in.
	st, err := mgr.Status("dev")
	if err != nil {
		t.Fatalf("mgr.Status: %v", err)
	}
	if st.State != process.StateReady {
		t.Fatalf("mgr.Status(dev) = %+v, want ready", st)
	}
}

// TestRunToolReusesHookPath proves RunTool does not bypass runToolCall's
// hook integration: ToolExecuteBefore/ToolExecuteAfter both fire, and the
// tool.execute.start/end plugin events are emitted, exactly like a
// native-loop tool call (see TestHooksIntegration for the native-loop
// equivalent this mirrors).
func TestRunToolReusesHookPath(t *testing.T) {
	dir := t.TempDir()
	hooks := &fakeHooks{afterSuffix: "[annotated]"}
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	s := NewSession(Config{
		WorkDir:   dir,
		Processes: mgr,
		Hooks:     hooks,
	})

	parts, err := s.RunTool(context.Background(), processToolName, json.RawMessage(`{"action":"status","name":"dev"}`))
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	text, ok := parts[len(parts)-1].(*message.Text)
	if !ok || text.Text != "[annotated]" {
		t.Fatalf("RunTool result = %#v, want ToolExecuteAfter's own annotation appended", parts)
	}

	wantTypes := []string{plugin.EventToolExecuteStart, plugin.EventToolExecuteEnd}
	if len(hooks.events) != len(wantTypes) {
		t.Fatalf("hook events = %+v, want types %v", hooks.events, wantTypes)
	}
	for i, want := range wantTypes {
		if hooks.events[i].Type != want {
			t.Errorf("events[%d].Type = %q, want %q", i, hooks.events[i].Type, want)
		}
	}
}

// TestRunToolDeniedByToolExecuteBeforeHook proves a ToolExecuteBefore hook
// denial short-circuits RunTool exactly like it does a native-loop call:
// the process never actually starts, and the denial reason comes back as
// RunTool's own error text.
func TestRunToolDeniedByToolExecuteBeforeHook(t *testing.T) {
	dir := t.TempDir()
	hooks := &fakeHooks{deny: "blocked by policy"}
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "sleep 100"}},
	})
	s := NewSession(Config{
		WorkDir:   dir,
		Processes: mgr,
		Hooks:     hooks,
	})

	_, err := s.RunTool(context.Background(), processToolName, json.RawMessage(`{"action":"start","name":"dev"}`))
	if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
		t.Fatalf("RunTool err = %v, want it to carry the hook's denial reason", err)
	}
	if st, statusErr := mgr.Status("dev"); statusErr == nil && st.State != "" {
		t.Errorf("mgr.Status(dev) = %+v, want the process never started (denied before dispatch)", st)
	}
}

// TestRunToolUnknownToolReturnsCleanError proves RunTool never panics on an
// unrecognized name — it returns the same "unknown tool" text executeTool
// already produces for a native-loop call, wrapped as a plain error.
func TestRunToolUnknownToolReturnsCleanError(t *testing.T) {
	s := NewSession(Config{})
	_, err := s.RunTool(context.Background(), "does_not_exist", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("RunTool err = nil, want an error for an unrecognized tool name")
	}
	if !strings.Contains(err.Error(), "does_not_exist") {
		t.Errorf("RunTool err = %v, want it to name the unrecognized tool", err)
	}
}

// TestToolDefReturnsRegisteredToolSchema proves ToolDef hands back the
// SAME Description/InputSchema the native loop advertises to a provider —
// server/mcp_history.go relies on this to avoid hand-duplicating the
// process tool's schema for its MCP tools/list entry.
func TestToolDefReturnsRegisteredToolSchema(t *testing.T) {
	dir := t.TempDir()
	s, _ := newProcessSession(t, dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})
	def, ok := s.ToolDef(processToolName)
	if !ok {
		t.Fatal("ToolDef(process) ok = false, want true with Config.Processes set")
	}
	want := s.tools[processToolName].Def
	if def.Name != want.Name || def.Description != want.Description || string(def.InputSchema) != string(want.InputSchema) {
		t.Errorf("ToolDef(process) = %+v, want it to match the registered tool's own Def exactly", def)
	}
}

// TestToolDefUnknownToolNotOK proves ToolDef reports ok=false, not a
// zero-valued lie, for a tool this session never registered (e.g.
// "process" with Config.Processes left nil).
func TestToolDefUnknownToolNotOK(t *testing.T) {
	s := NewSession(Config{})
	if _, ok := s.ToolDef(processToolName); ok {
		t.Error("ToolDef(process) ok = true with no Config.Processes configured, want false")
	}
}
