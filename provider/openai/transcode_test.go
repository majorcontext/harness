package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func mustTranscode(t *testing.T, req *provider.Request) *apiRequest {
	t.Helper()
	out, err := transcodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func baseRequest(msgs ...message.Message) *provider.Request {
	return &provider.Request{
		Model:     message.ModelRef{Provider: Family, Model: "gpt-5"},
		System:    []string{"You are a coding agent.", "Extra rules."},
		Messages:  msgs,
		MaxTokens: 4096,
	}
}

// probe is a permissive view of a single OpenAI Responses input item, used to
// inspect transcoded output regardless of the item's concrete shape.
type probe struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	} `json:"content"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
}

// probeContent mirrors apiContentPart including file fields for inspection.
type probeContent struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
}

func probeContents(t *testing.T, raw json.RawMessage) []probeContent {
	t.Helper()
	var item struct {
		Content []probeContent `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("bad item %s: %v", raw, err)
	}
	return item.Content
}

func probeItem(t *testing.T, raw json.RawMessage) probe {
	t.Helper()
	var p probe
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("bad item %s: %v", raw, err)
	}
	return p
}

func TestTranscodeSystemToInstructions(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	))

	if out.Instructions != "You are a coding agent.\n\nExtra rules." {
		t.Errorf("instructions = %q", out.Instructions)
	}
	if out.Model != "gpt-5" {
		t.Errorf("model = %q", out.Model)
	}
	if out.MaxOutputTokens != 4096 {
		t.Errorf("max_output_tokens = %d", out.MaxOutputTokens)
	}
	if !out.Stream {
		t.Error("stream not set")
	}
}

func TestTranscodeStoreAndIncludeAlwaysSet(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	))
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Store {
		t.Error("store should be false")
	}
	if !strings.Contains(string(raw), `"store":false`) {
		t.Errorf("store:false not in wire request:\n%s", raw)
	}
	if len(out.Include) != 1 || out.Include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v", out.Include)
	}
	if !strings.Contains(string(raw), `"include":["reasoning.encrypted_content"]`) {
		t.Errorf("include not in wire request:\n%s", raw)
	}
}

func TestTranscodeUserMessageAndImage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "what is this"},
			&message.Blob{MediaType: "image/png", Data: []byte{1, 2, 3}},
			&message.Blob{MediaType: "image/jpeg", URL: "https://example.com/x.jpg"},
		}},
	))
	if len(out.Input) != 1 {
		t.Fatalf("input items = %d", len(out.Input))
	}
	p := probeItem(t, out.Input[0])
	if p.Type != "message" || p.Role != "user" || len(p.Content) != 3 {
		t.Fatalf("item = %+v", p)
	}
	if p.Content[0].Type != "input_text" || p.Content[0].Text != "what is this" {
		t.Errorf("text content = %+v", p.Content[0])
	}
	if p.Content[1].Type != "input_image" || !strings.HasPrefix(p.Content[1].ImageURL, "data:image/png;base64,") {
		t.Errorf("inline image content = %+v", p.Content[1])
	}
	if p.Content[2].Type != "input_image" || p.Content[2].ImageURL != "https://example.com/x.jpg" {
		t.Errorf("url image content = %+v", p.Content[2])
	}
}

func TestTranscodeBlobPDFData(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "summarize"},
			&message.Blob{MediaType: "application/pdf", Data: []byte("%PDF-fake")},
		}},
	))
	content := probeContents(t, out.Input[0])
	if len(content) != 2 {
		t.Fatalf("content parts = %d", len(content))
	}
	pdf := content[1]
	if pdf.Type != "input_file" || pdf.Filename != "file.pdf" ||
		!strings.HasPrefix(pdf.FileData, "data:application/pdf;base64,") {
		t.Errorf("pdf content = %+v", pdf)
	}
	if pdf.ImageURL != "" {
		t.Errorf("pdf typed as image: %+v", pdf)
	}
}

func TestTranscodeBlobPDFURLUnsupported(t *testing.T) {
	_, err := transcodeRequest(baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Blob{MediaType: "application/pdf", URL: "https://example.com/doc.pdf"},
		}},
	))
	if err == nil || !strings.Contains(err.Error(), "application/pdf") {
		t.Fatalf("err = %v", err)
	}
}

func TestTranscodeBlobUnsupportedMediaType(t *testing.T) {
	_, err := transcodeRequest(baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{
			&message.Blob{MediaType: "audio/mpeg", Data: []byte{1, 2}},
		}},
	))
	if err == nil || !strings.Contains(err.Error(), "audio/mpeg") {
		t.Fatalf("err = %v", err)
	}
}

