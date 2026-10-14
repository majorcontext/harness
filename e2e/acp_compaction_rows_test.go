package e2e

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// acpCompactionSteps folds the first turn with /compact, then runs check. With load set it closes and loads the session before check, so the replay holds the summary.
func acpCompactionSteps(init acpInit, load bool, check acpCheck) []acpStep {
	steps := []acpStep{
		init,
		acpNew{as: "a"},
		acpPrompt{as: "a", text: "one", stop: "end_turn"},
		acpPrompt{as: "a", text: "two", stop: "end_turn"},
		acpPrompt{as: "a", text: "/compact", stop: "end_turn"},
	}
	if load {
		steps = append(steps, acpClose{as: "a"}, acpLoad{as: "a"})
	}
	return append(steps, check)
}

func acpUpdateBody(f acpFrame) map[string]any {
	var m struct {
		Params struct {
			Update map[string]any `json:"update"`
		} `json:"params"`
	}
	_ = json.Unmarshal(f.Frame, &m)
	return m.Params.Update
}

// acpUpdatesFrom returns the updates of kind that the agent sent on the wire, or only those after the session/load request when replay is set.
func acpUpdatesFrom(c *acpConn, kind string, replay bool) []map[string]any {
	wire := c.wire
	if replay {
		split := slices.IndexFunc(wire, func(f acpFrame) bool {
			return f.Dir == "client" && strings.Contains(string(f.Frame), `"method":"session/load"`)
		})
		wire = wire[split:]
	}
	var out []map[string]any
	for _, f := range wire {
		if updateKind(f) == kind {
			out = append(out, acpUpdateBody(f))
		}
	}
	return out
}

func acpSummaryText(u map[string]any) string {
	var b strings.Builder
	blocks, _ := u["summary"].([]any)
	for _, blk := range blocks {
		m, _ := blk.(map[string]any)
		text, _ := m["text"].(string)
		b.WriteString(text)
	}
	return b.String()
}

func acpUserText(c *acpConn, replay bool) string {
	var b strings.Builder
	for _, u := range acpUpdatesFrom(c, "user_message_chunk", replay) {
		content, _ := u["content"].(map[string]any)
		text, _ := content["text"].(string)
		b.WriteString(text)
	}
	return b.String()
}

