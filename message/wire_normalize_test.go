package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// This file tests NormalizeForWire, the transcode-only sibling of
// ResolveOrphanToolCalls (see NormalizeForWire's own doc comment and
// NEP-5293 part 2). Every case below is checked against the independent
// wire-model oracle in wire_oracle_test.go — never against this function's
// own internals — exactly as message/wire_oracle_meta_test.go's red-
// verification cases do for ResolveOrphanToolCalls.

// TestNormalizeForWireRepairsDuplicateCallID is gap 1: two tool_use blocks
// in one assistant message share a CallID, answered by a single
// tool_result. NormalizeForWire must count occurrences (not just presence)
// and add a second synthetic result so both are answered.
func TestNormalizeForWireRepairsDuplicateCallID(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			toolCallPart("A", "bash", `{}`),
		}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire did not repair duplicate-CallID gap: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
}

// TestNormalizeForWireRepairsToolCallInNonAssistantMessage is gap 2: a
// ToolCall sits in a RoleUser message, never scanned by
// ResolveOrphanToolCalls's RoleAssistant-gated scan. NormalizeForWire must
// scan every message's parts regardless of role and answer it within its
// own run.
func TestNormalizeForWireRepairsToolCallInNonAssistantMessage(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "carries on"}}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire did not repair the non-assistant tool_call gap: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
}

// TestNormalizeForWireRepairsToolResultPrecedingToolCall is gap 3: a real
// ToolResult appears before its ToolCall ever exists in history.
// NormalizeForWire must RELOCATE the real result to sit after the call it
// answers, never leave it stranded while also inserting a synthetic.
func TestNormalizeForWireRepairsToolResultPrecedingToolCall(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "stray, too early"}}}}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire did not repair the preceding-tool_result gap: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
	// The real content must have been relocated, not replaced by a
	// synthetic marker: exactly one ToolResult in the output, and it must
	// carry the ORIGINAL text.
	got := toolResultRecords(out, false)
	if len(got) != 1 || got[0].callID != "A" || got[0].content == "" {
		t.Fatalf("expected exactly one real ToolResult A in output, got %+v", got)
	}
	for _, m := range out {
		for _, p := range m.Parts {
			if tr, ok := p.(*ToolResult); ok && tr.Content.Text() == SyntheticOrphanResultText {
				t.Fatalf("NormalizeForWire synthesized a result when the real one should have been relocated: %+v", out)
			}
		}
	}
}

// TestNormalizeForWireRepairsIntervalAssistantMessageSplit is gap 4 (the
// fourth shape found by the oracle, see the Linear comment on NEP-5293): a
// real ToolResult is separated from its ToolCall by an intervening
// assistant message. provider/anthropic/transcode.go merges adjacent
// same-role messages, so the wire sees ONE assistant run spanning both
// assistant messages — the run-merged wire is therefore already valid, and
// NormalizeForWire must not disturb it (must not synthesize an erroneous
// "no result was found" error ahead of the real one, the exact defect
// documented on current main).
func TestNormalizeForWireRepairsIntervalAssistantMessageSplit(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{&Text{Text: "go"}}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "thinking out loud"}}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "REAL OUTPUT"}}}}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire left the split-assistant-message shape wire-invalid: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
	got := toolResultRecords(out, false)
	if len(got) != 1 || got[0].callID != "A" {
		t.Fatalf("expected exactly one real ToolResult A in output, got %+v", got)
	}
	if got[0].isError {
		t.Fatalf("the real result must not be marked is_error: %+v", got[0])
	}
	for _, m := range out {
		for _, p := range m.Parts {
			if tr, ok := p.(*ToolResult); ok && tr.Content.Text() == SyntheticOrphanResultText {
				t.Fatalf("NormalizeForWire synthesized a spurious error result: %+v", out)
			}
		}
	}
}

// TestNormalizeForWireRepairsResultsSplitAcrossToolMessages is the second
// legitimate shape named in the revert commit and repeated in this issue:
// results answering two tool_use blocks split across two consecutive
// RoleTool messages. Run-level matching must see both without adding a
// spurious synthetic for the second.
func TestNormalizeForWireRepairsResultsSplitAcrossToolMessages(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			toolCallPart("B", "bash", `{}`),
		}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "a-out"}}}}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "B", Content: Parts{&Text{Text: "b-out"}}}}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire left the split-tool-messages shape wire-invalid: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
}

