package message

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// This file is an INDEPENDENT oracle for "does this canonical history
// transcode to a protocol-valid provider request." It exists because of a
// bug class this package already shipped once: message/properties_test.go
// used to define hasOrphanToolCall by re-deriving ResolveOrphanToolCalls's
// own documented scan line-for-line (RoleAssistant-gated, "check only
// messages[i+1]", set-membership presence) — an oracle that shares its
// implementation's definition of correctness cannot fail on a wrong
// definition. A rewrite of ResolveOrphanToolCalls that deleted genuine tool
// output shipped and was reverted for exactly that reason (see the
// "fix(message,engine): narrow to the verified incident fix" commit).
//
// Every type and function below is built ONLY from two things: this
// package's own doc comments (Message.Role, ToolCall, ToolResult,
// ToolResult.SafeContent, message.NoToolOutputText,
// message.SyntheticOrphanResultText — all exported, all read for their
// documented contract, never for their body) and the three real
// transcoders' wire mapping (provider/anthropic, provider/openai,
// provider/openaicompat/transcode.go): specifically, that every transcoder
// maps message.RoleAssistant to a wire "assistant" turn and every other
// Role to a non-assistant turn, and that Anthropic (the strictest — see
// ResolveOrphanToolCalls's own doc comment, "Incident
// ses_01kx48z4rqfkpbwmzfdv1jzeg6") merges adjacent same-side canonical
// messages into one wire turn and requires every tool_use in an assistant
// turn to be answered, id for id, by a tool_result in the IMMEDIATELY
// FOLLOWING turn. This file never calls, imports, or copies
// ResolveOrphanToolCalls's internals, and never calls hasToolCall (there is
// no such symbol left in this package — see the note in properties_test.go
// on hasOrphanToolCall's removal).

// wireMsg is this oracle's minimal model of one canonical Message as it
// would reach a provider's wire: which side of the exchange it lands on,
// and the ordered tool_use / tool_result ids it carries. assistant is read
// directly off Message.Role — the one fact every transcoder's own role
// mapping keys on (`role := "user"; if m.Role == message.RoleAssistant {
// role = "assistant" }`, provider/anthropic/transcode.go and mirrored by
// provider/openai and provider/openaicompat).
type wireMsg struct {
	assistant  bool
	toolUseIDs []string
	results    []wireResult
}

// wireResult is one tool_result's id and whether its content is empty in
// the sense SafeContent's doc comment (NEP-5272, root cause 2) defines:
// nil, or carrying only a blank Text part — the shape a provider reads as
// ABSENT, not as an empty result.
type wireResult struct {
	callID string
	empty  bool
}

// foldWire builds the oracle's wire model straight from each canonical
// Message's Role and Parts, one wireMsg per input Message, order
// preserved. It looks at every part regardless of its enclosing Role
// (matching every transcoder's own transcodeParts, which is role-agnostic:
// it processes whatever parts are present) rather than assuming a ToolCall
// only ever appears in an assistant message or a ToolResult only in a tool
// message.
func foldWire(messages []Message) []wireMsg {
	out := make([]wireMsg, len(messages))
	for i, m := range messages {
		w := wireMsg{assistant: m.Role == RoleAssistant}
		for _, p := range m.Parts {
			switch v := p.(type) {
			case *ToolCall:
				w.toolUseIDs = append(w.toolUseIDs, v.CallID)
			case *ToolResult:
				w.results = append(w.results, wireResult{
					callID: v.CallID,
					empty:  emptyToolResultContent(v.Content),
				})
			}
		}
		out[i] = w
	}
	return out
}

// emptyToolResultContent decides what the PROVIDER treats as an absent
// tool_result: nil content, or content whose every part is an empty Text.
// That is the NEP-5272 wire fact — a null-content tool_result is read as
// ABSENT and rejects the whole request — not a restatement of any harness
// function.
//
// Its body currently matches ToolResult.isEmpty line for line. That is
// convergence on one wire rule, not the coupling this file's header
// forbids: the header's rule is that the oracle never derives its notion
// of CORRECTNESS from the code under test, and isEmpty is not under test
// here — ResolveOrphanToolCalls, and the transcode-only repair NEP-5293
// part 2 adds, are. If isEmpty ever
// changes, this function must NOT follow it; it must keep encoding what
// the provider does, and the disagreement is the signal.
//
// It reads the RAW Content field, never SafeContent or isEmpty themselves
// — calling SafeContent would already have repaired the very thing this
// function exists to detect.
func emptyToolResultContent(content Parts) bool {
	for _, p := range content {
		t, ok := p.(*Text)
		if !ok || t.Text != "" {
			return false
		}
	}
	return true
}

