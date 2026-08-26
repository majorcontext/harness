package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// goalEvaluatorPromptCeilingBytes is a generous, hardcoded safety ceiling —
// deliberately NOT computed from goalEvaluatorTranscriptBudgetBytes or any
// other production constant, so this test still fails meaningfully if a
// future change quietly widens the budget back toward unbounded. It sits
// far below the synthetic transcript this test seeds (see
// hugeSyntheticHistory) and comfortably above the real evaluator budget
// (goalEvaluatorFallbackContextWindowTokens * goalEvaluatorContextBudgetFraction
// * bytesPerTokenEstimate, on the order of tens of KB for the "test/eval"
// fake model, which modelmeta has no entry for and therefore falls back to
// the floor), leaving headroom for the fixed "GOAL CONDITION"/"CONVERSATION
// TRANSCRIPT" wrapper text and the truncation notice.
const goalEvaluatorPromptCeilingBytes = 100_000

// hugeSyntheticHistory builds n messages of roughly size bytes each,
// alternating user/assistant, reproducing the shape of a real long-running
// session's transcript (see box bx-01m0x8996's live incident: "prompt
// 245332 tokens > limit") without needing an actual multi-hundred-turn run.
func hugeSyntheticHistory(n, size int) []message.Message {
	history := make([]message.Message, 0, n)
	filler := strings.Repeat("x", size)
	for i := 0; i < n; i++ {
		role := message.RoleUser
		if i%2 == 1 {
			role = message.RoleAssistant
		}
		history = append(history, message.Message{
			ID:    newID("msg"),
			Role:  role,
			Parts: message.Parts{&message.Text{Text: filler}},
		})
	}
	return history
}

// TestPursueGoalEvaluatorPromptBoundedForHugeTranscript is the red-first
// regression test for the first live-evidence defect on box bx-01m0x8996:
// "engine: goal evaluator failed at 5 consecutive turn boundaries: context
// exhausted: prompt 245332 tokens > limit ...". Before this fix,
// runEvaluator built its CONVERSATION TRANSCRIPT field from
// renderConversation(s.History()) with no bound at all — it grows with the
// entire session transcript forever, unlike the main session, which
// automatic compaction protects.
//
// This seeds a synthetic history far larger (300 messages * 3000 bytes ==
// ~900KB, comfortably north of what any bounded evaluator budget should
// admit) than a goal evaluator call can safely fit, then drives the actual
// production entry point — PursueGoal, not renderConversationBounded
// directly — so the fix is proven on the path a real caller takes. The
// worker and evaluator are both scripted to succeed on the first turn, so
// nothing about retries or backoff is under test here — only whether the
// REQUEST the evaluator receives fits a sane bound.
func TestPursueGoalEvaluatorPromptBoundedForHugeTranscript(t *testing.T) {
	prov := &goalProvider{
		worker: [][]provider.Event{
			asstTurn(provider.StopEndTurn, &message.Text{Text: "all done"}),
		},
		eval: [][]provider.Event{
			evalTurn("MET: looks complete"),
		},
	}
	s := goalSession(t, prov, t.TempDir())
	s.history = hugeSyntheticHistory(300, 3000)
	seededBytes := 0
	for _, m := range s.history {
		seededBytes += len(m.Parts.Text())
	}

	res, err := s.PursueGoal(context.Background(), "cond", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal error = %v, want nil (a huge transcript must not fail the evaluator)", err)
	}
	if !res.Achieved {
		t.Fatalf("result = %+v, want achieved", res)
	}

	var evalReq *provider.Request
	for _, r := range prov.requests {
		if len(r.Tools) == 0 {
			evalReq = r
		}
	}
	if evalReq == nil {
		t.Fatal("no evaluator (tool-less) request recorded")
	}
	content := evalReq.Messages[0].Parts.Text()
	if len(content) > goalEvaluatorPromptCeilingBytes {
		t.Errorf("evaluator prompt = %d bytes, want <= %d (bounded to the evaluator's own context budget, not the whole %d-byte seeded transcript)",
			len(content), goalEvaluatorPromptCeilingBytes, seededBytes)
	}
	if !strings.Contains(content, goalEvaluatorTruncationNotice) {
		t.Error("evaluator prompt does not carry the truncation notice, want it present since the transcript was truncated")
	}
	// The newest message (the worker's own "all done" turn, plus the
	// directive that started it) must survive truncation — an evaluator
	// that cannot see what just happened cannot assess anything.
	if !strings.Contains(content, "all done") {
		t.Error("evaluator prompt lost the most recent turn, want the tail preserved")
	}
}

