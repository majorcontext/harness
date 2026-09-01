package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// runModelToolAction runs the model tool's Run function directly against s and
// decodes a successful result as modelToolResult. t.Fatal on a tool error.
func runModelToolAction(t *testing.T, s *Session, args string) modelToolResult {
	t.Helper()
	tool, ok := s.tools[modelToolName]
	if !ok {
		t.Fatal("model tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(args))
	if err != nil {
		t.Fatalf("model tool run(%s): %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("model tool result is not text: %#v", parts[0])
	}
	var res modelToolResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("model tool result not valid JSON: %v (%s)", err, text.Text)
	}
	return res
}

// callModelToolExpectError runs the model tool's Run function directly and
// requires a non-nil error, returning that error's message.
func callModelToolExpectError(t *testing.T, s *Session, args string) string {
	t.Helper()
	tool, ok := s.tools[modelToolName]
	if !ok {
		t.Fatal("model tool absent")
	}
	_, err := tool.Run(context.Background(), s, []byte(args))
	if err == nil {
		t.Fatalf("model tool run(%s): want error, got nil", args)
	}
	return err.Error()
}

// newModelToolSession builds a session with the model tool enabled, two
// configured providers ("test", "other"), one alias ("fast" -> test/m2), and
// the current model test/m1.
func newModelToolSession(t *testing.T) *Session {
	t.Helper()
	return NewSession(Config{
		ModelTool: true,
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Providers: provider.Registry{
			"test":  &scriptedProvider{name: "test"},
			"other": &scriptedProvider{name: "other"},
		},
		ModelAliases: map[string]string{"fast": "test/m2"},
	})
}

// runModelToolListAction runs the model tool's "list" action directly
// against s and decodes the result as modelListResult. t.Fatal on a tool
// error.
func runModelToolListAction(t *testing.T, s *Session, args string) modelListResult {
	t.Helper()
	tool, ok := s.tools[modelToolName]
	if !ok {
		t.Fatal("model tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(args))
	if err != nil {
		t.Fatalf("model tool run(%s): %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("model tool result is not text: %#v", parts[0])
	}
	var res modelListResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("model tool result not valid JSON: %v (%s)", err, text.Text)
	}
	return res
}

// TestModelToolList proves the list action reports the same configured
// provider families and aliases modelToolStatus reads — the data a
// delegated caller (e.g. a claude-code-lane agent over the MCP shim) needs
// to pick a family for task's spawn(model:...) override, without a second
// data source that could drift from status's own.
func TestModelToolList(t *testing.T) {
	s := newModelToolSession(t)

	res := runModelToolListAction(t, s, `{"action":"list"}`)
	want := []string{"other", "test"}
	if strings.Join(res.Providers, ",") != strings.Join(want, ",") {
		t.Fatalf("list providers = %v, want %v (sorted)", res.Providers, want)
	}
	if res.Aliases["fast"] != "test/m2" {
		t.Fatalf("list aliases = %+v, want fast->test/m2", res.Aliases)
	}
}

func TestModelToolStatus(t *testing.T) {
	s := newModelToolSession(t)

	res := runModelToolAction(t, s, `{"action":"status"}`)
	if res.Model != "test/m1" {
		t.Fatalf("status model = %q, want test/m1", res.Model)
	}
	if res.Aliases["fast"] != "test/m2" {
		t.Fatalf("status aliases = %+v, want fast->test/m2", res.Aliases)
	}
	want := []string{"other", "test"}
	if strings.Join(res.Providers, ",") != strings.Join(want, ",") {
		t.Fatalf("status providers = %v, want %v (sorted)", res.Providers, want)
	}
}

// TestModelToolSetWithAlias: set with a configured alias resolves the alias and
// actually swaps the session model (via SetModel).
func TestModelToolSetWithAlias(t *testing.T) {
	s := newModelToolSession(t)

	res := runModelToolAction(t, s, `{"action":"set","model":"fast"}`)
	if res.Model != "test/m2" {
		t.Fatalf("set alias result model = %q, want test/m2", res.Model)
	}
	if got := s.Model(); got != (message.ModelRef{Provider: "test", Model: "m2"}) {
		t.Fatalf("s.Model() = %v, want test/m2 (SetModel must have run)", got)
	}
}

// TestModelToolSetWithFullRef: set with a full provider/model ref for a
// configured provider swaps the model.
func TestModelToolSetWithFullRef(t *testing.T) {
	s := newModelToolSession(t)

	res := runModelToolAction(t, s, `{"action":"set","model":"other/big"}`)
	if res.Model != "other/big" {
		t.Fatalf("set full-ref result model = %q, want other/big", res.Model)
	}
	if got := s.Model(); got != (message.ModelRef{Provider: "other", Model: "big"}) {
		t.Fatalf("s.Model() = %v, want other/big", got)
	}
}

// TestModelToolSetUnconfiguredProviderLeavesModel is the named guard for the
// validate branch in runModelTool: a set to an unconfigured provider returns an
// error AND leaves the session model unchanged. Red-verify: weaken the
// s.cfg.Providers.For validate branch (drop the error return) and this test
// fails because s.Model() then becomes ghost/x.
func TestModelToolSetUnconfiguredProviderLeavesModel(t *testing.T) {
	s := newModelToolSession(t)

	msg := callModelToolExpectError(t, s, `{"action":"set","model":"ghost/x"}`)
	if !strings.Contains(msg, "not configured") {
		t.Fatalf("set-unconfigured error = %q, want it to mention \"not configured\"", msg)
	}
	// The error must list the valid choices so the model can recover.
	if !strings.Contains(msg, "fast") || !strings.Contains(msg, "test") {
		t.Fatalf("set-unconfigured error = %q, want it to list aliases and providers", msg)
	}
	if got := s.Model(); got != (message.ModelRef{Provider: "test", Model: "m1"}) {
		t.Fatalf("s.Model() = %v after rejected set, want unchanged test/m1", got)
	}
}

func TestModelToolSetRejectsEmptyAndUnknownAction(t *testing.T) {
	s := newModelToolSession(t)

	if msg := callModelToolExpectError(t, s, `{"action":"set"}`); !strings.Contains(msg, "non-empty") {
		t.Fatalf("set-empty error = %q, want it to mention a non-empty model", msg)
	}
	if got := s.Model(); got != (message.ModelRef{Provider: "test", Model: "m1"}) {
		t.Fatalf("s.Model() = %v after empty set, want unchanged test/m1", got)
	}

	for _, action := range []string{"clear", "bogus", ""} {
		args, _ := json.Marshal(map[string]string{"action": action})
		if msg := callModelToolExpectError(t, s, string(args)); !strings.Contains(msg, "unknown action") {
			t.Fatalf("action %q error = %q, want it to mention unknown action", action, msg)
		}
	}
}

// TestModelToolSetSurvivesReload drives a model swap through the REAL tool Run
// (not a hand-built log), then reloads via LoadSession and asserts the swapped
// model is restored — proving the tool's SetModel call persists the durable
// recModel record the resume path reads. Verification drives the production
// path: the tool runs, and LoadSession is the resume entry point.
func TestModelToolSetSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	cfg := Config{
		ModelTool:  true,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	}
	s := NewSession(cfg)

	// A first turn so the session log exists (persistModel is a no-op until
	// the log is started — see store.go).
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	// Swap via the tool's own Run, the production path.
	res := runModelToolAction(t, s, `{"action":"set","model":"test/m2"}`)
	if res.Model != "test/m2" {
		t.Fatalf("tool set result model = %q, want test/m2", res.Model)
	}
	if err := s.PersistErr(); err != nil {
		t.Fatalf("PersistErr = %v", err)
	}

	loaded, err := LoadSession(cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Model(); got != (message.ModelRef{Provider: "test", Model: "m2"}) {
		t.Fatalf("reloaded model = %v, want test/m2 (tool-driven set must survive LoadSession)", got)
	}
}

func TestModelToolAbsentWhenDisabled(t *testing.T) {
	off := NewSession(Config{})
	if _, ok := off.tools[modelToolName]; ok {
		t.Fatal("model tool present with Config.ModelTool false, want absent")
	}
	for _, d := range off.toolDefs(context.Background()) {
		if d.Name == modelToolName {
			t.Fatal("model tool advertised in toolDefs with Config.ModelTool false")
		}
	}

	on := NewSession(Config{ModelTool: true})
	if _, ok := on.tools[modelToolName]; !ok {
		t.Fatal("model tool absent with Config.ModelTool true, want present")
	}
}

// TestSetModelEmitsEventOnChange and its no-op sibling guard the
// EventModelChanged emit in SetModel. Red-verify the no-op guard: remove the
// `if ref == s.model { return }` early return and TestSetModelNoOpEmitsNothing
// fails (a set to the current model then emits an event).
func TestSetModelEmitsEventOnChange(t *testing.T) {
	var got []Event
	s := NewSession(Config{
		Model:   message.ModelRef{Provider: "test", Model: "m1"},
		OnEvent: func(ev Event) { got = append(got, ev) },
	})

	s.SetModel(message.ModelRef{Provider: "test", Model: "m2"})

	var changes []Event
	for _, ev := range got {
		if ev.Type == EventModelChanged {
			changes = append(changes, ev)
		}
	}
	if len(changes) != 1 {
		t.Fatalf("EventModelChanged count = %d, want exactly 1: %+v", len(changes), got)
	}
	if changes[0].Model != (message.ModelRef{Provider: "test", Model: "m2"}) {
		t.Fatalf("EventModelChanged model = %v, want test/m2", changes[0].Model)
	}
}

func TestSetModelNoOpEmitsNothing(t *testing.T) {
	var got []Event
	m1 := message.ModelRef{Provider: "test", Model: "m1"}
	s := NewSession(Config{
		Model:   m1,
		OnEvent: func(ev Event) { got = append(got, ev) },
	})

	s.SetModel(m1) // same as current: a no-op.

	for _, ev := range got {
		if ev.Type == EventModelChanged {
			t.Fatalf("EventModelChanged emitted on a no-op SetModel: %+v", got)
		}
	}
}