// TestNormalizeForWirePreservesResultInSameMessageAsCall confirms the
// off-label but blessed shape (ToolCall and its ToolResult sharing one
// non-assistant message) stays valid and untouched.
func TestNormalizeForWirePreservesResultInSameMessageAsCall(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{
			toolCallPart("A", "bash", `{}`),
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "self-answered"}}},
		}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire flagged a legitimate same-message call/result shape: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("checkNoDataLoss flagged a legitimate shape: %s", violationStrings(v))
	}
}

// TestNormalizeForWireNoOpOnValidHistory pins that ordinary, already-valid
// history is left untouched.
func TestNormalizeForWireNoOpOnValidHistory(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{&Text{Text: "go"}}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "done"}}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire flagged already-valid history: %s", violationStrings(v))
	}
	if len(out) != len(in) {
		t.Fatalf("NormalizeForWire changed message count on already-valid history: got %d, want %d", len(out), len(in))
	}
}

// TestNormalizeForWireOrdinaryOrphanAtEndOfHistory pins that the ordinary,
// already-repaired-by-ResolveOrphanToolCalls case (a trailing unanswered
// tool_use, incident ses_01kx48z4rqfkpbwmzfdv1jzeg6) is still handled.
func TestNormalizeForWireOrdinaryOrphanAtEndOfHistory(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
	}
	out := NormalizeForWire(in)
	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire did not repair a trailing orphaned tool_use: %s", violationStrings(v))
	}
	if v := checkNoDataLoss(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}
}