// TestRenderConversationBoundedPrefersSummaryPlusTail proves
// renderConversationBounded's "prefer summary + tail" behavior directly: a
// leading compaction-summary message (see isCompactionSummaryID,
// engine/compact.go — Compact splices its summary in place of the range it
// folded, tagged with the compactionSummaryIDTag prefix) followed by ordinary
// tail messages must render as exactly [summary, tail...], with the walk
// stopping at the summary rather than continuing further back into
// already-summarized history — even though a much older, oversized filler
// message sits before it that a naive byte-budget walk would otherwise have
// room to include.
func TestRenderConversationBoundedPrefersSummaryPlusTail(t *testing.T) {
	oldFiller := message.Message{
		ID:    newID("msg"),
		Role:  message.RoleUser,
		Parts: message.Parts{&message.Text{Text: "ancient message that predates compaction"}},
	}
	summary := message.Message{
		ID:    newID(compactionSummaryIDTag),
		Role:  message.RoleUser,
		Parts: message.Parts{&message.Text{Text: CompactionSummaryBanner + "earlier work: set up the repo and wrote tests"}},
	}
	tailUser := message.Message{ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "please continue"}}}
	tailAsst := message.Message{ID: newID("msg"), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "continuing now"}}}

	history := []message.Message{oldFiller, summary, tailUser, tailAsst}

	got, truncated := renderConversationBounded(history, 1_000_000) // budget is not the limiting factor here
	if !truncated {
		t.Error("truncated = false, want true (the pre-summary filler message was correctly dropped)")
	}
	if strings.Contains(got, "ancient message") {
		t.Errorf("rendered transcript = %q, must not include content from before the compaction summary", got)
	}
	if !strings.Contains(got, "earlier work: set up the repo") {
		t.Errorf("rendered transcript = %q, want the compaction summary's own content present", got)
	}
	if !strings.Contains(got, "please continue") || !strings.Contains(got, "continuing now") {
		t.Errorf("rendered transcript = %q, want both tail messages present", got)
	}
}

// TestRenderConversationBoundedKeepsNewestMessageEvenOverBudget proves the
// one deliberate exception: a budget too small for even one message never
// yields an empty transcript. renderPart/truncateForGoal's own per-part cap
// (goalPartCap) still bounds the single kept message, so this stays finite.
func TestRenderConversationBoundedKeepsNewestMessageEvenOverBudget(t *testing.T) {
	history := []message.Message{
		{ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: strings.Repeat("y", 10_000)}}},
	}
	got, truncated := renderConversationBounded(history, 10) // far too small for the message alone
	if truncated {
		t.Error("truncated = true, want false (a single message is never dropped, only per-part capped)")
	}
	if got == "" {
		t.Error("rendered transcript is empty, want the newest message kept regardless of budget")
	}
}

// TestGoalEvaluatorTranscriptBudgetBytesUsesRealWindowBelowFloor is the
// red-first regression test for a review finding on this fix:
// goalEvaluatorTranscriptBudgetBytes originally called resolveContextWindow,
// which conflates two different things behind the same (0, disabled)
// result — a model with NO modelmeta entry at all, and a model with a REAL,
// KNOWN entry that merely sits below minAutoContextWindowTokens (gpt-4's
// documented 8_192-token window is modelmeta's own example of the latter).
// resolveContextWindow's floor exists to answer "should automatic
// compaction ARM for this window," which is the right question for THAT
// caller but the wrong one here: folding a real 8_192-token evaluator model
// into the SAME goalEvaluatorFallbackContextWindowTokens (16k) fallback a
// genuinely unrecognized model gets would hand it a budget roughly DOUBLE
// its actual context window — the exact overflow class this whole fix
// exists to close.
//
// Uses the modelContextWindowLookup test seam (engine/context_window.go)
// to register one small-but-real window and leave a second model
// genuinely unregistered, then asserts the two get DIFFERENT budgets: the
// known-small one derived from its real 8_192 window, the unknown one from
// the 16k floor — proving the fix no longer treats them as the same case.
func TestGoalEvaluatorTranscriptBudgetBytesUsesRealWindowBelowFloor(t *testing.T) {
	orig := modelContextWindowLookup
	t.Cleanup(func() { modelContextWindowLookup = orig })

	small := message.ModelRef{Provider: "test", Model: "small-known"}
	unknown := message.ModelRef{Provider: "test", Model: "genuinely-unrecognized"}
	modelContextWindowLookup = func(m message.ModelRef) (int, bool) {
		if m == small {
			return 8_192, true // real, known, but below minAutoContextWindowTokens (16k)
		}
		return 0, false
	}

	smallBudget := goalEvaluatorTranscriptBudgetBytes(small)
	unknownBudget := goalEvaluatorTranscriptBudgetBytes(unknown)

	wantSmall := (int(8_192*goalEvaluatorContextBudgetFraction) - goalEvaluatorReserveTokens) * bytesPerTokenEstimate
	if smallBudget != wantSmall {
		t.Errorf("goalEvaluatorTranscriptBudgetBytes(known 8192-token model) = %d, want %d (derived from the model's REAL window)", smallBudget, wantSmall)
	}
	if smallBudget >= unknownBudget {
		t.Errorf("known-small-window budget (%d bytes) must be strictly SMALLER than the genuinely-unknown-model fallback budget (%d bytes) — a real 8192-token model must never get the same or a larger budget than an unrecognized one", smallBudget, unknownBudget)
	}
}
