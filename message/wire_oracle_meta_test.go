package message

import (
	"strings"
	"testing"
)

// This file has two jobs:
//
//  1. Meta-tests proving the oracle in wire_oracle_test.go can actually
//     fail — an oracle that always passes is worthless. One test per
//     invariant, feeding a known-bad wire shape and asserting checkWire (or
//     checkNoDataLoss) flags it.
//  2. Deliberate-gap documentation: ResolveOrphanToolCalls is purely
//     additive and PERMANENTLY leaves several shapes unrepaired (see its
//     own doc comment and NEP-5293). Each is run through that REAL
//     function and then the oracle, which must flag it. These tests
//     assert the gap EXISTS, so they pass today and must keep passing.
//     They are not pending work, and closing them here would be a BUG.
//     ResolveOrphanToolCalls must stay additive because LoadSession writes
//     its output back into live history — see AGENTS.md's additive-only
//     history-repair invariant. NEP-5293 part 2 closes these shapes in a
//     separate transcode-only repair instead, on the side of that line
//     where a destructive rule is safe. If one of these tests ever goes
//     red, ResolveOrphanToolCalls has become destructive and that
//     invariant is broken.

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
// to a wire "user" turn (provider/anthropic/transcode.go) and emits a
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
// NEP-5272 root cause 2 describes as read by the provider as ABSENT — and
// asserts the oracle flags it.
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

// TestResolveOrphanToolCallsLeavesDuplicateCallIDUnrepaired red-verifies gap 1 named
// in NEP-5293: two tool_use blocks in one assistant message share a
// CallID. ResolveOrphanToolCalls's presence check is set-membership
// (`present[id] = true`), so the single matching tool_result satisfies
// BOTH tool_use blocks and the second is never repaired — the request
// ships 2 tool_use / 1 tool_result for the same id.
//
// This gap is PERMANENT in ResolveOrphanToolCalls by design. The test
// asserts it still exists. NEP-5293 part 2 closes it at transcode time,
// in a separate repair, not here.
func TestResolveOrphanToolCallsLeavesDuplicateCallIDUnrepaired(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			toolCallPart("A", "bash", `{}`),
		}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
	}
	out := ResolveOrphanToolCalls(in)
	v := checkWire(out)
	if len(v) == 0 {
		t.Fatalf("expected the oracle to flag the duplicate-CallID gap, got no violations")
	}
	t.Logf("oracle violations: %s", violationStrings(v))
}

// TestResolveOrphanToolCallsLeavesNonAssistantToolCallUnrepaired
// red-verifies gap 2: ResolveOrphanToolCalls's scan is gated on
// `m.Role != RoleAssistant { continue }`, so a ToolCall sitting in a
// RoleUser message is never scanned for an orphan at all — yet every
// transcoder still emits a tool_use block for it (transcodeParts is
// role-agnostic). The request ships 1 tool_use / 0 tool_result.
func TestResolveOrphanToolCallsLeavesNonAssistantToolCallUnrepaired(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "carries on"}}},
	}
	out := ResolveOrphanToolCalls(in)
	v := checkWire(out)
	if len(v) == 0 {
		t.Fatalf("expected the oracle to flag the non-assistant tool_call gap, got no violations")
	}
	t.Logf("oracle violations: %s", violationStrings(v))
}

// TestResolveOrphanToolCallsLeavesEarlyToolResultUnrepaired
// red-verifies gap 3: a ToolResult appears BEFORE any ToolCall with the
// same id ever exists. ResolveOrphanToolCalls's scan only ever looks
// FORWARD from an assistant message to messages[i+1], never backward, so
// the stray early result is left in place AND, when the later ToolCall
// itself gets scanned and finds nothing at messages[i+1], a synthetic
// result is ALSO added. The request ships 1 tool_use / 2 tool_result.
func TestResolveOrphanToolCallsLeavesEarlyToolResultUnrepaired(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "stray, too early"}}}}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
	}
	out := ResolveOrphanToolCalls(in)
	v := checkWire(out)
	if len(v) == 0 {
		t.Fatalf("expected the oracle to flag the preceding-tool_result gap, got no violations")
	}
	t.Logf("oracle violations: %s", violationStrings(v))
}

