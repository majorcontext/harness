package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func provPage(t *testing.T, d *runtimeDriver, id string) []protocol.Message {
	t.Helper()
	var page protocol.MessagePage
	d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id+"/messages?limit=1000", nil, &page)
	return page.Messages
}

func provInput(id, text, source, sourceID, sourceLabel string) map[string]any {
	return map[string]any{"id": id, "parts": []protocol.Part{{Type: protocol.PartText, Text: text}},
		"source": source, "source_id": sourceID, "source_label": sourceLabel}
}

func errorCode(t *testing.T, res callResult) string {
	t.Helper()
	body, _ := bodyOf(t, res)["error"].(map[string]any)
	code, _ := body["code"].(string)
	return code
}

func userMessageWithText(t *testing.T, msgs []protocol.Message, text string) protocol.Message {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "user" && len(m.Parts) > 0 && strings.Contains(m.Parts[0].Text, text) {
			return m
		}
	}
	t.Fatalf("no user message holds %q: %+v", text, msgs)
	return protocol.Message{}
}

func TestContractInputProvenanceReadBack(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("input_provenance_reads_back_on_its_user_message", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("ok"))
			id := d.Create(t)
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in1", "from slack", "slack", "slack:C1:1.2", strings.Repeat("é", 200)), nil)
			d.WaitIdle(t, id)
			m := userMessageWithText(t, provPage(t, d, id), "from slack")
			if m.Source != "slack" || m.SourceID != "slack:C1:1.2" || m.SourceLabel != strings.Repeat("é", 128) || len(m.OperatorBatch) != 0 {
				t.Errorf("user message provenance = %+v", m)
			}
		})
	})
}

func TestContractInputProvenanceSteerBatch(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("steer_inputs_that_join_a_turn_read_as_one_message_with_an_entry_each", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("start"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo tool"}},
				}}},
				replyText("done"))
			id := d.Create(t)
			d.Submit(t, id, "start")
			if !fake.AwaitRequests(1, waitBound) {
				t.Fatal("the turn sent no model request")
			}
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_a", "alpha", "slack", "slack:C1:1", "Ann"), nil)
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_b", "beta", "api", "cron-7", "Nightly"), nil)
			fake.Release("call")
			d.WaitIdle(t, id)
			m := userMessageWithText(t, provPage(t, d, id), "OPERATOR MESSAGES")
			want := []protocol.OperatorBatchEntry{
				{ID: "in_a", Text: "alpha", Source: "slack", SourceID: "slack:C1:1", SourceLabel: "Ann"},
				{ID: "in_b", Text: "beta", Source: "api", SourceID: "cron-7", SourceLabel: "Nightly"},
			}
			if !slices.Equal(m.OperatorBatch, want) {
				t.Errorf("operator_batch = %+v, want %+v", m.OperatorBatch, want)
			}
			if m.Source != "" || m.SourceID != "" || m.SourceLabel != "" {
				t.Errorf("a joined message carries its provenance only in its entries: %+v", m)
			}
		})
	})
}

func TestContractInputProvenanceValidation(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("invalid_source_id_is_rejected_and_leaves_no_trace", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("ok"))
			id := d.Create(t)
			before := len(d.events(t, id))
			for name, in := range map[string][2]string{
				"over_128_bytes": {"text", strings.Repeat("x", 129)},
				"control_byte":   {"text", "a\x01b"},
				"typed_command":  {"/compact", "a\x01b"},
			} {
				res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_"+name, in[0], "typed", in[1], ""))
				if res.Status != http.StatusBadRequest || errorCode(t, res) != "invalid_request" {
					t.Errorf("%s: response = %d %v, want 400 invalid_request", name, res.Status, res.Body)
				}
			}
			if after := len(d.events(t, id)); after != before {
				t.Errorf("a rejected input appended %d records", after-before)
			}
			if msgs := provPage(t, d, id); len(msgs) != 0 {
				t.Errorf("a rejected input left messages: %+v", msgs)
			}
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in_ok", "text", "api", strings.Repeat("x", 128), ""), nil)
		})
		t.Run("a_repeat_with_another_source_id_conflicts", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("ok"))
			id := d.Create(t)
			path := "/sessions/" + id + "/inputs"
			d.expect(t, http.StatusCreated, http.MethodPost, path, provInput("in1", "text", "slack", "slack:C1:1", "Ann"), nil)
			d.expect(t, http.StatusOK, http.MethodPost, path, provInput("in1", "text", "slack", "slack:C1:1", "Ann"), nil)
			for name, body := range map[string]map[string]any{
				"source_id":    provInput("in1", "text", "slack", "slack:C1:2", "Ann"),
				"source_label": provInput("in1", "text", "slack", "slack:C1:1", "Bob"),
			} {
				res := d.call(t, http.MethodPost, path, body)
				if res.Status != http.StatusConflict || errorCode(t, res) != "input_conflict" {
					t.Errorf("repeat with another %s = %d %v, want 409 input_conflict", name, res.Status, res.Body)
				}
			}
		})
	})
}

func TestContractInputProvenancePromptCommand(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_prompt_command_reads_back_with_source_command_and_the_typed_line_as_label", func(t *testing.T) {
			t.Parallel()
			files := map[string]string{".agents/commands/review.md": reviewCommand}
			d, _ := startOn(t, h, runtimeWorkdir(t, files), nil, replyText("ok"))
			id := d.Create(t)
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", provInput("in1", "/review main", "typed", "web:abc", "andy@example.com"), nil)
			d.WaitIdle(t, id)
			m := userMessageWithText(t, provPage(t, d, id), "Review main now.")
			if m.Source != "command" || m.SourceID != "web:abc" || m.SourceLabel != "/review main" {
				t.Errorf("prompt command message provenance = %+v", m)
			}
		})
	})
}

func TestContractInputProvenanceChildReport(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_child_report_to_an_idle_parent_reads_as_a_user_message_from_child", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "general-purpose", "prompt": "child work"},
				}}}},
				harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child finished", Block: true}},
				harnesstest.Step{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				harnesstest.Step{Name: "parent", Match: harnesstest.LastUserText("A background task"), Reply: harnesstest.Reply{Text: "parent done"}},
			)
			id := d.Create(t)
			d.Submit(t, id, "delegate")
			if !fake.AwaitRequests(3, waitBound) {
				t.Fatal("the parent and the child sent fewer than three model requests")
			}
			d.WaitIdle(t, id)
			fake.Release("child")
			if !fake.AwaitRequests(4, waitBound) {
				t.Fatal("the report started no parent turn")
			}
			d.WaitIdle(t, id)
			m := userMessageWithText(t, provPage(t, d, id), "A background task you started has finished")
			if m.Source != "child" {
				t.Errorf("the report message has source %q, want child: %+v", m.Source, m)
			}
		})
	})
}
