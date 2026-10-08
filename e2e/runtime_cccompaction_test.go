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

func TestContractRuntimeClaudeCodeCompactionFrames(t *testing.T) {
	skipShort(t)
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
	rows := []struct {
		name   string
		frames []map[string]any
		want   []string
	}{
		{"claude_code_compaction_sends_compacting_then_running_when_the_summary_is_appended",
			[]map[string]any{init, compacting, settled, boundary, summary, reply, result},
			[]string{"turn.started", "~status compacting", "compaction.applied", "~status running", "turn.ended"}},
		{"claude_code_compaction_with_a_failed_result_sends_compaction_failed",
			[]map[string]any{init, compacting, failed, reply, result},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}},
		{"claude_code_compaction_that_never_settles_sends_compaction_failed_before_the_turn_ends",
			[]map[string]any{init, compacting, reply, result},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}},
		{"claude_code_compaction_cut_off_by_the_end_of_the_stream_sends_compaction_failed",
			[]map[string]any{init, compacting},
			[]string{"turn.started", "~status compacting", "~status compaction_failed", "turn.ended"}},
		{"claude_code_boundary_without_a_compacting_status_sends_no_frame",
			[]map[string]any{init, boundary, summary, reply, result},
			[]string{"turn.started", "compaction.applied", "turn.ended"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			l := newCCLane(t, "", ccStream(t, row.frames...))
			r := l.runtime()
			s := l.create(r)
			var got []string
			head := s.View().HeadSeq
			for e, err := range s.Events(t.Context(), head-1) {
				if err != nil {
					t.Fatal(err)
				}
				if e.Seq == head && !e.Ephemeral {
					l.send(s, "1")
				}
				if line, ok := compactionLine(e); ok {
					got = append(got, line)
				}
				if e.Kind == "turn.ended" && !e.Ephemeral {
					break
				}
			}
			l.close(r)
			if !slices.Equal(got, row.want) {
				t.Errorf("events =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(row.want, "\n"))
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
