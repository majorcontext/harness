package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestEffortSetsReasoning: a non-off effort level sets reasoning.effort to the
// same string, drops temperature/top_p (reasoning models reject them), and
// raises max_output_tokens above the floor (reasoning tokens count against it).
func TestEffortSetsReasoning(t *testing.T) {
	temp := 0.7
	topP := 0.9
	// LITERAL expected floors (an oracle, NOT a call to reasoningOutputFloor):
	// caller MaxTokens 8192 is below every floor, so each level is raised to its
	// floor (minimal's floor is 10000, the lowest — 8192 is raised to it).
	wantFloor := map[message.Effort]int{
		message.EffortMinimal: 10000,
		message.EffortLow:     12000,
		message.EffortMedium:  18000,
		message.EffortHigh:    25000,
	}
	for _, e := range []message.Effort{message.EffortMinimal, message.EffortLow, message.EffortMedium, message.EffortHigh} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Temperature = &temp
		req.TopP = &topP
		req.MaxTokens = 8192
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Reasoning == nil {
			t.Fatalf("effort %q: reasoning absent", e)
		}
		if out.Reasoning.Effort != string(e) {
			t.Errorf("effort %q: reasoning.effort = %q", e, out.Reasoning.Effort)
		}
		if out.Temperature != nil {
			t.Errorf("effort %q: temperature must be dropped with reasoning on", e)
		}
		if out.TopP != nil {
			t.Errorf("effort %q: top_p must be dropped with reasoning on", e)
		}
		if out.MaxOutputTokens != wantFloor[e] {
			t.Errorf("effort %q: max_output_tokens = %d, want %d", e, out.MaxOutputTokens, wantFloor[e])
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"reasoning":{"effort":"`+string(e)+`"}`) {
			t.Errorf("effort %q: wire missing reasoning: %s", e, raw)
		}
	}
}

// TestEffortKeepsLargeMaxOutput: a caller's max_output_tokens already above the
// floor is preserved, not shrunk.
func TestEffortKeepsLargeMaxOutput(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.MaxTokens = 64000
	req.Effort = message.EffortHigh
	out := mustTranscode(t, req)
	if out.MaxOutputTokens != 64000 {
		t.Errorf("max_output_tokens = %d, want 64000 (kept)", out.MaxOutputTokens)
	}
}

// TestNoEffortKeepsSampling: with no reasoning, temperature/top_p pass through
// unchanged (the fix must not touch the non-reasoning path).
func TestNoEffortKeepsSampling(t *testing.T) {
	temp := 0.7
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.Temperature = &temp
	req.Effort = message.EffortOff
	out := mustTranscode(t, req)
	if out.Temperature == nil {
		t.Error("temperature must be preserved when reasoning is off")
	}
}

// TestReasoningStrippedWhenOff: a stored reasoning item (from an earlier
// reasoning-ON turn) is STRIPPED only on an EXPLICIT EffortOff, and REPLAYED
// verbatim on the default EffortUnset. The two are DELIBERATELY asymmetric.
// gpt-5 reasons by default, so an unset (default) session must keep the stored
// items its stateless multi-turn tool use requires; only an explicit "reasoning
// off" intent drops them (symmetric with the anthropic thinking-block strip:
// shipping a stored item while the request omits `reasoning` is rejected).
// (Regression: NEP-5272 review of PR #117 — the prior loop asserted stripping
// on BOTH EffortUnset and EffortOff, codifying the default-session data loss.)
func TestReasoningStrippedWhenOff(t *testing.T) {
	history := func() []message.Message {
		return []message.Message{
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
			{Role: message.RoleAssistant, Parts: message.Parts{
				&message.Reasoning{Text: "hmm", ProviderData: message.ProviderData{
					Family: json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"ENC"}`),
				}},
				&message.Text{Text: "answer"},
			}},
			{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "again"}}},
		}
	}

	// EffortOff: an explicit "reasoning disabled" intent strips the stored item.
	{
		req := baseRequest(history()...)
		req.Effort = message.EffortOff
		out := mustTranscode(t, req)
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"type":"reasoning"`) || strings.Contains(string(raw), `rs_1`) {
			t.Errorf("EffortOff: wire carries a stored reasoning item, want it stripped:\n%s", raw)
		}
		if !strings.Contains(string(raw), "answer") {
			t.Error("EffortOff: assistant text lost with the stripped reasoning")
		}
	}

	// EffortUnset (the default of every harness run/serve session): reasoning is
	// not disabled, only uncontrolled — gpt-5 reasons by default, so the stored
	// item MUST be replayed, exactly as every pre-effort-control build did.
	{
		req := baseRequest(history()...)
		req.Effort = message.EffortUnset
		out := mustTranscode(t, req)
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"type":"reasoning"`) || !strings.Contains(string(raw), `rs_1`) {
			t.Errorf("EffortUnset: stored reasoning item dropped, want it replayed (gpt-5 reasons by default):\n%s", raw)
		}
		if !strings.Contains(string(raw), "answer") {
			t.Error("EffortUnset: assistant text lost")
		}
	}
}