var acpCompactionScenarios = []acpScenario{
	{
		name:   "a_compaction_sends_the_entity_lifecycle_when_the_client_has_the_capability",
		cite:   "A `status` frame with `compacting` sends a `compaction_update` with the status `in_progress`.",
		model:  acpRepeatText("summary-marker"),
		config: map[string]any{"compaction_keep_turns": 1},
		steps: acpCompactionSteps(acpInit{version: 1, want: 1, compaction: true}, false, func(t *testing.T, c *acpConn) {
			var order []string
			for _, f := range c.wire {
				switch updateKind(f) {
				case "compaction_update":
					status, _ := acpUpdateBody(f)["status"].(string)
					order = append(order, "update:"+status)
				case "compaction_summary_chunk":
					order = append(order, "chunk")
				}
			}
			if want := []string{"update:in_progress", "chunk", "update:completed"}; !slices.Equal(order, want) {
				t.Fatalf("compaction frames = %v, want %v", order, want)
			}
			updates := acpUpdatesOf(c, "compaction_update")
			chunks := acpUpdatesOf(c, "compaction_summary_chunk")
			if id, _ := updates[0]["compactionId"].(string); id == "" || id != updates[1]["compactionId"] || id != chunks[0]["compactionId"] {
				t.Errorf("compaction IDs differ: %v %v", updates, chunks)
			}
			content, _ := chunks[0]["content"].(map[string]any)
			if text, _ := content["text"].(string); content["type"] != "text" || !strings.Contains(text, "summary-marker") {
				t.Errorf("summary chunk = %v, want a text block with the summary", chunks[0])
			}
			if len(acpUpdatesOf(c, "usage_update")) == 0 {
				t.Error("no usage_update")
			}
		}),
	},
	{
		name:   "a_compaction_sends_no_compaction_frame_without_the_capability",
		cite:   "Without the capability, the agent sends no compaction update and no compaction message.",
		model:  acpRepeatText("summary-marker"),
		config: map[string]any{"compaction_keep_turns": 1},
		steps: acpCompactionSteps(acpInit{version: 1, want: 1}, false, func(t *testing.T, c *acpConn) {
			for _, kind := range []string{"compaction_update", "compaction_summary_chunk"} {
				if got := acpUpdatesOf(c, kind); len(got) != 0 {
					t.Errorf("%s frames = %v, want none", kind, got)
				}
			}
			if len(acpUpdatesOf(c, "usage_update")) == 0 {
				t.Error("no usage_update")
			}
		}),
	},
	{
		name:   "replay_sends_the_summary_as_a_completed_compaction_with_the_capability",
		cite:   "The replay sends the summary message of a compaction as one `compaction_update` with the status `completed`, the ID of the summary message, and the summary text as one `text` block in `summary`, at the place of the message in the conversation.",
		model:  acpRepeatText("summary-marker"),
		config: map[string]any{"compaction_keep_turns": 1},
		steps: acpCompactionSteps(acpInit{version: 1, want: 1, compaction: true}, true, func(t *testing.T, c *acpConn) {
			got := acpUpdatesFrom(c, "compaction_update", true)
			if len(got) != 1 || got[0]["status"] != "completed" || !strings.HasPrefix(got[0]["compactionId"].(string), "cmpsum_") || !strings.Contains(acpSummaryText(got[0]), "summary-marker") {
				t.Fatalf("replayed compaction updates = %v, want one completed entity with the summary", got)
			}
			if chunks := acpUpdatesFrom(c, "compaction_summary_chunk", true); len(chunks) != 0 {
				t.Errorf("replay sent summary chunks %v, want none", chunks)
			}
			if text := acpUserText(c, true); strings.Contains(text, "summary-marker") {
				t.Errorf("the replay sent the summary as user text: %q", text)
			}
		}),
	},
	{
		name:   "replay_skips_the_summary_without_the_capability",
		cite:   "Without the capability, the replay sends nothing for it.",
		model:  acpRepeatText("summary-marker"),
		config: map[string]any{"compaction_keep_turns": 1},
		steps: acpCompactionSteps(acpInit{version: 1, want: 1}, true, func(t *testing.T, c *acpConn) {
			if text := acpUserText(c, true); strings.Contains(text, "summary-marker") || !strings.Contains(text, "two") {
				t.Errorf("replayed user text = %q, want the turns with no summary", text)
			}
			for _, kind := range []string{"compaction_update", "compaction_summary_chunk"} {
				if got := acpUpdatesFrom(c, kind, true); len(got) != 0 {
					t.Errorf("replay sent %s frames %v, want none", kind, got)
				}
			}
		}),
	},
	{
		name: "a_cancelled_compaction_ends_the_entity_cancelled",
		cite: "A `status` frame with `compaction_canceled` sends one with the status `cancelled`.",
		model: []harnesstest.Step{
			{Name: "one", Match: harnesstest.LastUserText("one"), Reply: harnesstest.Reply{Text: "ok"}},
			{Name: "two", Match: harnesstest.LastUserText("two"), Reply: harnesstest.Reply{Text: "ok", Usage: harnesstest.Usage{Input: 900, Output: 1}}},
			{Name: "summary", Match: harnesstest.SystemContains("You are summarizing a prefix"), Reply: harnesstest.Reply{Text: "gist", Block: true}},
			{Name: "three", Match: harnesstest.LastUserText("three"), Reply: harnesstest.Reply{Text: "ok"}},
		},
		config: map[string]any{"context_window_tokens": 1000, "compaction_keep_turns": 1},
		steps: []acpStep{
			acpInit{version: 1, want: 1, compaction: true},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpPrompt{as: "a", text: "two", stop: "end_turn"},
			acpPromptStart{as: "a", id: "three", text: "three"},
			acpAwaitUpdate{kind: "compaction_update", status: "in_progress"},
			acpPromptStart{as: "a", id: "abort", text: "/abort"},
			acpResult{id: "abort", stop: "end_turn"},
			acpResult{id: "three", stop: "end_turn"},
			acpAwaitUpdate{kind: "compaction_update", status: "cancelled"},
			acpCheck(func(t *testing.T, c *acpConn) {
				var statuses []any
				for _, u := range acpUpdatesOf(c, "compaction_update") {
					statuses = append(statuses, u["status"])
				}
				if !slices.Equal(statuses, []any{"in_progress", "cancelled"}) {
					t.Errorf("compaction statuses = %v, want in_progress then cancelled", statuses)
				}
				if got := acpUpdatesOf(c, "compaction_summary_chunk"); len(got) != 0 {
					t.Errorf("summary chunks = %v, want none", got)
				}
			}),
		},
	},
	{
		name: "a_claude_code_compaction_in_a_turn_sends_the_entity_lifecycle",
		cite: "A frame with `idle` or `running` ends no compaction, because the summary of a compaction of the Claude Code CLI follows its `running` frame.",
		login: &acpLoginSpec{claude: true, claudeFrames: []map[string]any{
			{"type": "system", "subtype": "init", "session_id": "S", "model": "claude-haiku-4-5-20251001", "tools": []string{}},
			{"type": "system", "subtype": "status", "status": "compacting", "session_id": "S"},
			{"type": "system", "subtype": "status", "status": nil, "compact_result": "success", "session_id": "S"},
			{"type": "system", "subtype": "compact_boundary", "session_id": "S", "compact_metadata": map[string]any{"trigger": "auto", "pre_tokens": 167000}},
			{"type": "user", "message": map[string]any{"role": "user", "content": "cli-summary-marker"}},
			{"type": "assistant", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Continuing."}}}},
			{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "result": "Continuing.", "session_id": "S", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}},
		}},
		steps: []acpStep{
			acpInit{version: 1, want: 1, compaction: true, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "go", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				var order []string
				for _, f := range c.wire {
					switch updateKind(f) {
					case "compaction_update":
						status, _ := acpUpdateBody(f)["status"].(string)
						order = append(order, "update:"+status)
					case "compaction_summary_chunk":
						order = append(order, "chunk")
					}
				}
				if want := []string{"update:in_progress", "chunk", "update:completed"}; !slices.Equal(order, want) {
					t.Fatalf("compaction frames = %v, want %v", order, want)
				}
				chunk := acpUpdatesOf(c, "compaction_summary_chunk")[0]
				if content, _ := chunk["content"].(map[string]any); !strings.Contains(content["text"].(string), "cli-summary-marker") {
					t.Errorf("summary chunk = %v", chunk)
				}
			}),
		},
	},
}