// TestResolveOrphanToolCallsLeavesSelfAnsweredNonAssistantCallUnrepaired
// red-verifies a shape this file used to (wrongly) call legitimate: a
// ToolCall answered by its own ToolResult in the SAME message, that
// message's Role being RoleTool rather than RoleAssistant. Nothing is
// orphaned — the call and its result are present and matched — so
// ResolveOrphanToolCalls, whose scan is gated on `m.Role == RoleAssistant`,
// correctly leaves the message untouched. But invariant 2's tool_use half
// (TestOracleMetaFlagsToolUseInNonAssistantBlock above) still applies: this
// message's Role is not RoleAssistant, so every transcoder maps it to a
// non-assistant wire turn, and the tool_use block inside it is wire-invalid
// on that turn independent of the matching result sitting right next to
// it. This is deliberately off-label input (see properties_test.go's
// generator comment on why off-label role/part combinations are still
// valid input space). No data is lost or altered — the gap is placement,
// not loss — which is what distinguishes this from the duplicate-CallID
// and early-tool_result gaps above.
func TestResolveOrphanToolCallsLeavesSelfAnsweredNonAssistantCallUnrepaired(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "self-answered"}}},
		}},
	}
	out := ResolveOrphanToolCalls(in)
	v := checkWire(out)
	if !hasInvariant(v, "no-tool-use-in-non-assistant-block") {
		t.Fatalf("expected the oracle to flag the self-answered non-assistant tool_use gap, got: %s", violationStrings(v))
	}
	t.Logf("oracle violations: %s", violationStrings(v))
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("checkNoDataLoss flagged a shape that loses nothing: %s", violationStrings(v))
	}
}

// --- Legitimate shapes: must NOT be flagged ---

// TestLegitimateSplitAcrossAssistantMessages is the precise shape named in
// the "narrow to the verified incident fix" revert commit:
// [assistant(tool_call A), assistant(text), tool(result A)]. That commit's
// claim is narrower than "fully wire-valid": it says the reverted rewrite
// DELETED this result, and that main's purely-additive implementation
// "can fail to repair, leaving a visible and recoverable 400, but it never
// deletes." This test verifies exactly that narrower claim — no data loss
// — and does NOT assert full wire validity, because it does not hold here.
//
// TENSION FOUND (reported per the task's instruction, not silently
// resolved): ResolveOrphanToolCalls only ever looks at messages[i+1], the
// single next canonical message, never a merged run of same-role
// messages. Since messages[1] here is RoleAssistant (not RoleTool),
// message[0]'s tool_call A is treated as unanswered and a SYNTHETIC
// result is spliced in between message[0] and message[1] — leaving the
// REAL tool(result A) at the end dangling with no tool_use immediately
// before it. Run against the real function, checkWire flags this with
// "tool-result-count-exact: tool_result A appears 1 time(s), but only 0
// tool_use(s) need it here" (logged below). This is a fourth,
// previously-unnamed wire-validity gap in the current additive
// implementation — never a data-loss regression, since the real result
// is untouched — that NEP-5293 part 2 should also close.
func TestLegitimateSplitAcrossAssistantMessages(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "thinking out loud"}}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
	}
	out := ResolveOrphanToolCalls(in)
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("checkNoDataLoss flagged a shape the implementation is documented to handle without data loss: %s", violationStrings(v))
	}
	if v := checkWire(out); len(v) != 0 {
		t.Logf("checkWire finds this shape NOT fully wire-valid under the current additive implementation (no data loss, but a wire-validity gap — see this test's doc comment): %s", violationStrings(v))
	}
}

// TestLegitimateResultsSplitAcrossToolMessages is the other shape named in
// the revert commit: results answering one assistant turn's two
// tool_use blocks split across two consecutive RoleTool messages. Same
// narrower claim and same kind of tension as
// TestLegitimateSplitAcrossAssistantMessages above.
//
// TENSION FOUND: ResolveOrphanToolCalls merges a synthetic result into
// messages[i+1] alone when checking whether messages[i+1] answers ALL of
// an assistant turn's calls — it never looks past messages[i+1] to a
// SECOND consecutive RoleTool message. Here messages[i+1] only carries
// A's result, so a SYNTHETIC B is merged into it, even though a REAL B
// arrives one message later. checkWire flags the resulting surplus
// ("tool_result B appears 2 time(s), but only 1 tool_use(s) need it
// here", logged below). Again: no data loss (the real B is untouched),
// but not fully wire-valid — the same class of gap as above.
func TestLegitimateResultsSplitAcrossToolMessages(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			toolCallPart("B", "bash", `{}`),
		}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "a-out"}}}}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "B", Content: Parts{&Text{Text: "b-out"}}}}},
	}
	out := ResolveOrphanToolCalls(in)
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("checkNoDataLoss flagged a shape the implementation is documented to handle without data loss: %s", violationStrings(v))
	}
	if v := checkWire(out); len(v) != 0 {
		t.Logf("checkWire finds this shape NOT fully wire-valid under the current additive implementation (no data loss, but a wire-validity gap — see this test's doc comment): %s", violationStrings(v))
	}
}
