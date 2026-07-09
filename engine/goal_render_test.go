package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestRenderPart covers renderPart's part-type switch for the goal
// evaluator's transcript rendering (renderConversation). Before this test,
// coverage only exercised the *message.Text branch (via renderConversation
// in the ordinary goal-loop tests); ToolCall, ToolResult (both ok and
// is_error), Reasoning, and Blob were 0-hit.
func TestRenderPart(t *testing.T) {
	tests := []struct {
		name string
		part message.Part
		want string
	}{
		{
			name: "text",
			part: &message.Text{Text: "hello"},
			want: "hello",
		},
		{
			name: "reasoning",
			part: &message.Reasoning{Text: "thinking it through"},
			want: "[reasoning] thinking it through",
		},
		{
			name: "tool call",
			part: &message.ToolCall{CallID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			want: `[tool call bash] {"command":"ls"}`,
		},
		{
			name: "tool result ok",
			part: &message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "file1\nfile2"}}},
			want: "[tool result] file1\nfile2",
		},
		{
			name: "tool result is_error",
			part: &message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "no such file"}}, IsError: true},
			want: "[tool result (error)] no such file",
		},
		{
			name: "blob",
			part: &message.Blob{MediaType: "image/png"},
			want: "[blob image/png]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := renderPart(tt.part)
			if got != tt.want {
				t.Errorf("renderPart(%#v) = %q, want %q", tt.part, got, tt.want)
			}
		})
	}
}

// TestRenderConversationIncludesAllPartKinds exercises renderPart through its
// only caller, renderConversation, with a history containing every part kind
// in one assistant/tool turn — the shape the goal evaluator actually sees
// when the worker has called a tool.
func TestRenderConversationIncludesAllPartKinds(t *testing.T) {
	history := []message.Message{
		{
			Role: message.RoleAssistant,
			Parts: message.Parts{
				&message.Reasoning{Text: "let me check the files"},
				&message.Text{Text: "Checking now."},
				&message.ToolCall{CallID: "call_1", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
			},
		},
		{
			Role: message.RoleTool,
			Parts: message.Parts{
				&message.ToolResult{CallID: "call_1", Content: message.Parts{&message.Text{Text: "missing.txt: no such file"}}, IsError: true},
			},
		},
	}
	got := renderConversation(history)

	for _, want := range []string{
		"[reasoning] let me check the files",
		"Checking now.",
		`[tool call bash] {"command":"ls"}`,
		"[tool result (error)] missing.txt: no such file",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderConversation missing %q in:\n%s", want, got)
		}
	}
}
