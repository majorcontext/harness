package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestEmptyToolOutputNeverAppendsNullContent reproduces the second root
// cause folded into NEP-5272: a tool that runs successfully and produces
// truly empty output (mirroring box hyper-lemon's `grep ... | head -20`
// that matched nothing) must never leave a ToolResult in history whose
// Content collapses to nothing on the wire. Session.append's call to
// Message.Normalize (see message.ToolResult.SafeContent's doc comment) is
// the fix under test here; this exercises it through the real engine loop
// rather than calling Normalize directly.
func TestEmptyToolOutputNeverAppendsNullContent(t *testing.T) {
	emptyOutputTool := Tool{
		Def: provider.ToolDef{Name: "grep"},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			// The exact shape bash.go's captured-output path leaves behind
			// for a command with no stdout/stderr: a single blank Text
			// part, not a nil/empty Parts.
			return message.Parts{&message.Text{Text: ""}}, nil
		},
	}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "grep", `{"pattern":"nope"}`)),
		asstTurn(provider.StopEndTurn, &message.Text{Text: "done"}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		Tools:     []Tool{emptyOutputTool},
	})

	if _, err := s.Prompt(context.Background(), "search for nope"); err != nil {
		t.Fatalf("Prompt = %v, want success", err)
	}

	h := s.History()
	if len(h) != 4 || h[2].Role != message.RoleTool {
		t.Fatalf("history = %+v, want [user, assistant, tool, assistant]", h)
	}
	tr, ok := h[2].Parts[0].(*message.ToolResult)
	if !ok {
		t.Fatalf("h[2].Parts[0] = %T, want *message.ToolResult", h[2].Parts[0])
	}
	if len(tr.Content) == 0 || tr.Content.Text() == "" {
		t.Fatalf("ToolResult.Content = %+v, want a non-empty marker (Normalize must have filled it)", tr.Content)
	}

	// The whole point: the persisted/serialized form must never carry a
	// literal null (or otherwise empty) content, since that is exactly
	// what wedged box hyper-lemon despite the request being internally
	// balanced (44 tool_use, 44 tool_result, every pair adjacent).
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal(History()) = %v, want success", err)
	}
	if strings.Contains(string(raw), `"content":null`) {
		t.Fatalf("history marshals with literal null tool_result content: %s", raw)
	}
}
