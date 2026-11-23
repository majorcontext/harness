package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/provider"
)

func changeRequest(tools []provider.ToolDef, msgs ...message.Message) *provider.Request {
	return &provider.Request{Model: modelref.ModelRef{Provider: "anthropic", Model: "claude-fable-5-2"}, MaxTokens: 100, Tools: tools, Messages: msgs}
}

func text(role message.Role, s string) message.Message {
	return message.Message{Role: role, Parts: message.Parts{&message.Text{Text: s}}}
}

func change(removed, added []string) message.Message {
	return message.Message{Role: message.RoleUser, Parts: message.Parts{&message.ToolChange{Removed: removed, Added: added}}}
}

func wireJSON(t *testing.T, req *provider.Request) (string, *apiRequest) {
	t.Helper()
	wire, err := transcodeRequest(req, CacheTTL5m, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), wire
}

func TestToolChangeRendersRemovalsThenAdditionsAsOneSystemMessage(t *testing.T) {
	got, wire := wireJSON(t, changeRequest(nil, text(message.RoleUser, "go"), change([]string{"a", "b"}, []string{"c"})))
	want := `[{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral"}}]},` +
		`{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"a"}},{"type":"tool_removal","tool":{"type":"tool_reference","name":"b"}},{"type":"tool_addition","tool":{"type":"tool_reference","name":"c"}}]}]`
	msgs, _ := json.Marshal(wire.Messages)
	if string(msgs) != want {
		t.Errorf("messages = %s\nwant %s\nrequest %s", msgs, want, got)
	}
	if !wire.toolChange {
		t.Error("a tool-change message needs the beta header")
	}
}

func TestToolChangeMovesBeforeTheNextAssistantMessageAfterUserMessages(t *testing.T) {
	_, wire := wireJSON(t, changeRequest(nil,
		text(message.RoleUser, "go"), change([]string{"a"}, nil), text(message.RoleUser, "context"), text(message.RoleAssistant, "ok"), text(message.RoleUser, "more")))
	var roles []string
	for _, m := range wire.Messages {
		roles = append(roles, m.Role)
	}
	if got, want := len(wire.Messages[0].Content), 2; got != want {
		t.Errorf("first message holds %d blocks, want %d: the context merges into it", got, want)
	}
	if want := []string{"user", "system", "assistant", "user"}; len(roles) != len(want) || roles[1] != "system" || roles[2] != "assistant" {
		t.Errorf("roles = %v, want %v", roles, want)
	}
}

func TestToolChangeAfterAToolResultKeepsTheMarkerOnTheLastUserBlock(t *testing.T) {
	result := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.ToolResult{CallID: "c1", Content: message.Parts{&message.Text{Text: "ok"}}}}}
	call := message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "c1", Name: "x", Arguments: json.RawMessage(`{}`)}}}
	_, wire := wireJSON(t, changeRequest(nil, text(message.RoleUser, "go"), call, result, change(nil, []string{"a"})))
	last := wire.Messages[len(wire.Messages)-1]
	if last.Role != "system" || last.Content[0].Type != "tool_addition" {
		t.Fatalf("last message = %+v, want the tool-change message last", last)
	}
	user := wire.Messages[len(wire.Messages)-2]
	if user.Role != "user" || user.Content[len(user.Content)-1].CacheControl == nil {
		t.Errorf("the cache marker is not on the last block of the last user message: %+v", user)
	}
}

func TestDeferredToolCarriesDeferLoadingAndTheBeta(t *testing.T) {
	got, wire := wireJSON(t, changeRequest([]provider.ToolDef{{Name: "code"}, {Name: "bash", Deferred: true}}, text(message.RoleUser, "go")))
	want := `[{"name":"code"},{"name":"bash","defer_loading":true}]`
	tools, _ := json.Marshal(wire.Tools)
	if string(tools) != want || !wire.toolChange {
		t.Errorf("tools = %s, toolChange = %v, want %s and true\n%s", tools, wire.toolChange, want, got)
	}
}

func TestRequestWithNoToolChangeNeedsNoBeta(t *testing.T) {
	_, wire := wireJSON(t, changeRequest([]provider.ToolDef{{Name: "code"}}, text(message.RoleUser, "go")))
	if wire.toolChange {
		t.Error("a request with no deferred tool and no tool-change message sent the beta")
	}
}

func TestToolChangeRequestCarriesTheBetaWithNoDeferredTool(t *testing.T) {
	req := changeRequest([]provider.ToolDef{{Name: "code"}}, text(message.RoleUser, "go"))
	req.ToolChange = true
	if _, wire := wireJSON(t, req); !wire.toolChange {
		t.Error("a request that may change tools sent no beta")
	}
}
