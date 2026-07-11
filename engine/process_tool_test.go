package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/provider"
)

func TestProcessToolAbsentWhenNoProcessesConfigured(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, ok := s.tools[processToolName]; ok {
		t.Fatal("process tool present with nil Config.Processes, want absent")
	}
	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	for _, td := range prov.requests[0].Tools {
		if td.Name == processToolName {
			t.Fatal("process tool advertised to the provider with nil Config.Processes")
		}
	}
}

func TestProcessToolPresentAndDescribesConfiguredRoster(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"pnpm", "dev"}, Dir: "apps/app"},
	})
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	tool, ok := s.tools[processToolName]
	if !ok {
		t.Fatal("process tool absent with Config.Processes set")
	}
	if !strings.Contains(tool.Def.Description, "dev: pnpm dev (dir: apps/app)") {
		t.Errorf("description = %q, want it to list the configured roster", tool.Def.Description)
	}
}

func TestProcessToolDescriptionStableAcrossRuntimeDeclare(t *testing.T) {
	dir := t.TempDir()
	mgr := process.NewManager(dir, map[string]process.Def{
		"dev": {Command: []string{"pnpm", "dev"}},
	})
	tool := processTool(mgr)
	before := tool.Def.Description

	if err := mgr.Declare("adhoc", process.Def{Command: []string{"sh", "-c", "true"}}); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	// Description was computed once at Tool-build time; a later runtime
	// declare must never change it (cache safety — see the package doc).
	if tool.Def.Description != before {
		t.Fatal("process tool description changed after a runtime declare")
	}
	if strings.Contains(tool.Def.Description, "adhoc") {
		t.Fatal("stable description must not include a runtime-declared process")
	}
}

func runProcessAction(t *testing.T, s *Session, args string) processResult {
	t.Helper()
	tool := s.tools[processToolName]
	parts, err := tool.Run(context.Background(), s, json.RawMessage(args))
	if err != nil {
		t.Fatalf("process tool run(%s): %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("process tool result is not text: %#v", parts[0])
	}
	var res processResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("process tool result not valid JSON: %v (%s)", err, text.Text)
	}
	return res
}

func newProcessSession(t *testing.T, dir string, defs map[string]process.Def) (*Session, *process.Manager) {
	t.Helper()
	mgr := process.NewManager(dir, defs)
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:   dir,
		Processes: mgr,
	})
	return s, mgr
}

func TestProcessToolStartStatusStopViaTool(t *testing.T) {
	dir := t.TempDir()
	s, _ := newProcessSession(t, dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", `echo "Ready in 5ms"; sleep 100`}, ReadyRegex: "Ready in .*ms", ReadyTimeout: 5 * time.Second},
	})

	start := runProcessAction(t, s, `{"action":"start","name":"dev"}`)
	if start.State != string(process.StateReady) || !start.Ready {
		t.Fatalf("start result = %+v, want ready", start)
	}
	if start.Log == "" {
		t.Errorf("start result missing log path")
	}

	status := runProcessAction(t, s, `{"action":"status","name":"dev"}`)
	if status.State != string(process.StateReady) {
		t.Fatalf("status result = %+v, want ready", status)
	}

	logs := runProcessAction(t, s, `{"action":"logs","name":"dev","tail":10}`)
	if !strings.Contains(logs.Logs, "Ready in 5ms") {
		t.Errorf("logs = %q, want the ready line", logs.Logs)
	}

	stop := runProcessAction(t, s, `{"action":"stop","name":"dev"}`)
	if stop.State != string(process.StateStopped) {
		t.Fatalf("stop result = %+v, want stopped", stop)
	}
}

func TestProcessToolListDeclareUndeclare(t *testing.T) {
	dir := t.TempDir()
	s, _ := newProcessSession(t, dir, map[string]process.Def{
		"dev": {Command: []string{"sh", "-c", "true"}},
	})

	tool := s.tools[processToolName]
	parts, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var infos []process.Info
	if err := json.Unmarshal([]byte(parts[0].(*message.Text).Text), &infos); err != nil {
		t.Fatalf("list result not valid JSON array: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "dev" || infos[0].Origin != process.OriginConfig {
		t.Fatalf("list = %+v, want [dev(config)]", infos)
	}

	if _, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"declare","name":"adhoc","command":["sh","-c","true"]}`)); err != nil {
		t.Fatalf("declare: %v", err)
	}
	if _, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"declare","name":"dev","command":["x"]}`)); err == nil {
		t.Fatal("declare over config-origin name via tool: want error")
	}
	if _, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"declare","name":"bad","command":[],"ready_regex":"("}`)); err == nil {
		t.Fatal("declare with empty command: want error")
	}
	if _, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"undeclare","name":"adhoc"}`)); err != nil {
		t.Fatalf("undeclare: %v", err)
	}
	if _, err := tool.Run(context.Background(), s, json.RawMessage(`{"action":"undeclare","name":"dev"}`)); err == nil {
		t.Fatal("undeclare config-origin name via tool: want error")
	}
}