// wireRun is a maximal run of consecutive wireMsg sharing one side. This
// models the turn-merging every transcoder performs on adjacent same-role
// canonical messages (provider/anthropic/transcode.go: "The API requires
// strict user/assistant alternation; merge adjacent same-role messages") —
// without it, the documented legitimate shape "results split across two
// consecutive RoleTool messages" would look unanswered to a naive
// per-message check.
type wireRun struct {
	assistant   bool
	useCount    map[string]int
	resultCount map[string]int
}

func foldRuns(msgs []wireMsg) []wireRun {
	var runs []wireRun
	for _, m := range msgs {
		if len(runs) == 0 || runs[len(runs)-1].assistant != m.assistant {
			runs = append(runs, wireRun{
				assistant:   m.assistant,
				useCount:    map[string]int{},
				resultCount: map[string]int{},
			})
		}
		r := &runs[len(runs)-1]
		for _, id := range m.toolUseIDs {
			r.useCount[id]++
		}
		for _, res := range m.results {
			r.resultCount[res.callID]++
		}
	}
	return runs
}

// wireViolation names one broken invariant plus enough detail to debug it.
// The invariant field is one of the five labels this file's doc comment
// enumerates.
type wireViolation struct {
	invariant string
	detail    string
}

func (v wireViolation) String() string {
	return fmt.Sprintf("[%s] %s", v.invariant, v.detail)
}

// checkWire runs invariants 1-4 (adjacency, role placement, non-empty
// content, exact counts) against messages and returns every violation
// found; nil means messages is wire-valid under all four. Invariant 5
// (no data loss) is a two-sided comparison and lives in checkNoDataLoss
// below.
func checkWire(messages []Message) []wireViolation {
	wmsgs := foldWire(messages)
	var violations []wireViolation

	// Invariant 2: role placement is symmetric, and Anthropic enforces both
	// halves independently (each is its own distinct 400 on the wire): a
	// tool_result never sits in an assistant-role wire block, AND a
	// tool_use never sits in a non-assistant-role wire block. Every
	// transcoder's own role mapping (this file's header comment) sends
	// message.RoleAssistant to "assistant" and everything else to a
	// non-assistant turn, so a ToolCall living anywhere but a RoleAssistant
	// message — the off-label input this package's generators deliberately
	// still produce — ships a tool_use block on the wrong side of that
	// line.
	// Invariant 3: no tool_result with empty content.
	for i, w := range wmsgs {
		if !w.assistant {
			for _, id := range w.toolUseIDs {
				violations = append(violations, wireViolation{
					invariant: "no-tool-use-in-non-assistant-block",
					detail:    fmt.Sprintf("message %d: tool_use %q sits in a non-assistant-role wire block", i, id),
				})
			}
		}
		for _, r := range w.results {
			if w.assistant {
				violations = append(violations, wireViolation{
					invariant: "no-tool-result-in-assistant-block",
					detail:    fmt.Sprintf("message %d: tool_result %q sits in an assistant-role wire block", i, r.callID),
				})
			}
			if r.empty {
				violations = append(violations, wireViolation{
					invariant: "no-empty-tool-result-content",
					detail:    fmt.Sprintf("message %d: tool_result %q has absent/empty content", i, r.callID),
				})
			}
		}
	}

	// Invariant 1 (every tool_use answered, id for id, in the immediately
	// following turn) and invariant 4 (counts match exactly, both
	// directions) are two faces of one per-run count comparison.
	//
	// An "other" (non-assistant) run's demand is its OWN tool_use ids
	// (off-label, but not forbidden by any invariant below) plus, when the
	// immediately preceding run is an assistant run, that run's tool_use
	// ids too — the only run any tool_result answering an assistant
	// tool_use can legally sit in (a tool_result inside the assistant run
	// itself already trips the invariant-2 check above). An assistant
	// run's own tool_use is only ever checked here when it is the LAST run
	// in history: nothing can ever answer it.
	runs := foldRuns(wmsgs)
	for i, r := range runs {
		if r.assistant {
			if i == len(runs)-1 {
				for id, n := range r.useCount {
					violations = append(violations, wireViolation{
						invariant: "tool-use-unanswered",
						detail:    fmt.Sprintf("run %d (assistant, last in history): tool_use %q (x%d) has no following turn to answer it", i, id, n),
					})
				}
			}
			continue
		}
		demand := map[string]int{}
		for id, n := range r.useCount {
			demand[id] += n
		}
		if i > 0 && runs[i-1].assistant {
			for id, n := range runs[i-1].useCount {
				demand[id] += n
			}
		}
		ids := map[string]bool{}
		for id := range demand {
			ids[id] = true
		}
		for id := range r.resultCount {
			ids[id] = true
		}
		for id := range ids {
			need, have := demand[id], r.resultCount[id]
			switch {
			case need > have:
				violations = append(violations, wireViolation{
					invariant: "tool-use-unanswered",
					detail:    fmt.Sprintf("run %d: tool_use %q needs %d tool_result(s), got %d", i, id, need, have),
				})
			case have > need:
				violations = append(violations, wireViolation{
					invariant: "tool-result-count-exact",
					detail:    fmt.Sprintf("run %d: tool_result %q appears %d time(s), but only %d tool_use(s) need it here", i, id, have, need),
				})
			}
		}
	}
	return violations
}