// TestReasoningReplayedOnUnsetToolContinuation is the NEP-5272 regression guard
// for the gpt-5 tool-continuation shape. A multi-turn TOOL continuation at the
// DEFAULT (EffortUnset) effort — nothing sets req.Effort, exactly as every
// harness run/serve session — MUST replay the stored encrypted reasoning item.
// OpenAI reasoning models run Store:false (stateless) and REQUIRE the reasoning
// item that precedes a function_call to be replayed on the continuation
// request; dropping it wedges every turn-2+ tool call. Red-verify: with the
// buggy !reasoningEnabled gate the item is stripped on the default session;
// with the EffortOff-only strip it is replayed verbatim, before the
// function_call.
func TestReasoningReplayedOnUnsetToolContinuation(t *testing.T) {
	rawReasoning := json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"ENC"}`)
	req := baseRequest(
		message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "list files"}}},
		message.Message{Role: message.RoleAssistant, Parts: message.Parts{
			&message.Reasoning{Text: "plan", ProviderData: message.ProviderData{Family: rawReasoning}},
			&message.ToolCall{CallID: "call_ls", Name: "bash", Arguments: json.RawMessage(`{"command":"ls"}`)},
		}},
		message.Message{Role: message.RoleTool, Parts: message.Parts{
			&message.ToolResult{CallID: "call_ls", Content: message.Parts{&message.Text{Text: "main.go"}}},
		}},
	)
	// DEFAULT effort: req.Effort stays EffortUnset, as a plain harness session.
	out := mustTranscode(t, req)

	// No reasoning control is sent at unset, but the stored item is replayed.
	if out.Reasoning != nil {
		t.Errorf("EffortUnset must send no reasoning control, got %+v", out.Reasoning)
	}
	reasoningIdx, callIdx := -1, -1
	for i, item := range out.Input {
		switch probeItem(t, item).Type {
		case "reasoning":
			reasoningIdx = i
			if !jsonEqual(t, item, rawReasoning) {
				t.Errorf("reasoning item not replayed verbatim:\n got %s\nwant %s", item, rawReasoning)
			}
		case "function_call":
			callIdx = i
		}
	}
	if reasoningIdx < 0 {
		raw, _ := json.Marshal(out)
		t.Fatalf("stored reasoning item dropped at default effort, want it replayed:\n%s", raw)
	}
	if callIdx < 0 {
		t.Fatal("function_call item missing")
	}
	if reasoningIdx > callIdx {
		t.Errorf("reasoning item at index %d must precede function_call at index %d", reasoningIdx, callIdx)
	}
}

// TestEffortOffAndUnsetNoReasoning: EffortOff and EffortUnset send no reasoning
// control at all (omitempty drops the field).
func TestEffortOffAndUnsetNoReasoning(t *testing.T) {
	for _, e := range []message.Effort{message.EffortUnset, message.EffortOff} {
		req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
		req.Effort = e
		out := mustTranscode(t, req)
		if out.Reasoning != nil {
			t.Errorf("effort %q: reasoning must be absent", e)
		}
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "reasoning\":{") {
			t.Errorf("effort %q: wire must omit reasoning object: %s", e, raw)
		}
	}
}
