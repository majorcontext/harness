package engine

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestDropUnansweredDirectiveSkipsWhenAnchorGone is the red-first
// regression test for dropUnansweredDirective's idx==-1 fallback (goal.go):
// when anchorID is no longer present in s.history — compaction folded the
// anchor message away entirely between the snapshot and this call — the
// lookup loop leaves idx at its initial -1. A buggy fallback that then
// treated "not found" the same as "start of history" (tail :=
// s.history[0:]) would go on to inspect, and potentially drop from, index
// 0 — this test proves the fix recognizes idx==-1 as "no safe anchor" and
// returns immediately instead.
//
// This does NOT, on its own, discriminate a length-based histBefore
// snapshot (the pre-identity implementation) from the identity-based
// anchorID one: histBefore would also have recorded 1 here, and a
// post-compaction history of length 1 leaves a length check with nothing
// to truncate, so a length-based approach "passes" this exact scenario by
// coincidence, not by correctly detecting staleness. See
// TestDropUnansweredDirectiveByIdentityIgnoresLaterUnrelatedGrowth below
// for the test that does discriminate length-based staleness from
// identity-based lookup. What this test proves is narrower, and still
// worth guarding on its own: a gone anchor must stop the method cold.
func TestDropUnansweredDirectiveSkipsWhenAnchorGone(t *testing.T) {
	s := NewSession(Config{
		SessionDir:   t.TempDir(),
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})

	s.append(message.Message{ID: "before1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "turn 1"}}})
	anchorID := s.lastMessageID()
	if anchorID != "before1" {
		t.Fatalf("lastMessageID = %q, want %q", anchorID, "before1")
	}

	// Simulate compaction folding "before1" away entirely — the race
	// maybeAutoCompact can win against a stale length index — leaving
	// behind exactly ONE RoleUser message. This shape is chosen
	// deliberately: it is indistinguishable, by role pattern alone, from
	// "just this attempt's own unanswered directive" (isSafeToDropDirective
	// Tail's len==1 case), so a buggy fallback that treats "anchor not
	// found" as "start of history" would wrongly approve dropping it. The
	// fix must recognize the anchor is gone and stop before ever reaching
	// that pattern check.
	s.mu.Lock()
	s.history = []message.Message{
		{ID: "directive1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "cond"}}},
	}
	s.mu.Unlock()

	s.dropUnansweredDirective(anchorID)

	got := s.History()
	if len(got) != 1 || got[0].ID != "directive1" {
		t.Fatalf("history = %+v, want unchanged (anchor gone, must not guess)", got)
	}
}

// TestDropUnansweredDirectiveByIdentityIgnoresLaterUnrelatedGrowth proves
// the identity-based anchor is immune to the OTHER histBefore failure mode
// lastMessageID's doc comment names: history that SHRANK (compaction
// folding older messages into one summary, while the anchor itself survives
// the fold — unlike TestDropUnansweredDirectiveSkipsWhenAnchorGone above,
// where the anchor is folded away too) and then GREW BACK PAST the now-
// stale numeric length by unrelated means — this attempt's own multi-
// message work. A length-based snapshot (histBefore := 4, taken before the
// fold) would slice at that stale numeric index, landing inside THIS
// ATTEMPT's own tail instead of at the anchor's boundary: it would cut an
// arbitrary interior point (keeping the directive and a partial assistant
// reply, but discarding the tool result that answers it) instead of
// dropping the whole tail atomically. The ID lookup instead finds the
// anchor's NEW position directly and only ever considers what comes
// strictly after it.
func TestDropUnansweredDirectiveByIdentityIgnoresLaterUnrelatedGrowth(t *testing.T) {
	s := NewSession(Config{
		SessionDir:   t.TempDir(),
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})

	// Before this attempt: 4 messages, ending in the anchor. A length-based
	// snapshot taken here would record histBefore = 4.
	s.append(message.Message{ID: "m1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "turn 1"}}})
	s.append(message.Message{ID: "m2", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "turn 1 reply"}}})
	s.append(message.Message{ID: "m3", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "turn 2"}}})
	s.append(message.Message{ID: "anchor", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "turn 2 reply"}}})
	anchorID := s.lastMessageID()
	if anchorID != "anchor" {
		t.Fatalf("lastMessageID = %q, want %q", anchorID, "anchor")
	}

	// Simulate compaction running inside this attempt's own s.Prompt call,
	// before the directive is even appended (see lastMessageID's doc
	// comment): m1-m3 fold into one summary, but the anchor itself survives
	// the fold boundary — realistic, since the cutoff falls in OLDER
	// history and the anchor is the most recent message. This SHRINKS
	// history from 4 messages to 2, moving the anchor from index 3 to
	// index 1 — the exact staleness a length snapshot cannot detect.
	s.mu.Lock()
	s.history = []message.Message{
		{ID: "summary", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "[compacted summary]"}}},
		{ID: "anchor", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "turn 2 reply"}}},
	}
	s.mu.Unlock()

	// This attempt's own work: the directive, then an interrupted turn's
	// partial assistant reply and its synthetic tool result — the one
	// three-message shape isSafeToDropDirectiveTail's case 3 approves (see
	// its own doc comment). This GROWS history from 2 back past the stale
	// histBefore=4, to 5 — but every message past index 1 (the anchor's NEW
	// position) belongs to this attempt, not to whatever unrelated content
	// a stale index 4 would have pointed at before the fold.
	s.append(message.Message{ID: "directive1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "cond"}}})
	s.append(message.Message{ID: "partial1", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "tc1", Name: "test_tool", Arguments: json.RawMessage(`{}`)}}})
	s.append(message.Message{ID: "toolresult1", Role: message.RoleTool, Parts: message.Parts{&message.ToolResult{CallID: "tc1", Content: message.Parts{&message.Text{Text: interruptedTurnErrorText}}}}})

	s.dropUnansweredDirective(anchorID)

	got := s.History()
	if len(got) != 2 || got[0].ID != "summary" || got[1].ID != "anchor" {
		t.Fatalf("history = %+v, want exactly [summary, anchor] (this attempt's whole tail dropped atomically, not cut at a stale interior point)", got)
	}
}
