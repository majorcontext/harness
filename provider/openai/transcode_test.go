package openai

import (
	"encoding/base64"
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
			&message.Blob{MediaType: "image/png", Data: tinyPNG(t)},
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
				&message.Blob{MediaType: "image/png", Data: tinyPNG(t)},
				&message.Blob{MediaType: "image/png", Data: tinyPNG(t)},
			}},
			// Image only: the output must still surface the attachment.
			&message.ToolResult{CallID: "call_img", Content: message.Parts{
				&message.Blob{MediaType: "image/png", Data: tinyPNG(t)},
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
	// The dangling ToolCall now gets a synthetic function_call_output
	// appended after it (see TestTranscodeResolvesOrphanToolCalls), so the
	// function_call itself is the second-to-last item.
	call := probeItem(t, out.Input[len(out.Input)-2])
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

// TestTranscodeResolvesOrphanToolCalls: the Responses transcoder was the
// one transcoder NOT calling message.ResolveOrphanToolCalls at request
// build (anthropic and openaicompat both do — see their transcode.go and
// message.ResolveOrphanToolCalls's incident doc), so an assistant ToolCall
// with no following ToolResult transcoded to a dangling function_call item
// the API rejects on every retry. The repair must run here too: the
// dangling call gets a synthetic function_call_output immediately after.
func TestTranscodeResolvesOrphanToolCalls(t *testing.T) {
	req := &provider.Request{
		Model: message.ModelRef{Provider: Family, Model: "gpt-x"},
		Messages: []message.Message{
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
			{Role: message.RoleAssistant, Parts: message.Parts{
				&message.ToolCall{CallID: "call_orphan", Name: "bash", Arguments: []byte(`{}`)},
			}},
			// No tool message follows: the turn died before execution and
			// the synthetic-result append was lost (crash window), or the
			// history came from another producer entirely.
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "continue"}}},
		},
		MaxTokens: 10,
	}
	out, err := transcodeRequest(req)
	if err != nil {
		t.Fatalf("transcodeRequest: %v", err)
	}
	var callIdx, outputIdx = -1, -1
	for i, raw := range out.Input {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("input item %d: %v", i, err)
		}
		switch item.Type {
		case "function_call":
			if item.CallID == "call_orphan" {
				callIdx = i
			}
		case "function_call_output":
			if item.CallID == "call_orphan" {
				outputIdx = i
			}
		}
	}
	if callIdx == -1 {
		t.Fatal("function_call item for the orphaned ToolCall not transcoded at all")
	}
	if outputIdx == -1 {
		t.Fatal("no function_call_output synthesized for the orphaned ToolCall — the request ships dangling and the API will reject it")
	}
	if outputIdx != callIdx+1 {
		t.Errorf("function_call_output at %d, want immediately after the call at %d", outputIdx, callIdx)
	}
}

// TestTranscodeOrphanToolResultBuildsSuccessfully is the golden, wire-level
// counterpart to the openaicompat regression test of the same name (see PR
// #108's finding 1): an orphan ToolResult (its CallID matches no ToolCall
// anywhere in history) is demoted to a Text part by message.NormalizeForWire.
// This adapter's own transcodeMessage is role-agnostic -- a Text part is
// valid in a "user"-role input item regardless of the enclosing canonical
// message's Role -- so this was never the regressed provider, but it is
// asserted here too, against the REAL transcodeRequest, so a future change
// to this adapter's own role handling is caught by the same golden shape
// all three providers share.
func TestTranscodeOrphanToolResultBuildsSuccessfully(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "GHOST", Content: message.Parts{&message.Text{Text: "ORPHAN OUTPUT"}}},
		}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ok"}}},
	))

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal transcoded request: %v", err)
	}
	rendered := string(raw)
	if !strings.Contains(rendered, "GHOST") {
		t.Errorf("transcoded request does not mention the orphan call id GHOST: %s", rendered)
	}
	if !strings.Contains(rendered, "ORPHAN OUTPUT") {
		t.Errorf("transcoded request does not carry the orphan result's real content: %s", rendered)
	}
	// No function_call/function_call_output item may carry the orphan id --
	// it must have been demoted to plain text, not left claiming to answer
	// a call that never happened.
	for i, item := range out.Input {
		var probed struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(item, &probed); err != nil {
			t.Fatalf("input item %d: %v", i, err)
		}
		if probed.CallID == "GHOST" {
			t.Fatalf("GHOST survived as a %s item instead of being demoted to text: %s", probed.Type, item)
		}
	}
}

