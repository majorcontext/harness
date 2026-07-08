package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const testFamily = "openrouter"

func mustTranscode(t *testing.T, req *provider.Request) *apiRequest {
	t.Helper()
	out, err := transcodeRequest(req, testFamily)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func baseRequest(msgs ...message.Message) *provider.Request {
	return &provider.Request{
		Model:     message.ModelRef{Provider: testFamily, Model: "some/model"},
		System:    []string{"You are a coding agent.", "Extra rules."},
		Messages:  msgs,
		MaxTokens: 4096,
	}
}

// probe is a permissive view of a wire chat-completions message, used to
// inspect transcoded output regardless of its concrete content shape.
type probe struct {
	Role      string `json:"role"`
	Content   any    `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

func probeMessage(t *testing.T, raw json.RawMessage) probe {
	t.Helper()
	var p probe
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("bad message %s: %v", raw, err)
	}
	return p
}

func contentString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("content not a string %s: %v", raw, err)
	}
	return s
}

type probeContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

func contentParts(t *testing.T, raw json.RawMessage) []probeContentPart {
	t.Helper()
	var parts []probeContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("content not an array %s: %v", raw, err)
	}
	return parts
}

func marshalRaw(t *testing.T, m *apiMessage) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTranscodeSystemJoin(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	))
	if len(out.Messages) < 1 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	sys := probeMessage(t, marshalRaw(t, &out.Messages[0]))
	if sys.Role != "system" {
		t.Fatalf("first message role = %q, want system", sys.Role)
	}
	raw, err := json.Marshal(sys.Content)
	if err != nil {
		t.Fatal(err)
	}
	if contentString(t, raw) != "You are a coding agent.\n\nExtra rules." {
		t.Errorf("system content = %s", raw)
	}
}

func TestTranscodeBasics(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	))
	if out.Model != "some/model" {
		t.Errorf("model = %q", out.Model)
	}
	if !out.Stream {
		t.Error("stream not set")
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Errorf("stream_options = %+v", out.StreamOptions)
	}
	if out.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d", out.MaxTokens)
	}
}

func TestTranscodeUserTextOnly(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "what is this"}}},
	))
	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	if p.Role != "user" {
		t.Fatalf("role = %q", p.Role)
	}
	raw, _ := json.Marshal(p.Content)
	if contentString(t, raw) != "what is this" {
		t.Errorf("content = %s", raw)
	}
}

func TestTranscodeUserImage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "what is this"},
			&message.Blob{MediaType: "image/png", Data: []byte{1, 2, 3}},
			&message.Blob{MediaType: "image/jpeg", URL: "https://example.com/x.jpg"},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	raw := marshalRaw(t, &last)
	p := probeMessage(t, raw)
	if p.Role != "user" {
		t.Fatalf("role = %q", p.Role)
	}
	contentRaw, _ := json.Marshal(p.Content)
	parts := contentParts(t, contentRaw)
	if len(parts) != 3 {
		t.Fatalf("content parts = %d: %s", len(parts), contentRaw)
	}
	if parts[0].Type != "text" || parts[0].Text != "what is this" {
		t.Errorf("part0 = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("part1 = %+v", parts[1])
	}
	if parts[2].Type != "image_url" || parts[2].ImageURL.URL != "https://example.com/x.jpg" {
		t.Errorf("part2 = %+v", parts[2])
	}
}

func TestTranscodeUserNonImageBlobErrors(t *testing.T) {
	req := baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "what is this"},
			&message.Blob{MediaType: "application/pdf", Data: []byte{1, 2, 3}},
		}},
	)
	_, err := transcodeRequest(req, testFamily)
	if err == nil {
		t.Fatal("expected error for non-image blob, got nil")
	}
	if !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("error = %q, want it to name the media type application/pdf", err.Error())
	}
}

func TestTranscodeAssistantTextAndToolCalls(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Text{Text: "sure, running"},
			&message.ToolCall{CallID: "call_abc", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	if p.Role != "assistant" {
		t.Fatalf("role = %q", p.Role)
	}
	raw, _ := json.Marshal(p.Content)
	if contentString(t, raw) != "sure, running" {
		t.Errorf("content = %s", raw)
	}
	if len(p.ToolCalls) != 1 || p.ToolCalls[0].ID != "call_abc" || p.ToolCalls[0].Type != "function" ||
		p.ToolCalls[0].Function.Name != "bash" || p.ToolCalls[0].Function.Arguments != `{"command":"ls"}` {
		t.Errorf("tool_calls = %+v", p.ToolCalls)
	}
}

func TestTranscodeAssistantToolCallOnlyNoContent(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_x", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	if len(last.Content) != 0 {
		t.Errorf("content = %s, want empty", last.Content)
	}
}

func TestTranscodeToolResultsOnePerResult(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_ok", Name: "bash", Arguments: json.RawMessage(`{}`)},
			&message.ToolCall{CallID: "call_bad", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_ok", Content: message.Parts{&message.Text{Text: "fine"}}},
			&message.ToolResult{CallID: "call_bad", Content: message.Parts{&message.Text{Text: "denied"}}, IsError: true},
		}},
	))
	n := len(out.Messages)
	if n < 2 {
		t.Fatalf("messages = %d", n)
	}
	okMsg := probeMessage(t, marshalRaw(t, &out.Messages[n-2]))
	badMsg := probeMessage(t, marshalRaw(t, &out.Messages[n-1]))
	if okMsg.Role != "tool" || okMsg.ToolCallID != "call_ok" {
		t.Fatalf("ok tool message = %+v", okMsg)
	}
	rawOK, _ := json.Marshal(okMsg.Content)
	if contentString(t, rawOK) != "fine" {
		t.Errorf("ok content = %s", rawOK)
	}
	if badMsg.Role != "tool" || badMsg.ToolCallID != "call_bad" {
		t.Fatalf("bad tool message = %+v", badMsg)
	}
	rawBad, _ := json.Marshal(badMsg.Content)
	if contentString(t, rawBad) != "[tool error] denied" {
		t.Errorf("bad content = %s", rawBad)
	}
}

func TestTranscodeToolResultBlobNote(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "shot"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_img", Name: "screenshot", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_img", Content: message.Parts{
				&message.Text{Text: "captured"},
				&message.Blob{MediaType: "image/png", Data: []byte{1}},
				&message.Blob{MediaType: "image/png", Data: []byte{2}},
			}},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	raw, _ := json.Marshal(p.Content)
	if contentString(t, raw) != "captured\n[2 image attachment(s) omitted]" {
		t.Errorf("content = %s", raw)
	}
}

func TestTranscodeTools(t *testing.T) {
	req := baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	)
	req.Tools = []provider.ToolDef{{
		Name:        "bash",
		Description: "run a shell command",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
	}}
	out := mustTranscode(t, req)
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %d", len(out.Tools))
	}
	tool := out.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "bash" || tool.Function.Description != "run a shell command" {
		t.Errorf("tool = %+v", tool)
	}
	if !jsonEqual(t, tool.Function.Parameters, json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)) {
		t.Errorf("parameters = %s", tool.Function.Parameters)
	}
}

func TestTranscodeForeignReasoningDropped(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "anthropic thinking", ProviderData: message.ProviderData{
				"anthropic": json.RawMessage(`{"signature":"sig"}`),
			}},
			&message.Text{Text: "answer"},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	raw, _ := json.Marshal(p.Content)
	if contentString(t, raw) != "answer" {
		t.Errorf("content = %s, reasoning should be dropped", raw)
	}
}

func TestTranscodeSameFamilyReasoningStillDropped(t *testing.T) {
	// Compat endpoints have no wire field to replay opaque reasoning into,
	// and this adapter's own stream assembly never stores anything under
	// ProviderData[family] in the first place — so even "same family"
	// reasoning data is dropped. There is no signed-reasoning replay here.
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "pondering", ProviderData: message.ProviderData{
				testFamily: json.RawMessage(`{"anything":"here"}`),
			}},
			&message.Text{Text: "answer"},
		}},
	))
	last := out.Messages[len(out.Messages)-1]
	p := probeMessage(t, marshalRaw(t, &last))
	raw, _ := json.Marshal(p.Content)
	if contentString(t, raw) != "answer" {
		t.Errorf("content = %s, reasoning should be dropped", raw)
	}
}

func TestWireCallID(t *testing.T) {
	if got := wireCallID("call_01ABC"); got != "call_01ABC" {
		t.Errorf("passthrough = %q", got)
	}
	a := wireCallID("call with spaces!")
	b := wireCallID("call with spaces!")
	if a != b {
		t.Error("derivation not deterministic")
	}
	if !strings.HasPrefix(a, "call_") || len(a) > 64 {
		t.Errorf("derived id = %q", a)
	}
}

func TestTranscodeEmptyHistoryFails(t *testing.T) {
	if _, err := transcodeRequest(baseRequest(), testFamily); err == nil {
		t.Fatal("expected error for empty request")
	}
}

// jsonEqual reports whether two JSON documents are semantically equal.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("bad json %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}
