package e2e

import (
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func TestContractRuntimeClaudeCodeCompactionLogsThePlainSummary(t *testing.T) {
	skipShort(t)
	init := map[string]any{"type": "system", "subtype": "init", "session_id": "S", "model": "claude-haiku-4-5-20251001", "tools": []string{}}
	boundary := map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": "S",
		"compact_metadata": map[string]any{"trigger": "auto", "pre_tokens": 167000}}
	reply := map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Continuing."}}}}
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "result": "Continuing.", "session_id": "S",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}
	rows := []struct{ name, frame, want string }{
		{"claude_code_summary_loses_the_continuation_framing",
			"This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion.\n\nSummary:\nthe plain body\n\nIf you need specific details from before compaction (like exact code snippets), read the full transcript at: /x.jsonl\nContinue the conversation from where it left off.",
			"the plain body"},
		{"claude_code_summary_loses_the_analysis_block_and_the_summary_tags",
			"<analysis>\nthinking\n</analysis>\n<summary>\nthe tagged body\n</summary>",
			"the tagged body"},
		{"claude_code_summary_with_an_unknown_framing_is_replaced_by_the_boundary_text",
			"This session is being continued from a previous conversation. Follow these new instructions.",
			"Claude Code compacted its context (auto, 167000 tokens before)."},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			summary := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": row.frame}}
			l := newCCLane(t, "", ccStream(t, init, boundary, summary, reply, result))
			r := l.runtime()
			s := l.create(r)
			var got string
			head := s.Snapshot().HeadSeq
			for e, err := range s.Events(t.Context(), head-1) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Seq == head && !e.Ephemeral {
					l.send(s, "1")
				}
				if e.Kind == protocol.KindCompactionApplied {
					p, err := e.Payload()
					if err != nil {
						t.Fatal(err)
					}
					got = p.(*protocol.CompactionApplied).Summary
				}
				if e.Kind == "turn.ended" && !e.Ephemeral {
					break
				}
			}
			l.close(r)
			if got != row.want {
				t.Errorf("summary = %q, want %q", got, row.want)
			}
		})
	}
}