// TestNormalizeForWireIsFixedPoint proves re-applying NormalizeForWire to
// its own output changes nothing further, mirroring
// TestResolveOrphanToolCallsPropertyFixedPoint in properties_test.go.
func TestNormalizeForWireIsFixedPoint(t *testing.T) {
	cases := [][]Message{
		{
			{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`), toolCallPart("A", "bash", `{}`)}},
			{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
		},
		{
			{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "stray"}}}}},
			{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		},
	}
	for i, in := range cases {
		out := NormalizeForWire(in)
		again := NormalizeForWire(out)
		raw1, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("case %d: marshal(out): %v", i, err)
		}
		raw2, err := json.Marshal(again)
		if err != nil {
			t.Fatalf("case %d: marshal(again): %v", i, err)
		}
		if string(raw1) != string(raw2) {
			t.Fatalf("case %d: NormalizeForWire is not a fixed point:\n first: %s\nsecond: %s", i, raw1, raw2)
		}
	}
}

// TestResolveOrphanToolCallsRemainsAdditiveAcrossAllGapShapes guards the
// architecture NEP-5293 part 2 requires: ResolveOrphanToolCalls is the
// function engine.LoadSession applies to LIVE history (engine/store.go),
// so it must stay purely additive FOREVER, even for the four gap shapes
// NormalizeForWire (this file's own subject) exists specifically to
// repair at transcode time. This test pins that ResolveOrphanToolCalls's
// own output, run through EVERY one of the four gap shapes, never drops or
// reorders a real ToolResult — regardless of whether it also manages to
// make the shape wire-valid (it is documented NOT to, for most of these;
// see wire_oracle_meta_test.go's TestResolveOrphanToolCallsLeaves*Unrepaired and
// TestLegitimate* cases, which pin the precise wire-validity gaps this
// test deliberately does not re-assert). A future change that makes
// ResolveOrphanToolCalls itself destructive — the exact defect class that
// was reverted once already — fails this test first, before it could ever
// reach LIVE history.
func TestResolveOrphanToolCallsRemainsAdditiveAcrossAllGapShapes(t *testing.T) {
	shapes := map[string][]Message{
		"duplicate call id": {
			{Role: RoleAssistant, Parts: Parts{
				toolCallPart("A", "bash", `{}`),
				toolCallPart("A", "bash", `{}`),
			}},
			{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "ok"}}}}},
		},
		"tool call in non-assistant message": {
			{Role: RoleUser, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
			{Role: RoleAssistant, Parts: Parts{&Text{Text: "carries on"}}},
		},
		"tool result preceding its tool call": {
			{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "stray, too early"}}}}},
			{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
		},
		"tool result separated by an intervening assistant message": {
			{Role: RoleUser, Parts: Parts{&Text{Text: "go"}}},
			{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
			{Role: RoleAssistant, Parts: Parts{&Text{Text: "thinking out loud"}}},
			{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "A", Content: Parts{&Text{Text: "REAL OUTPUT"}}}}},
		},
	}
	for name, in := range shapes {
		t.Run(name, func(t *testing.T) {
			out := ResolveOrphanToolCalls(in)
			if v := checkNoDataLoss(in, out); len(v) != 0 {
				t.Fatalf("ResolveOrphanToolCalls lost or reordered real data for shape %q: %s", name, violationStrings(v))
			}
		})
	}
}

// TestNormalizeForWireDemotesUnanswerableToolResult is the golden
// regression test for the fifth gap: a ToolResult whose CallID matches NO
// ToolCall anywhere in history at all. Neither counting nor relocation can
// ever answer it (there is no tool_use to answer), and it is the SAME
// permanent-wedge class as NEP-5272 if shipped as a tool_result block —
// found live against the real anthropic transcoder. The fix changes its
// PART TYPE (ToolResult -> Text) rather than deleting or moving it: every
// byte of the real output stays plainly visible to the model, just no
// longer claiming to be an answer to a call that never happened.
func TestNormalizeForWireDemotesUnanswerableToolResult(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Parts: Parts{&Text{Text: "go"}}},
		{Role: RoleTool, Parts: Parts{&ToolResult{CallID: "GHOST", Content: Parts{&Text{Text: "ORPHAN OUTPUT"}}}}},
		{Role: RoleAssistant, Parts: Parts{&Text{Text: "ok"}}},
	}
	out := NormalizeForWire(in)

	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire left an unanswerable tool_result wire-invalid: %s", violationStrings(v))
	}
	// No ToolResult part survives for GHOST at all -- it must have been
	// demoted, not merely left in place (which checkWire's own
	// "tool-result-count-exact" surplus check above would already reject).
	for _, m := range out {
		for _, p := range m.Parts {
			if tr, ok := p.(*ToolResult); ok && tr.CallID == "GHOST" {
				t.Fatalf("GHOST survived as a ToolResult part: %+v -- it must be demoted to Text, not left as a tool_result with no answering tool_use", tr)
			}
		}
	}
	if v := checkNoDataLossAllowingDemotion(in, out); len(v) != 0 {
		t.Fatalf("demotion did not preserve the real output byte-for-byte: %s", violationStrings(v))
	}
}

// TestNormalizeForWireClaimSkipsUnclaimableHeadOfPool is the regression test
// for PR #108's finding 2: claimFromPool used to inspect only pool[0], so an
// unclaimable head entry (a surplus id nothing ever demands) permanently
// blocked a real, matching answer queued behind it in the pool. Here "B" is
// deposited before "A" in the same non-assistant run, and nothing anywhere
// ever demands "B" -- while "A" is later demanded by a ToolCall. Both land
// in the pool in that order ([B, A]). A claimFromPool that only checks
// pool[0] sees "B" first, refuses (wrong id), and gives up entirely instead
// of scanning past it to find "A" -- so the call for "A" gets a fabricated
// is_error "no result was found" AND the real answer for "A" is left
// unclaimed (later demoted to plain text by demoteWireInvalidToolResults,
// since it has no legitimate placement once relocation gave up on it).
// Wire-valid and lossless either way, which is why this needs its own
// targeted test rather than relying on the property tests (see the PR
// review finding for the full trace).
func TestNormalizeForWireClaimSkipsUnclaimableHeadOfPool(t *testing.T) {
	in := []Message{
		{Role: RoleTool, Parts: Parts{
			&ToolResult{CallID: "B", Content: Parts{&Text{Text: "surplus, never demanded"}}},
			&ToolResult{CallID: "A", Content: Parts{&Text{Text: "REAL A OUTPUT"}}},
		}},
		{Role: RoleAssistant, Parts: Parts{toolCallPart("A", "bash", `{}`)}},
	}
	out := NormalizeForWire(in)

	if v := checkWire(out); len(v) != 0 {
		t.Fatalf("NormalizeForWire left the pool head-of-line-blocking shape wire-invalid: %s", violationStrings(v))
	}
	if v := checkNoDataLossAllowingDemotion(in, out); len(v) != 0 {
		t.Fatalf("NormalizeForWire lost or altered real data: %s", violationStrings(v))
	}

	// The real answer for "A" must survive as an actual ToolResult -- a
	// genuine, non-error answer to the call -- not be demoted to text while
	// a synthetic is_error result stands in for it.
	var foundReal bool
	for _, m := range out {
		for _, p := range m.Parts {
			tr, ok := p.(*ToolResult)
			if !ok || tr.CallID != "A" {
				continue
			}
			if tr.Content.Text() == SyntheticOrphanResultText {
				t.Fatalf("NormalizeForWire synthesized a spurious is_error result for A instead of relocating the real one behind B in the pool: %+v", out)
			}
			if tr.IsError {
				t.Fatalf("the real result for A must not be marked is_error: %+v", tr)
			}
			if tr.Content.Text() != "REAL A OUTPUT" {
				t.Fatalf("the real result for A has the wrong content: %+v", tr)
			}
			foundReal = true
		}
	}
	if !foundReal {
		t.Fatalf("no real ToolResult A survived in output: %+v", out)
	}
}

// checkNoDataLossAllowingDemotion is checkNoDataLoss's test-side
// counterpart for NormalizeForWire specifically. It does NOT modify,
// relax, or reimplement checkNoDataLoss (message/wire_oracle_test.go):
// toolResultRecords, checkNoDataLoss's own extraction helper, is called
// here completely unmodified to build both sequences below. What this
// adds is recognizing ONE additional transformation checkNoDataLoss's own
// definition of survival ("still a ToolResult, in place, in sequence")
// cannot see at all: demoteWireInvalidToolResults legitimately rewrites a
// ToolResult that can never be wire-valid into a Text part — real content
// preserved, verbatim, just no longer claiming to answer a tool_use. This
// is intentionally NOT keyed on any specific PREDICTED reason a result
// might be demoted (an earlier version of this helper tried to predict
// "zero ToolCalls anywhere" specifically and broke the moment demotion's
// own scope widened to a second shape neither this helper nor the
// original fix anticipated — see demoteWireInvalidToolResults's own doc
// comment for that shape). Instead it walks checkNoDataLoss's own two
// sequences as a subsequence match: an input record either shows up next,
// in order, in the output's real-ToolResult sequence (exactly
// checkNoDataLoss's own criterion, unrelaxed) — or it doesn't, in which
// case it is required to be individually recoverable as plain text
// somewhere in the output, using the INPUT's own raw data, never
// NormalizeForWire's internal formatting choice for the replacement.
func checkNoDataLossAllowingDemotion(input, output []Message) []wireViolation {
	before := toolResultRecords(input, false)
	rawBefore := rawToolResults(input)
	after := toolResultRecords(output, true)

	var outputText strings.Builder
	for _, m := range output {
		outputText.WriteString(m.Parts.Text())
		outputText.WriteByte('\n')
	}
	rendered := outputText.String()

	var violations []wireViolation
	j := 0
	for i, b := range before {
		if j < len(after) && after[j] == b {
			j++
			continue
		}
		tr := rawBefore[i]
		// The call id may legitimately appear either literally or
		// %q-quoted (Go's standard safe rendering for a string that can
		// hold arbitrary, including non-printable, bytes). Neither form
		// is this checker imposing NormalizeForWire's own exact wording:
		// it is recognizing that a control byte has more than one safe
		// textual representation, not hard-coding one.
		idFound := strings.Contains(rendered, tr.CallID) || strings.Contains(rendered, fmt.Sprintf("%q", tr.CallID))
		body := tr.Content.Text()
		bodyFound := body == "" || strings.Contains(rendered, body)
		if !idFound || !bodyFound {
			violations = append(violations, wireViolation{
				invariant: "no-data-loss",
				detail:    fmt.Sprintf("tool_result %q neither survived as a ToolResult in sequence nor was found demoted to text (call id findable: %v, content findable: %v)", tr.CallID, idFound, bodyFound),
			})
		}
	}
	if j != len(after) {
		violations = append(violations, wireViolation{
			invariant: "no-data-loss",
			detail:    fmt.Sprintf("output's real ToolResult sequence has %d entr(y/ies) not explained by input's sequence (matched only %d of %d): %+v", len(after)-j, j, len(after), after),
		})
	}
	return violations
}

// rawToolResults extracts every ToolResult PART (not a snapshot record)
// from messages, in the exact same encounter order toolResultRecords
// (message/wire_oracle_test.go) uses, so index i here and index i in
// toolResultRecords(messages, false) always name the same occurrence.
func rawToolResults(messages []Message) []*ToolResult {
	var out []*ToolResult
	for _, m := range messages {
		for _, p := range m.Parts {
			if tr, ok := p.(*ToolResult); ok {
				out = append(out, tr)
			}
		}
	}
	return out
}
