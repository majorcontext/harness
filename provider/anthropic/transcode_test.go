package anthropic

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
		Model:     message.ModelRef{Provider: Family, Model: "claude-fable-5"},
		System:    []string{"You are a coding agent.", "Extra rules."},
		Messages:  msgs,
		MaxTokens: 4096,
	}
}

func TestTranscodeSystemAndCacheControl(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}},
	))

	if len(out.System) != 2 {
		t.Fatalf("system blocks = %d", len(out.System))
	}
	if out.System[0].CacheControl != nil {
		t.Error("cache_control on non-final system block")
	}
	if out.System[1].CacheControl == nil {
		t.Error("missing cache_control on final system block")
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Error("missing cache_control on final content block")
	}
}

func TestTranscodeForeignReasoningDroppedAndMerged(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "first"}}},
		// Assistant turn whose only content is another provider's
		// reasoning: transcodes to nothing.
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "gpt thinking", ProviderData: message.ProviderData{
				"openai-responses": json.RawMessage(`{"encrypted":"xyz"}`),
			}},
		}},
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "second"}}},
	))

	// The empty assistant turn vanishes and the two user turns merge to
	// preserve role alternation.
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1 (merged)", len(out.Messages))
	}
	m := out.Messages[0]
	if m.Role != "user" || len(m.Content) != 2 || m.Content[0].Text != "first" || m.Content[1].Text != "second" {
		t.Errorf("merged message = %+v", m)
	}
}

// TestTranscodeReasoningEmptyProviderDataDropped is the round-2 forensic
// regression guard at the anthropic transcoder: a Reasoning part whose
// "anthropic" provider_data entry is present but zero-length (non-nil) —
// the exact shape #42 left unguarded, one map-indirection away from the
// ToolCall.Arguments field #42 actually fixed (see message.ProviderData's
// doc comment). Before message.ProviderData grew a Get accessor, this
// transcoder read the entry straight out of the map (ok == true) and handed
// the empty bytes to json.Unmarshal, which failed with "bad anthropic
// reasoning data: unexpected end of JSON input" instead of ever reaching
// the request marshal below — a related but distinct crash from the
// production "MarshalJSON for type json.RawMessage" error, closed by the
// same fix. A present-but-empty entry must now be treated exactly like a
// foreign-provider one: dropped, not unmarshaled.
func TestTranscodeReasoningEmptyProviderDataDropped(t *testing.T) {
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
					&message.Reasoning{Text: "let me think", ProviderData: message.ProviderData{
						Family: c.data,
					}},
					&message.Text{Text: "answer"},
				}},
			)
			out, err := transcodeRequest(req)
			if err != nil {
				t.Fatalf("transcodeRequest: %v", err)
			}
			// The full wire-request marshal, same as the incident's actual
			// failure point.
			if _, err := json.Marshal(out); err != nil {
				t.Fatalf("marshal apiRequest: %v", err)
			}
			asst := out.Messages[1]
			if len(asst.Content) != 1 || asst.Content[0].Text != "answer" {
				t.Errorf("assistant content = %+v, want the empty reasoning block dropped", asst.Content)
			}
		})
	}
}

func TestTranscodeThinkingReplay(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "let me think", ProviderData: message.ProviderData{
				Family: json.RawMessage(`{"signature":"sig123"}`),
			}},
			&message.Reasoning{ProviderData: message.ProviderData{
				Family: json.RawMessage(`{"redacted":"opaque"}`),
			}},
			&message.Text{Text: "answer"},
		}},
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "thanks"}}},
	))

	asst := out.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("role = %s", asst.Role)
	}
	if asst.Content[0].Type != "thinking" || asst.Content[0].Thinking == nil ||
		*asst.Content[0].Thinking != "let me think" || asst.Content[0].Signature != "sig123" {
		t.Errorf("thinking block = %+v", asst.Content[0])
	}
	if asst.Content[1].Type != "redacted_thinking" || asst.Content[1].Data != "opaque" {
		t.Errorf("redacted block = %+v", asst.Content[1])
	}
}

func TestTranscodeToolCallAndResult(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "run it"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "toolu_abc", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "toolu_abc", Content: message.Parts{
				&message.Text{Text: "file.go"},
				&message.Blob{MediaType: "image/png", Data: []byte{1, 2}},
			}, IsError: true},
		}},
	))

	use := out.Messages[1].Content[0]
	if use.Type != "tool_use" || use.ID != "toolu_abc" || use.Name != "bash" {
		t.Errorf("tool_use = %+v", use)
	}
	// RoleTool maps to wire role "user".
	res := out.Messages[2]
	if res.Role != "user" {
		t.Fatalf("tool result role = %s", res.Role)
	}
	tr := res.Content[0]
	if tr.Type != "tool_result" || tr.ToolUseID != "toolu_abc" || !tr.IsError {
		t.Errorf("tool_result = %+v", tr)
	}
	if len(tr.Content) != 2 || tr.Content[0].Text != "file.go" || tr.Content[1].Source.Type != "base64" {
		t.Errorf("tool_result content = %+v", tr.Content)
	}
}

func TestTranscodeEmptyThinkingKeepsField(t *testing.T) {
	// The API requires the "thinking" field on thinking blocks even when the
	// text is empty; omitempty dropping it causes an invalid_request_error
	// (found by harness building harness — a replayed empty thinking block
	// 400ed mid-session).
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "", ProviderData: message.ProviderData{
				Family: json.RawMessage(`{"signature":"sig123"}`),
			}},
			&message.Text{Text: "answer"},
		}},
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "next"}}},
	))
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"thinking":""`) {
		t.Errorf("empty thinking field omitted from wire request:\n%s", raw)
	}
}

func TestWireCallID(t *testing.T) {
	// Wire-safe IDs (same-provider replay) pass through untouched.
	if got := wireCallID("toolu_01ABC"); got != "toolu_01ABC" {
		t.Errorf("passthrough = %q", got)
	}
	// Foreign IDs get a deterministic derived replacement.
	a := wireCallID("call with spaces!")
	b := wireCallID("call with spaces!")
	if a != b {
		t.Error("derivation not deterministic")
	}
	if !strings.HasPrefix(a, "toolu_") || len(a) > 64 {
		t.Errorf("derived id = %q", a)
	}
}

func TestTranscodeEmptyHistoryFails(t *testing.T) {
	if _, err := transcodeRequest(baseRequest()); err == nil {
		t.Fatal("expected error for empty request")
	}
}