func TestTranscodeAssistantText(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "hello there"}}},
	))
	p := probeItem(t, out.Input[len(out.Input)-1])
	if p.Type != "message" || p.Role != "assistant" || len(p.Content) != 1 {
		t.Fatalf("item = %+v", p)
	}
	if p.Content[0].Type != "output_text" || p.Content[0].Text != "hello there" {
		t.Errorf("content = %+v", p.Content[0])
	}
}

func TestTranscodeToolCallAndResult(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_abc", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_abc", Content: message.Parts{
				&message.Text{Text: "file.go"},
				&message.Text{Text: "main.go"},
			}},
		}},
	))

	// user message, function_call, function_call_output.
	if len(out.Input) != 3 {
		t.Fatalf("input items = %d", len(out.Input))
	}
	call := probeItem(t, out.Input[1])
	if call.Type != "function_call" || call.CallID != "call_abc" || call.Name != "bash" ||
		call.Arguments != `{"command":"ls"}` {
		t.Errorf("function_call = %+v", call)
	}
	res := probeItem(t, out.Input[2])
	if res.Type != "function_call_output" || res.CallID != "call_abc" || res.Output != "file.go\nmain.go" {
		t.Errorf("function_call_output = %+v", res)
	}
}

func TestTranscodeToolResultIsError(t *testing.T) {
	// function_call_output has no boolean error field; IsError is encoded as
	// a marker prefix on the output text so the model can distinguish a
	// failed/denied call from a successful one.
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_ok", Name: "bash", Arguments: json.RawMessage(`{}`)},
			&message.ToolCall{CallID: "call_bad", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_ok", Content: message.Parts{&message.Text{Text: "fine"}}},
			&message.ToolResult{CallID: "call_bad", Content: message.Parts{&message.Text{Text: "permission denied"}}, IsError: true},
		}},
	))
	ok := probeItem(t, out.Input[3])
	if ok.CallID != "call_ok" || ok.Output != "fine" {
		t.Errorf("success output = %+v", ok)
	}
	bad := probeItem(t, out.Input[4])
	if bad.CallID != "call_bad" || bad.Output != "[tool error] permission denied" {
		t.Errorf("error output = %+v", bad)
	}
}

func TestTranscodeToolResultBlobNotSilentlyDropped(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "shot"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_mix", Name: "screenshot", Arguments: json.RawMessage(`{}`)},
			&message.ToolCall{CallID: "call_img", Name: "screenshot", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			// Text plus two images.
			&message.ToolResult{CallID: "call_mix", Content: message.Parts{
				&message.Text{Text: "captured"},
				&message.Blob{MediaType: "image/png", Data: []byte{1}},
				&message.Blob{MediaType: "image/png", Data: []byte{2}},
			}},
			// Image only: the output must still surface the attachment.
			&message.ToolResult{CallID: "call_img", Content: message.Parts{
				&message.Blob{MediaType: "image/png", Data: []byte{3}},
			}},
		}},
	))
	mix := probeItem(t, out.Input[3])
	if mix.Output != "captured\n[2 image attachment(s) omitted]" {
		t.Errorf("mixed output = %q", mix.Output)
	}
	img := probeItem(t, out.Input[4])
	if img.Output != "[1 image attachment(s) omitted]" {
		t.Errorf("image-only output = %q", img.Output)
	}
}

func TestTranscodeToolCallEmptyArguments(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "call_x", Name: "now"},
		}},
	))
	call := probeItem(t, out.Input[len(out.Input)-1])
	if call.Arguments != "{}" {
		t.Errorf("empty arguments = %q", call.Arguments)
	}
}

func TestWireCallID(t *testing.T) {
	// Wire-safe IDs from OpenAI pass through untouched.
	if got := wireCallID("call_01ABC"); got != "call_01ABC" {
		t.Errorf("passthrough = %q", got)
	}
	// Foreign IDs get a deterministic derived replacement.
	a := wireCallID("call with spaces!")
	b := wireCallID("call with spaces!")
	if a != b {
		t.Error("derivation not deterministic")
	}
	if !strings.HasPrefix(a, "call_") || len(a) > 64 {
		t.Errorf("derived id = %q", a)
	}
	// A foreign (non-wire-safe) id derives identically for both the call and
	// the result so they stay linked.
	foreign := "foreign id/with:chars"
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: foreign, Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: foreign, Content: message.Parts{&message.Text{Text: "ok"}}},
		}},
	))
	call := probeItem(t, out.Input[1])
	res := probeItem(t, out.Input[2])
	if call.CallID != res.CallID {
		t.Errorf("call/result ids diverge: %q vs %q", call.CallID, res.CallID)
	}
	if !strings.HasPrefix(call.CallID, "call_") {
		t.Errorf("derived call id = %q", call.CallID)
	}
}

