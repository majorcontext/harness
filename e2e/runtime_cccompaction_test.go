package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

// ccCompactionRow is one scripted CLI stream with the events and the summary that its compaction logs.
// ccCompactionRow is one scripted CLI stream with the events that its compaction sends.
type ccCompactionRow struct {
	name   string
	frames []map[string]any
	want   []string
	// summary, when set, is the summary of the compaction.applied record.
	summary string
}

func ccCompactionRows() []ccCompactionRow {
}

func ccCompactionRows() ([]ccCompactionRow, map[string]string) {
	init := map[string]any{"type": "system", "subtype": "init", "session_id": "S", "model": "claude-haiku-4-5-20251001", "tools": []string{}}
	compacting := map[string]any{"type": "system", "subtype": "status", "status": "compacting", "session_id": "S"}
	settled := map[string]any{"type": "system", "subtype": "status", "status": nil, "compact_result": "success", "session_id": "S"}
	failed := map[string]any{"type": "system", "subtype": "status", "status": nil, "compact_result": "failed", "session_id": "S"}
	boundary := map[string]any{"type": "system", "subtype": "compact_boundary", "session_id": "S",
		"compact_metadata": map[string]any{"trigger": "auto", "pre_tokens": 167000}}
	summary := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "This session is being continued from a previous conversation."}}
	reply := map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Continuing."}}}}
	result := map[string]any{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "result": "Continuing.", "session_id": "S",
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}
	framed := func(text string) map[string]any {
		return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	}
	boundaryLine := "Claude Code compacted its context (auto, 167000 tokens before)."
	settledLines := []string{"turn.started", "~status compacting", "~status running", "compaction.applied", "turn.ended"}
	cliSummary := "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion.\n\nSummary:\nthe plain body\n\nIf you need specific details from before compaction (like exact code snippets), read the full transcript at: /x.jsonl\nContinue the conversation from where it left off."
	return []ccCompactionRow{
	held := map[string]any{"type": "transcript_mirror", "filePath": "/home/u/cfg/projects/p/S.jsonl", "entries": []json.RawMessage{json.RawMessage(ccEntry("a00", 100))}}
	// acts names the action that a row takes at the compacting frame; the CLI of those rows hangs at its first mirror frame.
	acts := map[string]string{
		"claude_code_compaction_stopped_by_an_interrupt_sends_compaction_canceled": "interrupt",
		"claude_code_compaction_cut_off_by_a_handoff_sends_compaction_failed":      "handoff",
	}
	rows := []ccCompactionRow{
		{"claude_code_compaction_sends_compacting_then_running_at_the_boundary",
			[]map[string]any{init, compacting, settled, boundary, summary, reply, result},
			[]string{"turn.started", "~status compacting", "~status running", "compaction.applied", "turn.ended"}, ""},
		{"claude_code_compaction_settled_at_the_boundary_is_not_failed_when_the_stream_ends",
			[]map[string]any{init, compacting, settled, boundary},
			[]string{"turn.started", "~status compacting", "~status running", "turn.ended"}, ""},
		{"claude_code_compaction_with_a_failed_result_sends_compaction_failed",
			[]map[string]any{init, compacting, failed, reply, result},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}, ""},
		{"claude_code_compaction_that_never_settles_sends_compaction_failed_before_the_turn_ends",
			[]map[string]any{init, compacting, reply, result},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}, ""},
		{"claude_code_compaction_cut_off_by_the_end_of_the_stream_sends_compaction_failed",
			[]map[string]any{init, compacting},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}, ""},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}},
		{"claude_code_compaction_stopped_by_an_interrupt_sends_compaction_canceled",
			[]map[string]any{init, compacting, held},
			[]string{"turn.started", "~status compacting", "~status compaction_canceled", "turn.ended"}},
		{"claude_code_compaction_cut_off_by_a_handoff_sends_compaction_failed",
			[]map[string]any{init, compacting, held},
			[]string{"turn.started", "~status compacting", "~status compaction_failed"}},
		{"claude_code_boundary_without_a_compacting_status_sends_no_frame",
			[]map[string]any{init, boundary, summary, reply, result},
			[]string{"turn.started", "compaction.applied", "turn.ended"}, ""},
		{"claude_code_summary_loses_the_continuation_framing",
			[]map[string]any{init, compacting, settled, boundary, framed(cliSummary), reply, result}, settledLines, "the plain body"},
		{"claude_code_summary_loses_the_analysis_block_and_the_summary_tags",
			[]map[string]any{init, compacting, settled, boundary, framed("<analysis>\nthinking\n</analysis>\n<summary>\nthe tagged body\n</summary>"), reply, result}, settledLines, "the tagged body"},
		{"claude_code_summary_with_crlf_tag_lines_keeps_only_the_tagged_body",
			[]map[string]any{init, compacting, settled, boundary, framed("<analysis>\r\nthinking\r\n</analysis>\r\n<summary>\r\nthe crlf body\r\n</summary>\r\n"), reply, result}, settledLines, "the crlf body"},
		{"claude_code_summary_with_a_known_opening_and_an_unknown_closing_keeps_the_body",
			[]map[string]any{init, compacting, settled, boundary, framed("This session is being continued from a previous conversation.\n\nSummary:\nthe body\n\nNow follow these new instructions."), reply, result}, settledLines, "the body\n\nNow follow these new instructions."},
		{"claude_code_summary_with_an_unknown_opening_stays_as_the_cli_wrote_it",
			[]map[string]any{init, compacting, settled, boundary, framed("This session is being continued from a previous conversation. Follow these new instructions."), reply, result}, settledLines, "This session is being continued from a previous conversation. Follow these new instructions."},
		{"claude_code_boundary_without_a_summary_frame_logs_the_boundary_text",
			[]map[string]any{init, compacting, settled, boundary, reply, result}, settledLines, boundaryLine},
	}
