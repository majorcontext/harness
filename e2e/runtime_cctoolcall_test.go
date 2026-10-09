package e2e

import (
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/protocol"
)

func TestContractRuntimeClaudeCodeToolCallVisible(t *testing.T) {
	skipShort(t)
	t.Run("claude_code_running_tool_call_is_in_the_messages_before_its_result", func(t *testing.T) {
		l := newCCLane(t, "")
		t.Setenv("FAKE_CLAUDE_MODE", "hang_in_tool")
		r := l.runtime()
		s := l.create(r)
		l.send(s, "1")
		l.awaitText(s, "Checking.")
		testpoll.Until(t, 10*time.Second, "the messages never held the call toolu_h while the CLI ran the tool", func() bool {
			v, err := harness.OpenView(t.Context(), l.st, "s1")
			if err != nil {
				t.Fatal(err)
			}
			page, err := v.Messages(t.Context(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var calls, results int
			for _, m := range page.Messages {
				for _, p := range m.Parts {
					switch {
					case p.Type == protocol.MessagePartToolCall && p.CallID == "toolu_h":
						calls++
					case p.Type == protocol.MessagePartToolResult:
						results++
					}
				}
			}
			return calls == 1 && results == 0
		})
		l.close(r)
	})
}