func TestTranscodeReasoningReplayVerbatim(t *testing.T) {
	rawReasoning := json.RawMessage(`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"hmm"}],"encrypted_content":"ENC"}`)
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "hmm", ProviderData: message.ProviderData{
				Family: rawReasoning,
			}},
			&message.Text{Text: "answer"},
		}},
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "thanks"}}},
	))

	// user, [reasoning verbatim], assistant message, user.
	if len(out.Input) != 4 {
		t.Fatalf("input items = %d: %s", len(out.Input), out.Input)
	}
	got := out.Input[1]
	if !jsonEqual(t, got, rawReasoning) {
		t.Errorf("reasoning not replayed verbatim:\n got %s\nwant %s", got, rawReasoning)
	}
	asst := probeItem(t, out.Input[2])
	if asst.Type != "message" || asst.Role != "assistant" || asst.Content[0].Text != "answer" {
		t.Errorf("assistant item = %+v", asst)
	}
}

func TestTranscodeForeignReasoningDropped(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			// Anthropic thinking: no "openai" key, dropped silently.
			&message.Reasoning{Text: "anthropic thinking", ProviderData: message.ProviderData{
				"anthropic": json.RawMessage(`{"signature":"sig"}`),
			}},
			&message.Text{Text: "answer"},
		}},
	))
	// user, assistant message (reasoning gone).
	if len(out.Input) != 2 {
		t.Fatalf("input items = %d: %s", len(out.Input), out.Input)
	}
	asst := probeItem(t, out.Input[1])
	if asst.Type != "message" || asst.Content[0].Text != "answer" {
		t.Errorf("assistant item = %+v", asst)
	}
}

// TestTranscodeReasoningEmptyProviderDataMarshal is the round-2 forensic
// regression guard, reconstructed at the transcoder layer: a Reasoning part
// whose "openai" provider_data entry is present but zero-length (non-nil)
// — the shape #42 left unguarded, one map-indirection away from the
// ToolCall.Arguments field #42 actually fixed (see message.ProviderData's
// doc comment). Before message.ProviderData grew a Get accessor,
// transcodeMessage read this entry straight out of the map
// (v.ProviderData[Family]) and copied it via append(json.RawMessage(nil),
// raw...) — which Go's append happens to normalize to a nil slice when
// raw has zero length, so this particular call site never actually hit
// the json.Marshal crash (unlike the direct-marshal path guarded by
// message.ProviderData.MarshalJSON, and unlike the anthropic transcoder's
// json.Unmarshal call on the same shape — see
// TestTranscodeReasoningEmptyProviderDataDropped). It was still a real bug:
// a nil json.RawMessage marshals as the JSON literal null, so the request
// sent to the wire carried a spurious `null` item in its input list instead
// of the reasoning item being dropped like a foreign-provider one. This
// test exercises the full path (transcodeRequest, then json.Marshal(out) —
// the "AND marshal request" the incident's method requires, not just the
// per-item transcode step) and asserts the empty entry is dropped
// entirely, matching TestTranscodeForeignReasoningDropped, with no
// spurious item and no error either before or after the fix.
func TestTranscodeReasoningEmptyProviderDataMarshal(t *testing.T) {
	for _, c := range []struct {
		name string
		data json.RawMessage
	}{
		{"empty-non-nil", json.RawMessage{}},
		{"empty-string-literal", json.RawMessage("")},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := baseRequest(
				message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
				message.Message{Role: message.RoleAssistant, Parts: message.Parts{
					&message.Reasoning{Text: "hmm", ProviderData: message.ProviderData{
						Family: c.data,
					}},
					&message.Text{Text: "answer"},
				}},
			)
			out, err := transcodeRequest(req)
			if err != nil {
				t.Fatalf("transcodeRequest: %v", err)
			}
			// The full wire-request marshal: this is the exact call that
			// failed in production with "json: error calling MarshalJSON
			// for type json.RawMessage: unexpected end of JSON input".
			if _, err := json.Marshal(out); err != nil {
				t.Fatalf("marshal apiRequest: %v", err)
			}
			// user, assistant message (empty reasoning item dropped, same
			// as a foreign-provider reasoning item).
			if len(out.Input) != 2 {
				t.Fatalf("input items = %d: %s", len(out.Input), out.Input)
			}
		})
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
	if tool.Type != "function" || tool.Name != "bash" || tool.Description != "run a shell command" {
		t.Errorf("tool = %+v", tool)
	}
	if !jsonEqual(t, tool.Parameters, json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)) {
		t.Errorf("parameters = %s", tool.Parameters)
	}
}

func TestTranscodeEmptyHistoryFails(t *testing.T) {
	if _, err := transcodeRequest(baseRequest()); err == nil {
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