// TestTranscodeOrphanToolResultImageBlobArrivesAsRealImagePart is the
// golden regression test for PR #108 round 5's finding on
// message/wire_normalize.go:496: a demoted ToolResult's Blob used to
// survive as a raw Part regardless of media type, and this adapter's own
// transcodeBlob (~line 298) hard-errors building a request containing a
// non-image/*, non-application/pdf Blob, or an application/pdf Blob
// referenced by URL rather than inline data — turning the orphan-
// tool_result wedge NormalizeForWire exists to fix back into a total
// request-BUILD failure. This carries a build-safe image AND an
// unsupported media type in the SAME demoted result: the request must
// still build, the image must arrive as a real input_image part, and the
// unsupported Blob must be note-flattened, never a raw content part.
func TestTranscodeOrphanToolResultImageBlobArrivesAsRealImagePart(t *testing.T) {
	png := tinyPNG(t)
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "GHOST", Content: message.Parts{
				&message.Text{Text: "ORPHAN OUTPUT"},
				&message.Blob{MediaType: "image/png", Data: png},
				&message.Blob{MediaType: "audio/mpeg", Data: []byte{1, 2, 3}},
			}},
		}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "ok"}}},
	))

	wantImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	var foundImage bool
	var rendered strings.Builder
	for i, item := range out.Input {
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal item %d: %v", i, err)
		}
		rendered.Write(raw)
		for _, c := range probeContents(t, item) {
			if c.Type == "input_image" && c.ImageURL == wantImageURL {
				foundImage = true
			}
			if strings.Contains(c.FileData, "audio") || strings.Contains(c.ImageURL, "audio") {
				t.Fatalf("non-build-safe Blob survived as a raw content part: %+v", c)
			}
		}
	}
	if !foundImage {
		t.Fatalf("demoted ToolResult's image did not arrive as a real input_image part: %s", rendered.String())
	}
	if !strings.Contains(rendered.String(), "audio/mpeg") {
		t.Fatalf("note-flattened Blob's media type is not findable in the transcoded request: %s", rendered.String())
	}
}

// TestTranscodeAssistantRunBlobDemotionBuildsAndNeverEntersAssistantTurn is
// the golden regression test for PR #108 round 5's finding on
// message/wire_normalize.go:370: a demoted ToolResult's Blob must never be
// left inside an assistant-role wire item. Two ToolResults sharing one
// assistant message (both with no ToolCall anywhere) reach
// demoteWireInvalidToolResults' assistant-run branch at all — see
// message/wire_normalize_test.go's
// TestNormalizeForWireAssistantRunBlobHoistedOutOfAssistantMessage for why
// a single one alone would not.
func TestTranscodeAssistantRunBlobDemotionBuildsAndNeverEntersAssistantTurn(t *testing.T) {
	png := tinyPNG(t)
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolResult{CallID: "A", Content: message.Parts{
				&message.Text{Text: "stuck in assistant"},
				&message.Blob{MediaType: "image/png", Data: png},
			}},
			&message.ToolResult{CallID: "B", Content: message.Parts{&message.Text{Text: "also stuck"}}},
		}},
	))

	wantImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	var foundImage bool
	for i, item := range out.Input {
		p := probeItem(t, item)
		for _, c := range p.Content {
			if p.Role == "assistant" && c.Type == "input_image" {
				t.Fatalf("input item %d: a demoted Blob landed in an assistant-role item: %+v", i, p)
			}
			if c.Type == "input_image" && c.ImageURL == wantImageURL {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("the hoisted image did not arrive as a real input_image part anywhere: %+v", out.Input)
	}
}

// TestTranscodeAssistantRunBlobHoistDoesNotSplitToolCallsFromTheirAnswer is
// the golden regression test for PR #108 round 6's finding: the
// assistant-run blob hoist (round 5) placed the hoisted item immediately
// after the assistant's own function_call item -- before the
// function_call_output answering that SAME function_call. This adapter's
// flat, call-id-addressed item list tolerates the interposed item (there is
// no message-turn grouping to violate), but this test pins the shape here
// too, so a future change to this adapter's own item ordering is caught by
// the same golden repro all three providers share. See
// message/wire_normalize_test.go's
// TestNormalizeForWireAssistantRunBlobHoistLandsAfterTheAnswerRunToo for the
// canonical-level account (why TWO ToolResults, A and B, are needed
// alongside the live, answered ToolCall C).
func TestTranscodeAssistantRunBlobHoistDoesNotSplitToolCallsFromTheirAnswer(t *testing.T) {
	png := tinyPNG(t)
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "C", Name: "bash", Arguments: json.RawMessage(`{}`)},
			&message.ToolResult{CallID: "A", Content: message.Parts{
				&message.Text{Text: "stuck in assistant"},
				&message.Blob{MediaType: "image/png", Data: png},
			}},
			&message.ToolResult{CallID: "B", Content: message.Parts{&message.Text{Text: "also stuck"}}},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "C", Content: message.Parts{&message.Text{Text: "REAL C OUTPUT"}}},
		}},
	))

	wantImageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	var foundAnswer, foundImage bool
	for i, item := range out.Input {
		p := probeItem(t, item)
		if p.Role == "assistant" {
			for _, c := range p.Content {
				if c.Type == "input_image" {
					t.Fatalf("input item %d: a demoted Blob landed in an assistant-role item: %+v", i, p)
				}
			}
		}
		if p.Type == "function_call_output" && p.CallID == "C" && p.Output == "REAL C OUTPUT" {
			foundAnswer = true
		}
		for _, c := range p.Content {
			if c.Type == "input_image" && c.ImageURL == wantImageURL {
				foundImage = true
			}
		}
	}
	if !foundAnswer {
		t.Fatalf("no function_call_output answering C found anywhere: %+v", out.Input)
	}
	if !foundImage {
		t.Fatalf("the hoisted image did not arrive as a real input_image part anywhere: %+v", out.Input)
	}
}

