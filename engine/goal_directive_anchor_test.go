package engine

import (
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestDropUnansweredDirectiveSkipsWhenAnchorGone is the red-first
// regression test for the histBefore-staleness finding: promptTurnWithRetry
// used to snapshot a LENGTH (histBefore := s.historyLen()) before calling
// s.Prompt. s.Prompt's own maybeAutoCompact runs first and can splice
// s.history to a different length — or fold the exact messages that length
// once pointed at into one summary message — before the directive is even
// appended. A length-based anchor cannot detect this: it either silently
// no-ops against the wrong boundary, or truncates an arbitrary interior
// point of a LATER attempt's own work.
//
// lastMessageID/dropUnansweredDirective's anchorID replaces the length with
// an identity: this test simulates compaction folding the anchor message
// away between the snapshot and the drop call, and proves the fix detects
// the gone anchor directly and safely does nothing, rather than guessing
// from a now-meaningless position.
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
// the identity-based anchor is immune to the OTHER histBefore failure mode:
// history growing back past a stale length by unrelated means (a
// compaction shrink followed by other appends) would make a length-based
// check cut an arbitrary interior point. The ID lookup instead finds the
// EXACT message and only ever considers what comes strictly after it.
func TestDropUnansweredDirectiveByIdentityIgnoresLaterUnrelatedGrowth(t *testing.T) {
	s := NewSession(Config{
		SessionDir:   t.TempDir(),
		Instructions: &InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})

	s.append(message.Message{ID: "anchor", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "earlier turn"}}})
	anchorID := s.lastMessageID()

	// This attempt's own directive — the only thing that should be dropped.
	s.append(message.Message{ID: "directive1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "cond"}}})

	s.dropUnansweredDirective(anchorID)

	got := s.History()
	if len(got) != 1 || got[0].ID != "anchor" {
		t.Fatalf("history = %+v, want just the anchor message (directive1 dropped)", got)
	}
}
