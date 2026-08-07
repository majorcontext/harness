package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestTranscodeOrphanDuplicateCallIDInOneMessage is an end-to-end,
// real-transcoder companion to
// message.TestResolveOrphanToolCallsDuplicateCallIDInOneMessage: two
// ToolCall parts sharing one CallID in a single assistant message,
// followed by only one matching ToolResult. Before
// message.ResolveOrphanToolCalls became count-aware, the set-membership
// `present` map marked the id satisfied on the first match, so the wire
// request carried 2 tool_use blocks for the id but only 1 tool_result --
// exactly the imbalance the Anthropic API 400s on.
func TestTranscodeOrphanDuplicateCallIDInOneMessage(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "dup1", Name: "bash", Arguments: json.RawMessage(`{}`)},
			&message.ToolCall{CallID: "dup1", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "dup1", Content: message.Parts{&message.Text{Text: "ok"}}},
		}},
	))

	var toolUseCount, toolResultCount int
	for _, m := range out.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if b.ID == "dup1" {
					toolUseCount++
				}
			case "tool_result":
				if b.ToolUseID == "dup1" {
					toolResultCount++
				}
			}
		}
	}
	if toolUseCount != 2 {
		t.Fatalf("tool_use count for dup1 = %d, want 2", toolUseCount)
	}
	if toolResultCount != 2 {
		t.Fatalf("tool_result count for dup1 = %d, want 2 (1 real + 1 synthesized)", toolResultCount)
	}
}

// TestTranscodeOrphanStrayResultBeforeCall is an end-to-end, real-transcoder
// companion to message.TestResolveOrphanToolCallsStrayResultBeforeCall: a
// ToolResult for callX appears in history BEFORE the assistant message that
// actually issues callX. Before ResolveOrphanToolCalls learned to drop a
// stray result with no matching ToolCall in the immediately-preceding
// message, the wire ended up with the stray tool_result AND a second,
// synthesized one for the same id -- 1 tool_use, 2 tool_result.
func TestTranscodeOrphanStrayResultBeforeCall(t *testing.T) {
	out := mustTranscode(t, baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "callX", Content: message.Parts{&message.Text{Text: "stray"}}},
		}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolCall{CallID: "callX", Name: "bash", Arguments: json.RawMessage(`{}`)},
		}},
	))

	var toolUseCount, toolResultCount int
	for _, m := range out.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				if b.ID == "callX" {
					toolUseCount++
				}
			case "tool_result":
				if b.ToolUseID == "callX" {
					toolResultCount++
				}
			}
		}
	}
	if toolUseCount != 1 {
		t.Fatalf("tool_use count for callX = %d, want 1", toolUseCount)
	}
	if toolResultCount != 1 {
		t.Fatalf("tool_result count for callX = %d, want exactly 1 (the stray dropped, replaced by exactly one properly-paired result)", toolResultCount)
	}
	assertToolUseFollowedByResult(t, out, "callX")
}