// toolResultRecord is one ToolResult's identity for the no-data-loss
// check: its call id, error flag, and a byte-exact snapshot of its RAW
// Content (never SafeContent — this must catch content being CHANGED, not
// just content being made non-empty).
type toolResultRecord struct {
	callID  string
	isError bool
	content string
}

// toolResultRecords extracts every ToolResult in messages, in encounter
// order (message order, then part order). When excludeSynthetic is true, a
// ToolResult whose Content is exactly message.SyntheticOrphanResultText —
// the fixed marker ResolveOrphanToolCalls's doc comment says it always and
// only uses for a result IT inserted — is skipped, so the remaining
// sequence is "only what was already there."
func toolResultRecords(messages []Message, excludeSynthetic bool) []toolResultRecord {
	var out []toolResultRecord
	for _, m := range messages {
		for _, p := range m.Parts {
			tr, ok := p.(*ToolResult)
			if !ok {
				continue
			}
			if excludeSynthetic && tr.Content.Text() == SyntheticOrphanResultText {
				continue
			}
			raw, err := json.Marshal(tr.Content)
			if err != nil {
				raw = fmt.Appendf(nil, "<unmarshalable: %v>", err)
			}
			out = append(out, toolResultRecord{callID: tr.CallID, isError: tr.IsError, content: string(raw)})
		}
	}
	return out
}

// checkNoDataLoss is invariant 5: every ToolResult present in input must
// still be present in output, byte-identical, in the same relative order
// among all other results — after discarding any NEW synthetic entries
// output may have gained. This is the property the reverted rewrite broke:
// it deleted a genuine tool_result and replaced it with a synthetic
// "no result existed" marker when results were split across consecutive
// tool messages.
//
// Comparing the FILTERED-synthetic output sequence against the input
// sequence, rather than trying to map each result to "the same message
// index," sidesteps needing any notion of message identity across the
// call (ResolveOrphanToolCalls's doc says messages may be merged into or
// inserted after — this check does not need to know which): a genuine
// result that survives, unmoved relative to every OTHER genuine result,
// is exactly what "not lost, not reordered, not relocated" means for a
// flattened wire stream, which is what a provider actually reads.
// Asymmetry note: before uses excludeSynthetic=false and after uses
// excludeSynthetic=true, so this checker assumes the INPUT never contains a
// genuine ToolResult whose content equals SyntheticOrphanResultText. If one
// ever did, it would count in before, be filtered from after, and report a
// false no-data-loss violation. The assumption is load-bearing and currently
// only implicit: no generator seeds that marker, and no production path
// writes it except ResolveOrphanToolCalls itself. A generator that ever
// seeds known markers must filter both sides symmetrically instead.
func checkNoDataLoss(input, output []Message) []wireViolation {
	before := toolResultRecords(input, false)
	after := toolResultRecords(output, true)
	if reflect.DeepEqual(before, after) {
		return nil
	}
	return []wireViolation{{
		invariant: "no-data-loss",
		detail:    fmt.Sprintf("original tool_result sequence changed:\n before: %+v\n after:  %+v", before, after),
	}}
}