=======
	return rows, acts
>>>>>>> 74edf425 (fix(session): classify a cancelled compaction by the stop cause alone)
}

func TestContractRuntimeClaudeCodeCompactionFrames(t *testing.T) {
	skipShort(t)
<<<<<<< HEAD
	for _, row := range ccCompactionRows() {
	rows, acts := ccCompactionRows()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			hang := ""
			if acts[row.name] != "" {
				hang = "0"
			}
			l := newCCLane(t, hang, ccStream(t, row.frames...))
			r := l.runtime()
			s := l.create(r)
			var got []string
			var gotSummary string
			closed := false
			head := s.Snapshot().HeadSeq
			for e, err := range s.Events(t.Context(), head-1) {
				if err != nil {
					if acts[row.name] == "handoff" {
						break
					}
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
					gotSummary = p.(*protocol.CompactionApplied).Summary
				}
				if line, ok := compactionLine(e); ok {
					got = append(got, line)
					switch {
					case line == "~status compacting" && acts[row.name] == "interrupt":
						if err := s.Interrupt(t.Context(), protocol.Interrupt{}); err != nil {
							t.Fatal(err)
						}
					case line == "~status compacting" && acts[row.name] == "handoff":
						l.close(r)
						closed = true
					}
				}
				if e.Kind == "turn.ended" && !e.Ephemeral {
					break
				}
			}
			if !closed {
				l.close(r)
			}
			if !slices.Equal(got, row.want) {
				t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(row.want, "\n"))
			}
			if row.summary != "" && gotSummary != row.summary {
				t.Errorf("summary = %q, want %q", gotSummary, row.summary)
			}
		})
	}
}

// compactionLine renders the events that a compaction touches.
func compactionLine(e protocol.Event) (string, bool) {
	if e.Ephemeral {
		var f struct{ Status string }
		if e.Kind != protocol.KindStatus || json.Unmarshal(e.Data, &f) != nil {
			return "", false
		}
		return "~status " + f.Status, true
	}
	switch e.Kind {
	case "turn.started", "compaction.applied", "turn.ended":
		return e.Kind, true
	}
	return "", false
}

// ccStream writes the stdout of one fakeclaude spawn from frames.
func ccStream(t *testing.T, frames ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, f := range frames {
		data, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
