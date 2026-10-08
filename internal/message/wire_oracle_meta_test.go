package message

import (
	"strings"
	"testing"
)

// Meta-tests proving the oracle in wire_oracle_test.go can actually fail:
// an oracle that always passes is worthless. One test per invariant, feeding
// a known-bad wire shape and asserting checkWire (or checkNoDataLoss) flags
// it.

// --- Meta-tests: one per invariant, proving checkWire/checkNoDataLoss can fail ---

// TestOracleMetaFlagsUnansweredToolUse (invariant 1) feeds a tool_use with
// no tool_result anywhere near it and asserts the oracle flags it.
func TestOracleMetaFlagsUnansweredToolUse(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleUser, Parts: Parts{&Text{Text: "next turn, no result"}}},
	}
	v := checkWire(in)
	if !hasInvariant(v, "tool-use-unanswered") {
		t.Fatalf("checkWire did not flag an unanswered tool_use: %v", v)
	}
}

// TestOracleMetaFlagsToolResultInAssistantBlock (invariant 2) feeds a
// ToolResult sitting inside a RoleAssistant message and asserts the oracle
// flags it.
func TestOracleMetaFlagsToolResultInAssistantBlock(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}},
		}},
	}
	v := checkWire(in)
	if !hasInvariant(v, "no-tool-result-in-assistant-block") {
		t.Fatalf("checkWire did not flag a tool_result in an assistant-role block: %v", v)
	}
}

// TestOracleMetaFlagsToolUseInNonAssistantBlock (invariant 2, the
// tool_use/non-assistant half) feeds a ToolCall sitting inside a RoleTool
// message and asserts the oracle flags it. Anthropic maps message.RoleTool
// to a wire "user" turn (internal/provider/anthropic/transcode.go) and emits a
// tool_use block for any ToolCall regardless of the enclosing role, which
// the API rejects with HTTP 400 on a non-assistant turn — the symmetric
// partner of TestOracleMetaFlagsToolResultInAssistantBlock above.
func TestOracleMetaFlagsToolUseInNonAssistantBlock(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}},
		}},
	}
	v := checkWire(in)
	if !hasInvariant(v, "no-tool-use-in-non-assistant-block") {
		t.Fatalf("checkWire did not flag a tool_use in a non-assistant-role block: %v", v)
	}
}

// TestOracleMetaFlagsEmptyToolResultContent (invariant 3) feeds a
// tool_result whose only content is a blank Text part — the exact shape
// SafeContent's doc comment describes as read by the provider as ABSENT —
// and asserts the oracle flags it.
func TestOracleMetaFlagsEmptyToolResultContent(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: ""}}}}},
	}
	v := checkWire(in)
	if !hasInvariant(v, "no-empty-tool-result-content") {
		t.Fatalf("checkWire did not flag an empty-content tool_result: %v", v)
	}
}

// TestOracleMetaFlagsSurplusToolResult (invariant 4, surplus direction)
// feeds one tool_use answered by TWO tool_results sharing its id and
// asserts the oracle flags the surplus — the task's own example that a
// single tool_use is not satisfied by two results.
func TestOracleMetaFlagsSurplusToolResult(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleTool, Parts: Parts{
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "first"}}},
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "second"}}},
		}},
	}
	v := checkWire(in)
	if !hasInvariant(v, "tool-result-count-exact") {
		t.Fatalf("checkWire did not flag a surplus tool_result: %v", v)
	}
}

// TestOracleMetaFlagsDataLoss (invariant 5) simulates a hand-rolled
// "repair" that drops a genuine tool_result and replaces it with a
// synthetic marker — exactly the shape the reverted rewrite produced for
// real — and asserts checkNoDataLoss flags it.
func TestOracleMetaFlagsDataLoss(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "real output, not synthesized"}}}}},
	}
	// A deliberately bad "repair": discards the real result and replaces
	// it with a synthetic marker, as if the tool_use had never been
	// answered at all.
	badRepair := []Message{
		in[0],
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: SyntheticOrphanResultText}}, IsError: true}}},
	}
	v := checkNoDataLoss(in, badRepair)
	if !hasInvariant(v, "no-data-loss") {
		t.Fatalf("checkNoDataLoss did not flag a deleted tool_result: %v", v)
	}
}

// TestOracleMetaPassesOnValidHistory is a sanity check that the oracle
// does not flag ordinary, well-formed history — otherwise every "must not
// be flagged" assertion below would be meaningless.
func TestOracleMetaPassesOnValidHistory(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{&Text{Text: "go"}}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "done"}}},
	}
	if v := checkWire(in); len(v) != 0 {
		t.Fatalf("checkWire flagged well-formed history: %v", v)
	}
	if v := checkNoDataLoss(in, in); len(v) != 0 {
		t.Fatalf("checkNoDataLoss flagged an unchanged history: %v", v)
	}
}

// hasInvariant reports whether any violation in v carries the named
// invariant label.
func hasInvariant(v []wireViolation, invariant string) bool {
	for _, x := range v {
		if x.invariant == invariant {
			return true
		}
	}
	return false
}

// violationStrings renders v for a failure message.
func violationStrings(v []wireViolation) string {
	var b strings.Builder
	for _, x := range v {
		b.WriteString(x.String())
		b.WriteString("; ")
	}
	return b.String()
}

// --- Red-verification: current main's known-unrepaired shapes ---

// --- Legitimate shapes: must NOT be flagged ---
