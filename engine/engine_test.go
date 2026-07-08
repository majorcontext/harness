package engine

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/andybons/harness/message"
	"github.com/andybons/harness/plugin"
	"github.com/andybons/harness/provider"
)

// scriptedProvider returns one pre-built stream per call.
type scriptedProvider struct {
	name     string
	turns    [][]provider.Event
	call     int
	requests []*provider.Request
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	if p.call >= len(p.turns) {
		return nil, io.ErrUnexpectedEOF
	}
	events := p.turns[p.call]
	p.call++
	return &scriptedStream{events: events}, nil
}

type scriptedStream struct {
	events []provider.Event
	i      int
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *scriptedStream) Close() error { return nil }

func asstTurn(stop provider.StopReason, parts ...message.Part) []provider.Event {
	msg := &message.Message{ID: "msg_a", Role: message.RoleAssistant, Parts: parts}
	return []provider.Event{{Type: provider.EventDone, Message: msg, StopReason: stop}}
}

func toolCall(id, name, args string) *message.ToolCall {
	return &message.ToolCall{CallID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestPromptToolLoop(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse,
			&message.Text{Text: "running"},
			toolCall("tc1", "bash", `{"command":"echo hello-from-bash"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}

	var events []Event
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base system"},
		OnEvent:   func(ev Event) { events = append(events, ev) },
	})

	final, err := s.Prompt(context.Background(), "run echo")
	if err != nil {
		t.Fatal(err)
	}
	if final.Parts.Text() != "done" {
		t.Errorf("final = %q", final.Parts.Text())
	}

	// History: user, assistant(tool call), tool(result), assistant.
	h := s.History()
	if len(h) != 4 {
		t.Fatalf("history len = %d: %+v", len(h), h)
	}
	if h[0].Role != message.RoleUser || h[1].Role != message.RoleAssistant ||
		h[2].Role != message.RoleTool || h[3].Role != message.RoleAssistant {
		t.Errorf("roles = %s %s %s %s", h[0].Role, h[1].Role, h[2].Role, h[3].Role)
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok || tr.CallID != "tc1" || tr.IsError {
		t.Fatalf("tool result = %+v", h[2].Parts[0])
	}
	if !strings.Contains(tr.Content.Text(), "hello-from-bash") {
		t.Errorf("bash output = %q", tr.Content.Text())
	}

	// The second request must include the full history.
	if len(prov.requests) != 2 {
		t.Fatalf("requests = %d", len(prov.requests))
	}
	if len(prov.requests[1].Messages) != 3 {
		t.Errorf("second request history = %d messages", len(prov.requests[1].Messages))
	}
	if prov.requests[0].System[0] != "base system" {
		t.Errorf("system = %v", prov.requests[0].System)
	}

	// Events include tool.start and tool.end.
	var toolStarts, toolEnds int
	for _, ev := range events {
		switch ev.Type {
		case EventToolStart:
			toolStarts++
		case EventToolEnd:
			toolEnds++
		}
	}
	if toolStarts != 1 || toolEnds != 1 {
		t.Errorf("tool events = %d/%d", toolStarts, toolEnds)
	}
}

func TestModelSwapMidSession(t *testing.T) {
	provA := &scriptedProvider{name: "prov-a", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "from a"}),
	}}
	provB := &scriptedProvider{name: "prov-b", turns: [][]provider.Event{
		asstTurn(provider.StopEndTurn, &message.Text{Text: "from b"}),
	}}

	s := NewSession(Config{
		Providers: provider.Registry{"prov-a": provA, "prov-b": provB},
		Model:     message.ModelRef{Provider: "prov-a", Model: "m1"},
	})

	if _, err := s.Prompt(context.Background(), "one"); err != nil {
		t.Fatal(err)
	}
	s.SetModel(message.ModelRef{Provider: "prov-b", Model: "m2"})
	if _, err := s.Prompt(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}

	if len(provA.requests) != 1 || len(provB.requests) != 1 {
		t.Fatalf("requests split = %d/%d", len(provA.requests), len(provB.requests))
	}
	// The swapped-to provider gets the full history, including provider A's
	// assistant turn.
	if len(provB.requests[0].Messages) != 3 {
		t.Errorf("prov-b history = %d messages", len(provB.requests[0].Messages))
	}
}

// fakeHooks implements Hooks for tests.
type fakeHooks struct {
	deny        string
	shellEnvVar map[string]string
	segments    []string
	afterSuffix string

	shellEnvCalls []string
	pluginTool    *plugin.ToolExecuteResponse
	events        []plugin.Event
}

func (f *fakeHooks) ChatParams(_ context.Context, req *plugin.ChatParamsRequest) plugin.ChatParams {
	return req.Params
}

func (f *fakeHooks) SystemTransform(_ context.Context, _ *plugin.SystemTransformRequest) []string {
	return f.segments
}

func (f *fakeHooks) ShellEnv(_ context.Context, req *plugin.ShellEnvRequest) map[string]string {
	f.shellEnvCalls = append(f.shellEnvCalls, req.Command)
	return f.shellEnvVar
}

func (f *fakeHooks) ToolExecuteBefore(_ context.Context, req *plugin.ToolExecuteBeforeRequest) (json.RawMessage, string) {
	return nil, f.deny
}

func (f *fakeHooks) ToolExecuteAfter(_ context.Context, req *plugin.ToolExecuteAfterRequest) message.Parts {
	if f.afterSuffix == "" {
		return req.Output
	}
	return append(req.Output, &message.Text{Text: f.afterSuffix})
}

func (f *fakeHooks) ExecuteTool(_ context.Context, _ *plugin.ToolExecuteRequest) (*plugin.ToolExecuteResponse, error) {
	return f.pluginTool, nil
}

func (f *fakeHooks) Emit(events []plugin.Event) { f.events = append(f.events, events...) }

func (f *fakeHooks) Tools() []plugin.ToolDef {
	if f.pluginTool == nil {
		return nil
	}
	return []plugin.ToolDef{{Name: "upload_file", Description: "d", InputSchema: json.RawMessage(`{}`)}}
}

func TestHooksIntegration(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"echo x"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	hooks := &fakeHooks{
		shellEnvVar: map[string]string{"GH_TOKEN": "tok"},
		segments:    []string{"injected rules"},
		afterSuffix: "[annotated]",
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		System:    []string{"base"},
		Hooks:     hooks,
	})

	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	// system.transform segments appended after base.
	if sys := prov.requests[0].System; len(sys) != 2 || sys[1] != "injected rules" {
		t.Errorf("system = %v", sys)
	}
	// shell.env consulted for the bash command.
	if len(hooks.shellEnvCalls) != 1 || hooks.shellEnvCalls[0] != "echo x" {
		t.Errorf("shellEnv calls = %v", hooks.shellEnvCalls)
	}
	// tool.execute.after annotation landed in the result.
	tr := s.History()[2].Parts[0].(*message.ToolResult)
	if !strings.Contains(tr.Content.Text(), "[annotated]") {
		t.Errorf("result = %q", tr.Content.Text())
	}
	// session.status events emitted (busy + idle).
	if len(hooks.events) != 2 || hooks.events[0].Type != plugin.EventSessionStatus {
		t.Errorf("events = %+v", hooks.events)
	}
}

func TestToolDeny(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bash", `{"command":"rm -rf /"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "understood"}),
	}}
	hooks := &fakeHooks{deny: "blocked by policy"}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Hooks:     hooks,
	})

	final, err := s.Prompt(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if final.Parts.Text() != "understood" {
		t.Errorf("final = %q", final.Parts.Text())
	}
	tr := s.History()[2].Parts[0].(*message.ToolResult)
	if !tr.IsError || tr.Content.Text() != "blocked by policy" {
		t.Errorf("denied result = %+v", tr)
	}
}

func TestPluginToolDispatch(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "upload_file", `{"file_path":"/tmp/x.png"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	hooks := &fakeHooks{
		pluginTool: &plugin.ToolExecuteResponse{
			Output: message.Parts{&message.Text{Text: "uploaded to https://example.com"}},
		},
	}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Hooks:     hooks,
	})

	if _, err := s.Prompt(context.Background(), "upload it"); err != nil {
		t.Fatal(err)
	}

	// Plugin tool def offered to the model alongside builtins.
	var names []string
	for _, d := range prov.requests[0].Tools {
		names = append(names, d.Name)
	}
	if !contains(names, "bash") || !contains(names, "upload_file") {
		t.Errorf("tool defs = %v", names)
	}

	tr := s.History()[2].Parts[0].(*message.ToolResult)
	if !strings.Contains(tr.Content.Text(), "uploaded to") {
		t.Errorf("result = %q", tr.Content.Text())
	}
}

func TestUnknownToolIsErrorResult(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "nope", `{}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "ok"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	tr := s.History()[2].Parts[0].(*message.ToolResult)
	if !tr.IsError || !strings.Contains(tr.Content.Text(), "unknown tool") {
		t.Errorf("result = %+v", tr)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