// TestTranscodeOrphanToolResultDoesNotSplitContiguousToolRun is the golden,
// wire-level counterpart to openaicompat's regression test of the same name
// (PR #108's review round 2): a stray (unanswerable) ToolResult sitting in
// the FIRST of two consecutive RoleTool messages must not corrupt the real
// function_call_output items answering the preceding assistant's
// function_calls. This adapter's flat, call-id-addressed item list was
// never the regressed provider for this shape (no contiguity requirement
// to violate), but it is pinned against the REAL transcodeRequest so a
// future change here is caught by the same golden shape all three
// providers share.
func TestTranscodeOrphanToolResultDoesNotSplitContiguousToolRun(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "A", Name: "bash", Arguments: json.RawMessage(`{}`)},
			&message.ToolCall{CallID: "B", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "A", Content: message.Parts{&message.Text{Text: "RA"}}},
			&message.ToolResult{CallID: "GHOST", Content: message.Parts{&message.Text{Text: "STRAY"}}},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "B", Content: message.Parts{&message.Text{Text: "RB"}}},
		}},
	))

	outputIdxA, outputIdxB, ghostIdx := -1, -1, -1
	for i, raw := range out.Input {
		var item struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Output  string `json:"output"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatalf("input item %d: %v", i, err)
		}
		switch {
		case item.Type == "function_call_output" && item.CallID == "A":
			outputIdxA = i
			if item.Output != "RA" {
				t.Errorf("function_call_output A = %q, want %q", item.Output, "RA")
			}
		case item.Type == "function_call_output" && item.CallID == "B":
			outputIdxB = i
			if item.Output != "RB" {
				t.Errorf("function_call_output B = %q, want %q", item.Output, "RB")
			}
		case item.Type == "function_call_output" && item.CallID == "GHOST":
			t.Fatalf("GHOST survived as a function_call_output item instead of being demoted to text: %s", raw)
		case item.Type == "message":
			for _, c := range item.Content {
				if strings.Contains(c.Text, "GHOST") && strings.Contains(c.Text, "STRAY") {
					ghostIdx = i
				}
			}
		}
	}
	if outputIdxA == -1 || outputIdxB == -1 {
		t.Fatalf("real function_call_output items for A and B must both survive: %+v", out.Input)
	}
	if ghostIdx == -1 {
		t.Fatalf("demoted GHOST result (call id and content) not found in any message item: %+v", out.Input)
	}
	if ghostIdx < outputIdxA || ghostIdx < outputIdxB {
		t.Fatalf("demoted GHOST item at index %d must come after both real outputs (A at %d, B at %d): %+v", ghostIdx, outputIdxA, outputIdxB, out.Input)
	}
}
