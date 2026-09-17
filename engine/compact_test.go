package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestEstimatePromptTokensFromHistoryCountsImageAtFixedTokenEstimate is the
// red-first regression test for NEW-BLOCKING 9's estimator half: an image
// message.Blob's contribution must be a fixed imageBlockTokenEstimate
// tokens, independent of its encoded byte size — not
// len(Blob.Data)/bytesPerTokenEstimate, which over-counts a real image by
// close to an order of magnitude. Anthropic resizes and tiles an image
// before tokenizing it, so a 40 KB blob costs the same ~1600 tokens any
// full-size image costs, not 40000/4 = 10000.
func TestEstimatePromptTokensFromHistoryCountsImageAtFixedTokenEstimate(t *testing.T) {
	bigImage := bytes.Repeat([]byte{0xFF}, 40000)
	history := []message.Message{
		{Role: message.RoleUser, Parts: message.Parts{
			&message.Text{Text: "look at this"},
			&message.Blob{MediaType: "image/png", Data: bigImage},
		}},
	}
	got := estimatePromptTokensFromHistory(history)
	want := len("look at this")/bytesPerTokenEstimate + imageBlockTokenEstimate
	if got != want {
		t.Fatalf("estimatePromptTokensFromHistory = %d, want %d (text bytes/4 plus a flat %d for the image, not %d for its raw byte length)",
			got, want, imageBlockTokenEstimate, len(bigImage)/bytesPerTokenEstimate)
	}
	if oldStyleEstimate := len(bigImage) / bytesPerTokenEstimate; got >= oldStyleEstimate {
		t.Fatalf("estimate %d did not improve on the byte-based estimate %d the fix replaces", got, oldStyleEstimate)
	}
}

// TestEstimatePromptTokensFromHistoryNonImageBlobStillUsesByteEstimate pins
// that the fix is scoped to image/* media types only: a non-image Blob
// (e.g. a PDF attachment) has no comparably documented flat per-unit cost,
// so it still falls back to the byte heuristic.
func TestEstimatePromptTokensFromHistoryNonImageBlobStillUsesByteEstimate(t *testing.T) {
	data := bytes.Repeat([]byte{'%'}, 4000)
	history := []message.Message{
		{Role: message.RoleUser, Parts: message.Parts{
			&message.Blob{MediaType: "application/pdf", Data: data},
		}},
	}
	got := estimatePromptTokensFromHistory(history)
	if want := len(data) / bytesPerTokenEstimate; got != want {
		t.Fatalf("estimatePromptTokensFromHistory for a non-image blob = %d, want %d (byte/4, unchanged)", got, want)
	}
}

// TestEstimatePromptTokensFromHistoryCountsImageInsideToolResult pins that
// the image-token fix also applies through the recursive ToolResult.Content
// path (e.g. a read_file tool result returning a screenshot), not only a
// top-level message part.
func TestEstimatePromptTokensFromHistoryCountsImageInsideToolResult(t *testing.T) {
	bigImage := bytes.Repeat([]byte{0xFF}, 40000)
	history := []message.Message{
		{Role: message.RoleAssistant, Parts: message.Parts{
			&message.ToolResult{CallID: "call_1", Content: message.Parts{
				&message.Blob{MediaType: "image/jpeg", Data: bigImage},
			}},
		}},
	}
	got := estimatePromptTokensFromHistory(history)
	want := len("call_1")/bytesPerTokenEstimate + imageBlockTokenEstimate
	if got != want {
		t.Fatalf("estimatePromptTokensFromHistory for an image inside ToolResult.Content = %d, want %d", got, want)
	}
}

// compactTurnSeq gives each compactTurn call in a test a distinct assistant
// message ID: asstTurn's shared "msg_a" constant is fine for tests that
// never compare IDs across turns, but compaction's FirstID/LastID and its
// ID-based splice (see spliceCompact) need turns to be distinguishable by
// ID, exactly as production message IDs always are (every message gets a
// fresh newID()).
var compactTurnSeq int

// compactTurn builds a scripted worker-turn assistant reply carrying usage,
// with a fresh, unique message ID (see compactTurnSeq).
func compactTurn(text string, usage provider.Usage) []provider.Event {
	compactTurnSeq++
	msg := &message.Message{ID: fmt.Sprintf("msg_asst_%d", compactTurnSeq), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}}
	ev := provider.Event{Type: provider.EventDone, Message: msg, StopReason: provider.StopEndTurn, Usage: usage}
	return []provider.Event{ev}
}

// compactSummaryTurn builds a scripted reply for the tool-less summarization
// call Session.Compact issues: a plain text assistant message, no tool
// calls, ending the stream via EventDone.
func compactSummaryTurn(text string, usage provider.Usage) []provider.Event {
	return compactTurn(text, usage)
}

// runTurns drives n ordinary Prompt calls against s, failing the test on any
// error.
func runTurns(t *testing.T, s *Session, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.Prompt(context.Background(), "go"); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
}

// TestCompactFoldsOldestPrefixKeepsRecentTurns is the red-first behavior
// test for the core mechanism (docs/design/context-compaction.md §2): a
// contiguous prefix of whole turns folds into one summary message, the most
// recent keep_turns turns survive verbatim, and FirstID/LastID name exactly
// the folded range's boundary messages.
func TestCompactFoldsOldestPrefixKeepsRecentTurns(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10, OutputTokens: 5}),
		compactTurn("two", provider.Usage{InputTokens: 20, OutputTokens: 5}),
		compactTurn("three", provider.Usage{InputTokens: 30, OutputTokens: 5}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 40, OutputTokens: 8}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 3)

	before := s.History()
	if len(before) != 6 {
		t.Fatalf("history before compact = %d messages, want 6 (3 turns x 2)", len(before))
	}
	wantFirstID := before[0].ID // turn 1's leading RoleUser message
	wantLastID := before[3].ID  // last message before turn 3's leading RoleUser message

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded != 2 {
		t.Fatalf("TurnsFolded = %d, want 2", res.TurnsFolded)
	}
	if res.FirstID != wantFirstID {
		t.Errorf("FirstID = %q, want %q", res.FirstID, wantFirstID)
	}
	if res.LastID != wantLastID {
		t.Errorf("LastID = %q, want %q", res.LastID, wantLastID)
	}

	after := s.History()
	if len(after) != 3 {
		t.Fatalf("history after compact = %d messages, want 3 (summary + kept turn 3)", len(after))
	}
	if after[0].Role != message.RoleUser {
		t.Errorf("after[0].Role = %s, want RoleUser (summary)", after[0].Role)
	}
	if after[0].ID != res.Summary.ID {
		t.Errorf("after[0].ID = %q, want summary id %q", after[0].ID, res.Summary.ID)
	}
	if got := after[0].Parts.Text(); got == "" {
		t.Error("summary message has no text")
	}
	// Turn 3 survives verbatim.
	if after[1].Parts.Text() != "go" && after[1].Role != message.RoleUser {
		t.Errorf("after[1] = %+v, want turn 3's user message", after[1])
	}
	if after[2].Parts.Text() != "three" {
		t.Errorf("after[2] text = %q, want %q (turn 3's assistant reply)", after[2].Parts.Text(), "three")
	}
}

// TestCompactPreservesRetainedResultsIndex is review finding F3(a)'s red
// test. The retention ceiling (Config.ToolResultRetainedBytes) is monotonic
// — only ever incremented, nothing evicts or reclaims it — and
// compactionSystemPrompt forbids the summarizer from transcribing tool
// output, so a fold that swallows a preview line carrying a trh_N handle
// leaves that handle ORPHANED: still counted against the ceiling, its
// sidecar file still on disk, but no longer named anywhere in live history
// for the model to find. Compact must carry a machine-written index of
// every still-live handle forward into the summary turn — built
// deterministically from session state, NOT from the LLM's summary text,
// which this test's scripted summarizer reply ("SUMMARY", containing no
// handle at all) proves: the index must appear regardless.
func TestCompactPreservesRetainedResultsIndex(t *testing.T) {
	dir := t.TempDir()
	big := linesText(3000)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn(provider.StopToolUse, toolCall("tc1", "bigtool", `{}`)),
		compactTurn("turn one done", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}), // deliberately never mentions trh_1
	}}
	s := NewSession(Config{
		Providers:             provider.Registry{"test": prov},
		Model:                 message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:            dir,
		ToolResultInlineBytes: 512,
		Tools:                 []Tool{bigOutputTool("bigtool", big)},
	})
	runTurns(t, s, 3) // turn 1 mints trh_1; turns 2 and 3 are ordinary

	if _, ok := s.lookupToolResult("trh_1"); !ok {
		t.Fatal("trh_1 not minted in turn 1")
	}

	// Fold turns 1-2, keeping only turn 3 — trh_1's preview line (turn 1)
	// is swallowed by the fold.
	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded != 2 {
		t.Fatalf("TurnsFolded = %d, want 2", res.TurnsFolded)
	}

	summaryText := res.Summary.Parts.Text()
	if !strings.Contains(summaryText, "trh_1") {
		t.Fatalf("summary message does not carry the retained-results index for trh_1:\n%s", summaryText)
	}
	if !strings.Contains(summaryText, "bigtool") {
		t.Errorf("index missing the source tool name:\n%s", summaryText)
	}
	if !strings.Contains(summaryText, fmt.Sprintf("%d", len(big))) {
		t.Errorf("index missing the byte size:\n%s", summaryText)
	}
	if !strings.Contains(summaryText, "line-1") {
		t.Errorf("index missing a recognizable head excerpt:\n%s", summaryText)
	}
	// The whole point: this survives even though the scripted LLM summary
	// never mentioned it.
	if strings.Contains("SUMMARY", "trh_1") {
		t.Fatal("test setup: the scripted summary must not itself mention trh_1")
	}
}

// TestCompactRetainedResultsIndexOmittedWhenNoHandles: an ordinary
// compaction with no retained results at all must not grow a spurious empty
// index block.
func TestCompactRetainedResultsIndexOmittedWhenNoHandles(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 3)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Summary.Parts) != 1 {
		t.Errorf("summary has %d parts, want 1 (no retained-results index when there are no handles): %+v", len(res.Summary.Parts), res.Summary.Parts)
	}
}

// TestRetainedResultsIndexCapped is review finding N8's red test:
// unbounded, 200 handles measured at roughly 6.9k tokens of index text —
// with the retention ceiling disabled (Config.ToolResultRetainedBytes <= 0)
// a long session can mint arbitrarily many. The index must list only the
// newest retainedResultsIndexMaxHandles and name the rest by COUNT only.
// Exercised directly against retainedResultsIndexPart (unit-level) rather
// than through a full multi-turn Compact, since minting 40 handles through
// real tool-calling turns would be needlessly slow for what is fundamentally
// a test of one function's list-bounding logic.
func TestRetainedResultsIndexCapped(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, ToolResultInlineBytes: 64})

	total := retainedResultsIndexMaxHandles + 8
	for i := 0; i < total; i++ {
		if _, err := s.writeRetainedToolResult("bash", strings.Repeat("x", 100)); err != nil {
			t.Fatal(err)
		}
	}

	idx := s.retainedResultsIndexPart()
	if idx == nil {
		t.Fatal("index is nil despite retained handles existing")
	}
	text := idx.Text

	listed := strings.Count(text, "tool=bash")
	if listed != retainedResultsIndexMaxHandles {
		t.Errorf("index lists %d handles, want exactly the cap %d", listed, retainedResultsIndexMaxHandles)
	}
	// The newest handles are the ones listed, not the oldest.
	newest := fmt.Sprintf("trh_%d", total)
	oldest := "trh_1 "
	if !strings.Contains(text, newest) {
		t.Errorf("index does not list the newest handle %s:\n%s", newest, text)
	}
	if strings.Contains(text, oldest) {
		t.Errorf("index lists the OLDEST handle %s — want only the newest %d:\n%s", oldest, retainedResultsIndexMaxHandles, text)
	}
	wantOlder := total - retainedResultsIndexMaxHandles
	if !strings.Contains(text, fmt.Sprintf("...and %d older retained result", wantOlder)) {
		t.Errorf("index does not name the %d older, unlisted handles by count:\n%s", wantOlder, text)
	}
}

// TestRetainedResultsIndexNotesMissingSidecar is review finding N9's red
// test: the index must not assert "still readable" for a handle whose
// sidecar file is gone (an operator wiped toolresults/, a volume rolled
// back) — it must check, not assume.
func TestRetainedResultsIndexNotesMissingSidecar(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir, ToolResultInlineBytes: 64})

	if _, err := s.writeRetainedToolResult("bash", "kept content\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeRetainedToolResult("bash", "gone content\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.toolResultPath("trh_2")); err != nil {
		t.Fatal(err)
	}

	idx := s.retainedResultsIndexPart()
	if idx == nil {
		t.Fatal("index is nil")
	}
	text := idx.Text
	lines := strings.Split(text, "\n")

	var line1, line2 string
	for _, l := range lines {
		if strings.Contains(l, "trh_1 ") {
			line1 = l
		}
		if strings.Contains(l, "trh_2 ") {
			line2 = l
		}
	}
	if line1 == "" || line2 == "" {
		t.Fatalf("both handles must still be listed (metadata is real regardless of file presence):\n%s", text)
	}
	if !strings.Contains(line1, "still readable") {
		t.Errorf("trh_1 (sidecar present) not marked readable: %q", line1)
	}
	if strings.Contains(line2, "still readable") {
		t.Errorf("trh_2 (sidecar REMOVED) incorrectly claimed readable: %q", line2)
	}
	if !strings.Contains(line2, "missing") && !strings.Contains(line2, "no longer readable") {
		t.Errorf("trh_2's missing sidecar is not flagged in the index: %q", line2)
	}
}

// TestCompactSummaryRequestAlwaysEndsInUserRole is the red-first test for
// the 2026-08-19 live incident (session ses_jumpy-pizza, model
// anthropic/anthropic/claude-fable-5): compacting with keep_turns=20
// returned `{"error":"[permanent] anthropic: This model does not support
// assistant message prefill. The conversation must end with a user
// message. (invalid_request_error, HTTP 400)"}` while keep_turns=8 on the
// SAME session succeeded. Root cause: foldEnd (Compact's fold range) is the
// last message before the next KEPT turn's leading RoleUser message —
// ordinarily that folded turn's own final assistant reply, RoleAssistant —
// and the old code sent `folded` as req.Messages verbatim, with no trailing
// message to guarantee the wire request ends in a user turn. This test's
// two-turn fold reproduces that ordinary shape directly: the fold range's
// own last message is RoleAssistant (turn 1's scripted reply), which is
// exactly what a plain-prefix compaction always was, not a rare edge case.
func TestCompactSummaryRequestAlwaysEndsInUserRole(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	// Sanity: the fold range's own last message really is RoleAssistant —
	// the reproduction shape. If this ever stops being true the test below
	// proves nothing about the bug it guards.
	history := s.History()
	if history[1].Role != message.RoleAssistant {
		t.Fatalf("sanity: fold range's last message (history[1]) role = %s, want RoleAssistant", history[1].Role)
	}

	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatal(err)
	}

	summaryReq := prov.requests[len(prov.requests)-1]
	if len(summaryReq.Messages) == 0 {
		t.Fatal("summarization request carried no messages")
	}
	last := summaryReq.Messages[len(summaryReq.Messages)-1]
	if last.Role != message.RoleUser {
		t.Errorf("summarization request's last message role = %s, want RoleUser (a request ending in RoleAssistant is treated as assistant message prefill, which some models — including the one that hit this live — reject outright)", last.Role)
	}
}

// TestCompactionRequestMessagesAlwaysEndsInUser is a focused unit test on
// compactionRequestMessages itself, covering every role the folded range's
// own last message can plausibly carry (RoleAssistant — the ordinary
// completed-turn case; RoleTool — a message.ResolveOrphanToolCalls
// synthetic repair left by an interrupted tool call, the shape that
// happened to mask this bug on keep_turns=8 in the live incident; RoleUser
// — folding a range that is itself already a prior compaction summary):
// every case must produce a request ending in RoleUser.
func TestCompactionRequestMessagesAlwaysEndsInUser(t *testing.T) {
	for _, tc := range []struct {
		name string
		last message.Role
	}{
		{"assistant", message.RoleAssistant},
		{"tool", message.RoleTool},
		{"user", message.RoleUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folded := []message.Message{
				{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task"}}},
				{ID: "msg_2", Role: tc.last, Parts: message.Parts{&message.Text{Text: "reply"}}},
			}
			out := compactionRequestMessages(folded)
			if len(out) == 0 {
				t.Fatal("compactionRequestMessages returned no messages")
			}
			if got := out[len(out)-1].Role; got != message.RoleUser {
				t.Errorf("last message role = %s, want RoleUser", got)
			}
			// The folded range itself is preserved verbatim, untouched, as a
			// prefix — only a trailing message is appended.
			if len(out) != len(folded)+1 {
				t.Fatalf("len(out) = %d, want %d (folded + 1 trailing instruction)", len(out), len(folded)+1)
			}
			for i := range folded {
				if out[i].ID != folded[i].ID {
					t.Errorf("out[%d].ID = %q, want %q (folded range must not be mutated)", i, out[i].ID, folded[i].ID)
				}
			}
		})
	}
}

// TestCompactSummaryRequestInheritsSessionEffort is the red-first test for
// issue #124: unlike the goal evaluator (a classifier that pins off), the
// compaction summarizer benefits from the session's own quality setting, so
// its request must carry the session's CURRENT effort level (set via
// SetEffort, read fresh, not the zero value the session started with).
// Drives the production entry point (Session.Compact -> runCompactionSummary).
func TestCompactSummaryRequestInheritsSessionEffort(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 10}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	s.SetEffort(message.EffortMedium)
	runTurns(t, s, 3)

	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatal(err)
	}

	if n := len(prov.requests); n == 0 {
		t.Fatal("no requests captured")
	}
	summaryReq := prov.requests[len(prov.requests)-1]
	if summaryReq.Effort != message.EffortMedium {
		t.Errorf("summarizer request Effort = %q, want %q", summaryReq.Effort, message.EffortMedium)
	}
}

// TestCompactSummaryBannerMarksSyntheticOrigin asserts the summary text
// carries the visible synthesized-and-marked banner, mirroring
// message.SyntheticOrphanResultText's spirit — a transcript reader can never
// mistake it for something the human actually typed.
func TestCompactSummaryBannerMarksSyntheticOrigin(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("the gist", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Summary.Parts.Text()
	if got != CompactionSummaryBanner+"the gist" {
		t.Errorf("summary text = %q, want banner-prefixed", got)
	}
}

// TestCompactNoopWhenNotEnoughTurns is the red-first test for §2's minimum-
// fold rule: fewer than keep_turns complete turns exist yet, so compaction
// is a no-op (turns_folded: 0, not an error) — and, crucially, it never
// calls the provider at all (nothing to summarize).
func TestCompactNoopWhenNotEnoughTurns(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 1)

	before := s.History()
	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if res.TurnsFolded != 0 {
		t.Errorf("TurnsFolded = %d, want 0", res.TurnsFolded)
	}
	if res.SkipReason != SkipReasonNotEnoughTurns {
		t.Errorf("SkipReason = %q, want %q (review follow-up on PR #136, Finding A/C)", res.SkipReason, SkipReasonNotEnoughTurns)
	}
	if len(prov.requests) != 1 {
		t.Errorf("provider calls = %d, want 1 (only the worker turn — no summarization call)", len(prov.requests))
	}
	after := s.History()
	if len(after) != len(before) {
		t.Errorf("history mutated on a no-op compaction: before=%d after=%d", len(before), len(after))
	}
}

// TestCompactKeepTurnsFloor is the red-first test for the hard floor on
// keep_turns (docs/design/context-compaction.md §1): the most recent turn
// is never foldable, so even an aggressive KeepTurns request always leaves
// at least one whole turn verbatim.
func TestCompactKeepTurnsFloor(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	// KeepTurns: 1 is the minimum valid value (server-side validation
	// rejects <= 0 before ever reaching here — see server/handlers.go); the
	// engine must honor it exactly, never defaulting it away.
	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.TurnsFolded != 1 {
		t.Fatalf("TurnsFolded = %d, want 1", res.TurnsFolded)
	}
	after := s.History()
	if len(after) != 3 { // summary + kept turn's user+assistant
		t.Fatalf("history after compact = %d messages, want 3", len(after))
	}
}

// TestCompactUsageAccountingCumulativeOnlyNotLastUsage is the red-first test
// for §2's "Usage accounting": the summarization call's tokens are real
// spend and must be added to cumulative Usage(), but must NEVER overwrite
// LastUsage() — the automatic trigger reads LastUsage as "how large is the
// next worker request", and a small summarization call would mask the very
// pressure that triggered compaction.
func TestCompactUsageAccountingCumulativeOnlyNotLastUsage(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 100, OutputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 200, OutputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 7, OutputTokens: 3}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)

	wantUsageBefore := provider.Usage{InputTokens: 300, OutputTokens: 20}
	if got := s.Usage(); got != wantUsageBefore {
		t.Fatalf("Usage before compact = %+v, want %+v", got, wantUsageBefore)
	}
	lastBefore, _ := s.LastUsage()

	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatal(err)
	}

	wantUsageAfter := provider.Usage{InputTokens: 307, OutputTokens: 23}
	if got := s.Usage(); got != wantUsageAfter {
		t.Errorf("Usage after compact = %+v, want %+v (summarization spend added)", got, wantUsageAfter)
	}
	lastAfter, ok := s.LastUsage()
	if !ok {
		t.Fatal("LastUsage not ok after compact")
	}
	if lastAfter != lastBefore {
		t.Errorf("LastUsage changed by compaction: before=%+v after=%+v, want unchanged", lastBefore, lastAfter)
	}
}

// TestCompactFailureNoJournalNoMutation is the red-first test for §2's
// "Failure handling": when the summarization call itself errors, compaction
// aborts cleanly — no history mutation, no journal write, and an emitted
// EventCompactionFailed.
func TestCompactFailureNoJournalNoMutation(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		// No third scripted turn: the summarization call (the 3rd
		// prov.Stream call) exhausts p.turns and returns io.ErrUnexpectedEOF
		// (see scriptedProvider.Stream).
	}}
	dir := t.TempDir()
	var evs []Event
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
		OnEvent:    func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	before := s.History()
	beforeCompactCount := s.CompactionCount()

	_, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err == nil {
		t.Fatal("Compact succeeded, want an error (provider call exhausted)")
	}

	after := s.History()
	if len(after) != len(before) {
		t.Errorf("history mutated on a failed compaction: before=%d after=%d", len(before), len(after))
	}
	if got := s.CompactionCount(); got != beforeCompactCount {
		t.Errorf("CompactionCount = %d, want unchanged at %d", got, beforeCompactCount)
	}

	var failed int
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("EventCompactionFailed count = %d, want 1", failed)
	}

	// Reload: the log must show no compact record — a torn/aborted
	// compaction is indistinguishable from "never started" (§3 "Crash
	// discipline").
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.CompactionCount(); got != 0 {
		t.Errorf("reloaded CompactionCount = %d, want 0 (failed compaction never journaled)", got)
	}
	if len(loaded.History()) != len(before) {
		t.Errorf("reloaded history = %d messages, want %d (unchanged)", len(loaded.History()), len(before))
	}
}

// TestCompactEmptySummarySkipsGracefully is the red-first test for the
// 2026-08-19 live incident (session ses_jumpy-pizza): a follow-up compact
// with keep_turns=2 (fold range dominated by a prior compaction summary
// plus a couple of real turns) returned
// `{"error":"engine: compaction summary was empty"}` — a summarization
// call that completed without a transport/stream error, but produced no
// usable text, was treated identically to a hard failure and surfaced as
// an HTTP 500 to an operator manually unwedging the session via repeated
// POST /session/{id}/compact calls. An empty summary must instead be a
// clean no-op skip: Compact returns no error and the same TurnsFolded==0
// shape §2's minimum-fold rule already uses for "nothing worth folding" —
// no history mutation, no journal write — while EventCompactionFailed
// still fires so the attempt remains visible to anything tailing events.
func TestCompactEmptySummarySkipsGracefully(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		// The model returns nothing, but the call still cost real,
		// distinguishable tokens (777/3, chosen to be unmistakable against
		// the ordinary turns' usage above) — see the usage-accounting
		// assertion below (review follow-up on PR #136, Finding A).
		compactSummaryTurn("", provider.Usage{InputTokens: 777, OutputTokens: 3}),
	}}
	dir := t.TempDir()
	var evs []Event
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
		OnEvent:    func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	before := s.History()
	beforeCount := s.CompactionCount()
	beforeUsage := s.Usage()
	evs = nil // discard the two ordinary turns' events

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("Compact with an empty summary returned an error, want a graceful skip: %v", err)
	}
	if res.TurnsFolded != 0 {
		t.Errorf("TurnsFolded = %d, want 0", res.TurnsFolded)
	}
	if res.SkipReason != SkipReasonSummarizerEmpty {
		t.Errorf("SkipReason = %q, want %q (review follow-up on PR #136, Finding A/C)", res.SkipReason, SkipReasonSummarizerEmpty)
	}

	// The empty-summary call still cost real tokens and must not vanish
	// from Session.Usage() (review follow-up on PR #136, Finding A: this
	// path used to drop the summarizer call's usage entirely).
	afterUsage := s.Usage()
	if got := afterUsage.InputTokens - beforeUsage.InputTokens; got != 777 {
		t.Errorf("InputTokens delta = %d, want 777 (the empty summarizer call's own usage must still be accounted)", got)
	}
	if got := afterUsage.OutputTokens - beforeUsage.OutputTokens; got != 3 {
		t.Errorf("OutputTokens delta = %d, want 3", got)
	}

	after := s.History()
	if len(after) != len(before) {
		t.Errorf("history mutated on an empty-summary compaction: before=%d after=%d", len(before), len(after))
	}
	if got := s.CompactionCount(); got != beforeCount {
		t.Errorf("CompactionCount = %d, want unchanged at %d", got, beforeCount)
	}

	var failed int
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("EventCompactionFailed count = %d, want 1 (observability preserved even though Compact itself did not error)", failed)
	}

	// Reload: an empty-summary skip must never be journaled, exactly like
	// any other aborted compaction (see TestCompactFailureNoJournalNoMutation).
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.CompactionCount(); got != 0 {
		t.Errorf("reloaded CompactionCount = %d, want 0", got)
	}
	if len(loaded.History()) != len(before) {
		t.Errorf("reloaded history = %d messages, want %d (unchanged)", len(loaded.History()), len(before))
	}
}

// TestCompactSkipsLoneExistingSummaryRangeWithoutCallingProvider is the
// red-first test for the cheap defense alongside the empty-summary skip
// above: a fold range whose ENTIRE content is a single earlier
// compaction's own summary message (no other turn alongside it) is
// detected BEFORE ever calling the provider — re-summarizing an
// already-compressed summary with nothing new to fold in has nothing to
// gain, the same "nothing worth folding" case §2's minimum-fold rule
// already covers for too-few-turns. This is the cheap, structural half of
// the empty-summary incident's root cause (a small keep_turns landing a
// fold range dominated by a prior summary); the graceful-skip fix above
// covers every other reason the model might return nothing.
func TestCompactSkipsLoneExistingSummaryRangeWithoutCallingProvider(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}), // folds turn 1 into a summary
		compactTurn("three", provider.Usage{InputTokens: 10}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1}); err != nil {
		t.Fatalf("first compact: %v", err)
	}
	// History is now: [summary, turn2-user, turn2-asst]. Run one more turn
	// so a second compact has a fold boundary landing exactly on the lone
	// summary message: starts = [0 (summary), 1 (turn2), 3 (turn3)];
	// keep_turns=2 folds exactly starts[0:1] = just the summary.
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatalf("third turn: %v", err)
	}
	requestsBefore := len(prov.requests)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2})
	if err != nil {
		t.Fatalf("second compact: %v", err)
	}
	if res.TurnsFolded != 0 {
		t.Errorf("TurnsFolded = %d, want 0 (lone existing-summary range, nothing to gain)", res.TurnsFolded)
	}
	if res.SkipReason != SkipReasonLoneExistingSummary {
		t.Errorf("SkipReason = %q, want %q (review follow-up on PR #136, Finding A/C)", res.SkipReason, SkipReasonLoneExistingSummary)
	}
	if got := len(prov.requests); got != requestsBefore {
		t.Errorf("provider calls = %d, want %d (no summarization call for a lone existing-summary range)", got, requestsBefore)
	}
}

// TestCompactUserMessageStartingWithBannerTextStillFolds is the red-first
// test for the review follow-up on PR #136, Finding B: isLoneExistingSummary
// used to match on CompactionSummaryBanner's TEXT alone (RoleUser +
// strings.HasPrefix), which a user-typed or pasted message can trivially
// collide with — e.g. pasting a transcript that itself contains an earlier
// compaction summary. A false match skips that range FOREVER without ever
// calling the provider; under the automatic trigger the session then never
// compacts again, the exact exhaustion mode this PR exists to fix, just
// reached via message content instead of a summarizer bug.
// isLoneExistingSummary must gate on isCompactionSummaryID's structural ID
// marker instead, so a genuine user message with real content to fold is
// never mistaken for a lone existing summary.
func TestCompactUserMessageStartingWithBannerTextStillFolds(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("real gist of the pasted message", provider.Usage{InputTokens: 5}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})

	// A genuine user message — an ordinary "msg_"-prefixed ID, never a
	// compaction summary's "cmpsum_" one — whose text happens to start with
	// the exact banner string. Constructed directly (rather than via
	// runTurns) so the fold range lands on exactly this one message with no
	// assistant reply beside it: the same single-message-turn shape a real
	// lone existing summary has, and the only shape isLoneExistingSummary
	// ever considers (see its len(folded) != 1 guard).
	s.mu.Lock()
	s.history = []message.Message{
		{ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: CompactionSummaryBanner + "please continue from here"}}},
		{ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "go"}}},
		{ID: newID("msg"), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "reply"}}},
	}
	s.mu.Unlock()

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if res.TurnsFolded != 1 {
		t.Errorf("TurnsFolded = %d, want 1 (a user-authored message that happens to start with the banner must still be folded, not skipped as a lone existing summary)", res.TurnsFolded)
	}
	if res.SkipReason != "" {
		t.Errorf("SkipReason = %q, want empty (a real fold happened)", res.SkipReason)
	}
	if got := len(prov.requests); got != 1 {
		t.Errorf("provider calls = %d, want 1 (the summarizer must actually be called for this range)", got)
	}
}

// TestCompactSummaryFlowsThroughEventMessageBeforeHistoryCompacted is the
// red-first test for §4's "Live event surface": a successful compaction
// emits the summary via the ordinary EventMessage path FIRST, then
// EventHistoryCompacted — never the other order, or an events.jsonl tailer
// would hold a dangling summary_id it never received a message for.
func TestCompactSummaryFlowsThroughEventMessageBeforeHistoryCompacted(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	var evs []Event
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		OnEvent:   func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	evs = nil // discard the two ordinary turns' events

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}

	var messageIdx, compactedIdx = -1, -1
	for i, ev := range evs {
		switch ev.Type {
		case EventMessage:
			if ev.Message != nil && ev.Message.ID == res.Summary.ID {
				messageIdx = i
			}
		case EventHistoryCompacted:
			compactedIdx = i
		}
	}
	if messageIdx == -1 {
		t.Fatal("no EventMessage carrying the summary was emitted")
	}
	if compactedIdx == -1 {
		t.Fatal("no EventHistoryCompacted was emitted")
	}
	if messageIdx >= compactedIdx {
		t.Errorf("EventMessage(summary) at %d, EventHistoryCompacted at %d; want message strictly before", messageIdx, compactedIdx)
	}
	last := evs[compactedIdx]
	if last.CompactFirstID != res.FirstID || last.CompactLastID != res.LastID ||
		last.CompactTurnsFolded != res.TurnsFolded || last.CompactSummaryID != res.Summary.ID {
		t.Errorf("EventHistoryCompacted = %+v, want it to carry the compact result", last)
	}
}

// TestCompactionStartedPrecedesSummaryAndHistoryCompacted is the red-first
// test for EventCompactionStarted (docs/design/context-compaction.md §4
// "Live event surface"): on a successful, triggered compaction, exactly one
// compaction.started fires, strictly before both the summary's EventMessage
// and the closing EventHistoryCompacted — i.e. before the summary result is
// even available — and it carries the same CompactFirstID/CompactLastID/
// CompactTurnsFolded the eventual EventHistoryCompacted carries, so a client
// can correlate "compacting now" with "compaction settled".
func TestCompactionStartedPrecedesSummaryAndHistoryCompacted(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
	}}
	var evs []Event
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		OnEvent:   func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	evs = nil // discard the two ordinary turns' events

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}

	var startedIdx, messageIdx, compactedIdx = -1, -1, -1
	var startedCount int
	for i, ev := range evs {
		switch ev.Type {
		case EventCompactionStarted:
			startedCount++
			startedIdx = i
		case EventMessage:
			if ev.Message != nil && ev.Message.ID == res.Summary.ID {
				messageIdx = i
			}
		case EventHistoryCompacted:
			compactedIdx = i
		}
	}
	if startedCount != 1 {
		t.Fatalf("EventCompactionStarted count = %d, want exactly 1", startedCount)
	}
	if messageIdx == -1 {
		t.Fatal("no EventMessage carrying the summary was emitted")
	}
	if compactedIdx == -1 {
		t.Fatal("no EventHistoryCompacted was emitted")
	}
	if startedIdx >= messageIdx {
		t.Errorf("EventCompactionStarted at %d, summary EventMessage at %d; want started strictly before the summary is even available", startedIdx, messageIdx)
	}
	if startedIdx >= compactedIdx {
		t.Errorf("EventCompactionStarted at %d, EventHistoryCompacted at %d; want started strictly before", startedIdx, compactedIdx)
	}

	started := evs[startedIdx]
	if started.CompactFirstID != res.FirstID || started.CompactLastID != res.LastID || started.CompactTurnsFolded != res.TurnsFolded {
		t.Errorf("EventCompactionStarted = %+v, want CompactFirstID=%q CompactLastID=%q CompactTurnsFolded=%d (matching the eventual result)",
			started, res.FirstID, res.LastID, res.TurnsFolded)
	}
	if started.CompactSummaryID != "" {
		t.Errorf("EventCompactionStarted.CompactSummaryID = %q, want empty (the summary does not exist yet when started fires)", started.CompactSummaryID)
	}
}

// TestCompactionStartedNeverOrphanedOnFailure is the red-first test for
// EventCompactionStarted's pairing invariant on the failure path: a
// compaction that fails AFTER starting (the summarization call itself
// errors) still emits exactly one EventCompactionStarted, and it is always
// followed by an EventCompactionFailed — started must never be left
// dangling with no terminal event. Reuses
// TestCompactFailureNoJournalNoMutation's setup: only two turns are
// scripted, so the third provider.Stream call (the summarization call)
// exhausts p.turns and fails.
func TestCompactionStartedNeverOrphanedOnFailure(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 10}),
	}}
	var evs []Event
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: t.TempDir(),
		OnEvent:    func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 2)
	evs = nil // discard the two ordinary turns' events

	_, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err == nil {
		t.Fatal("Compact succeeded, want an error (provider call exhausted)")
	}

	var startedIdx, failedIdx = -1, -1
	var startedCount, failedCount int
	for i, ev := range evs {
		switch ev.Type {
		case EventCompactionStarted:
			startedCount++
			startedIdx = i
		case EventCompactionFailed:
			failedCount++
			failedIdx = i
		}
	}
	if startedCount != 1 {
		t.Fatalf("EventCompactionStarted count = %d, want exactly 1", startedCount)
	}
	if failedCount != 1 {
		t.Fatalf("EventCompactionFailed count = %d, want exactly 1 (started must never be orphaned)", failedCount)
	}
	if startedIdx >= failedIdx {
		t.Errorf("EventCompactionStarted at %d, EventCompactionFailed at %d; want started strictly before failed", startedIdx, failedIdx)
	}
}

// TestCompactionStartedNotEmittedOnEarlyReturnSkip is the red-first test
// for EventCompactionStarted's other half of its pairing invariant: a
// compact call that skips BEFORE ever committing to a summary attempt
// (fewer than the effective keep-turns floor's worth of complete turns —
// SkipReasonNotEnoughTurns, the cheapest of the two early-return skips —
// never calls the provider) must not emit EventCompactionStarted at all —
// only a call that is actually going to attempt a summary ever fires it.
func TestCompactionStartedNotEmittedOnEarlyReturnSkip(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	var evs []Event
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
		OnEvent:   func(ev Event) { evs = append(evs, ev) },
	})
	runTurns(t, s, 1)
	evs = nil // discard the one ordinary turn's events

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.SkipReason != SkipReasonNotEnoughTurns {
		t.Fatalf("SkipReason = %q, want %q", res.SkipReason, SkipReasonNotEnoughTurns)
	}

	for _, ev := range evs {
		if ev.Type == EventCompactionStarted {
			t.Fatalf("EventCompactionStarted emitted on a not-enough-turns skip, want none (the provider was never called)")
		}
	}
}

// TestCompactSurvivesReload is the red-first restart test for §2's
// "LoadSession replay": a reloaded session replays the compact record and
// the trimmed history — the summary lands exactly where it did live, and
// cumulative usage (including the summarization spend) survives, but
// LastUsage does not pick up the summarization call's tiny usage.
func TestCompactSurvivesReload(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 100, OutputTokens: 10}),
		compactTurn("two", provider.Usage{InputTokens: 200, OutputTokens: 10}),
		compactTurn("three", provider.Usage{InputTokens: 300, OutputTokens: 10}),
		compactSummaryTurn("the gist of turns one and two", provider.Usage{InputTokens: 9, OutputTokens: 4}),
	}}
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	})
	runTurns(t, s, 3)

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantHistory := s.History()
	wantUsage := s.Usage()
	wantLast, _ := s.LastUsage()
	wantCount := s.CompactionCount()
	wantLastCompactedAt := s.LastCompactedAt()

	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotHistory := loaded.History()
	if len(gotHistory) != len(wantHistory) {
		t.Fatalf("reloaded history = %d messages, want %d", len(gotHistory), len(wantHistory))
	}
	for i := range wantHistory {
		if gotHistory[i].ID != wantHistory[i].ID || gotHistory[i].Role != wantHistory[i].Role ||
			gotHistory[i].Parts.Text() != wantHistory[i].Parts.Text() {
			t.Errorf("reloaded history[%d] = %+v, want %+v", i, gotHistory[i], wantHistory[i])
		}
	}
	if got := loaded.Usage(); got != wantUsage {
		t.Errorf("reloaded Usage = %+v, want %+v", got, wantUsage)
	}
	last, ok := loaded.LastUsage()
	if !ok || last != wantLast {
		t.Errorf("reloaded LastUsage = %+v (ok=%v), want %+v", last, ok, wantLast)
	}
	if got := loaded.CompactionCount(); got != wantCount {
		t.Errorf("reloaded CompactionCount = %d, want %d", got, wantCount)
	}
	if !loaded.LastCompactedAt().Equal(wantLastCompactedAt) {
		t.Errorf("reloaded LastCompactedAt = %v, want %v", loaded.LastCompactedAt(), wantLastCompactedAt)
	}
	if res.TurnsFolded != 2 {
		t.Fatalf("sanity: TurnsFolded = %d, want 2", res.TurnsFolded)
	}

	// A post-compaction session must restart cleanly and keep working: a
	// further Prompt on the reloaded session must succeed.
	prov.turns = append(prov.turns, compactTurn("four", provider.Usage{InputTokens: 50, OutputTokens: 5}))
	if _, err := loaded.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatalf("Prompt on reloaded post-compaction session: %v", err)
	}
}

// TestCompactCorruptRangeIsLoadError is the red-first test for §2's "Not
// found is treated as corruption" rule: a compact record naming a
// first_id/last_id pair that is not present in the accumulated history is
// an explicit LoadSession error, never a silent best-effort guess.
func TestCompactCorruptRangeIsLoadError(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("one", provider.Usage{InputTokens: 10}),
	}}
	cfg := Config{
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir: dir,
	}
	s := NewSession(cfg)
	if _, err := s.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.persistCompactLocked("msg_does_not_exist", "msg_also_missing", 1, message.Message{
		ID: newID("msg"), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "x"}},
	}, provider.Usage{})
	s.mu.Unlock()

	if _, err := LoadSession(cfg, s.ID); err == nil {
		t.Fatal("LoadSession succeeded on a corrupt compact record range, want an error")
	}
}

// nep5292FixtureLines is the exact reproduction journal from NEP-5292: three
// turns, the first turn's assistant message carrying a tool_call ("A") with
// no matching tool_result — the orphan message.ResolveOrphanToolCalls
// repairs at every LoadSession, in memory only. With keepTurns=2 the fold
// boundary lands exactly on that in-memory-only synthetic message.
const nep5292FixtureLines = `{"type":"message","message":{"id":"msg_1","role":"user","parts":[{"type":"text","text":"task 1"}]}}
{"type":"message","message":{"id":"msg_2","role":"assistant","parts":[{"type":"tool_call","call_id":"A","name":"bash","arguments":{}}]}}
{"type":"message","message":{"id":"msg_3","role":"user","parts":[{"type":"text","text":"task 2"}]}}
{"type":"message","message":{"id":"msg_4","role":"assistant","parts":[{"type":"text","text":"done"}]}}
{"type":"message","message":{"id":"msg_5","role":"user","parts":[{"type":"text","text":"task 3"}]}}
{"type":"message","message":{"id":"msg_6","role":"assistant","parts":[{"type":"text","text":"done"}]}}
`

// writeNEP5292Fixture writes the reproduction journal above under id, with a
// session header line so it satisfies every other reader's expectations too.
func writeNEP5292Fixture(t *testing.T, dir, id string) {
	t.Helper()
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// nep5292RawHistory is the exact message.Message values nep5292FixtureLines
// encodes, built directly (not by parsing JSON) for tests that need to feed
// them to spliceCompact without going through LoadSession at all — this is
// what ANY binary's scan loop, old or new, sees before
// message.ResolveOrphanToolCalls ever runs.
func nep5292RawHistory() []message.Message {
	return []message.Message{
		{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 1"}}},
		{ID: "msg_2", Role: message.RoleAssistant, Parts: message.Parts{&message.ToolCall{CallID: "A", Name: "bash", Arguments: json.RawMessage("{}")}}},
		{ID: "msg_3", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 2"}}},
		{ID: "msg_4", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
		{ID: "msg_5", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "task 3"}}},
		{ID: "msg_6", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "done"}}},
	}
}

// TestCompactNeverJournalsSyntheticOrphanID is the red-first test for Part A
// of NEP-5292's fix: Session.Compact must never persist a fold boundary ID
// that names a message.ResolveOrphanToolCalls synthetic repair message —
// that message exists only in this process's live memory (see
// engine/store.go's LoadSession, which applies the repair AFTER replay) and
// is never itself persisted, so a journal record naming one is corrupt on
// arrival: no future LoadSession will ever find it.
//
// It reproduces the exact mechanism from the issue: loading
// nep5292FixtureLines leaves an orphaned tool_call at msg_2, which
// LoadSession's ResolveOrphanToolCalls repair turns into a synthetic
// RoleTool message at live history index 2. A keepTurns=2 compact folds
// exactly turn 1 (indices 0-2), so the naive fold-end id would be that
// synthetic message's — this test asserts the FIXED Compact instead
// journals msg_2 (the nearest real, persisted message before it), and that
// the result reloads cleanly to the exact same kept history the live
// process already has.
func TestCompactNeverJournalsSyntheticOrphanID(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000001"
	writeNEP5292Fixture(t, dir, id)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	cfg := Config{
		SessionDir: dir,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
	}

	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession = %v", err)
	}

	before := s.History()
	if len(before) != 7 {
		t.Fatalf("history length = %d, want 7 (6 raw messages + 1 synthetic repair)", len(before))
	}
	if !message.IsSyntheticOrphanID(before[2].ID) {
		t.Fatalf("history[2].ID = %q, want a synthetic orphan-repair id (the mechanism this test guards)", before[2].ID)
	}

	res, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2})
	if err != nil {
		t.Fatalf("Compact = %v", err)
	}
	if res.TurnsFolded != 1 {
		t.Fatalf("TurnsFolded = %d, want 1 (matches the issue's reproduction)", res.TurnsFolded)
	}
	if message.IsSyntheticOrphanID(res.LastID) {
		t.Fatalf("Compact result names a synthetic LastID: %q, want a real, persisted id", res.LastID)
	}
	if res.LastID != "msg_2" {
		t.Errorf("LastID = %q, want %q (the nearest real message before the synthetic one)", res.LastID, "msg_2")
	}
	liveAfter := s.History()

	// The journaled record itself must be clean, not merely what
	// LoadSession happens to recover afterward: read the raw on-disk line
	// directly, bypassing LoadSession's own heal path entirely.
	raw, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last struct {
		Type    string `json:"type"`
		Compact *struct {
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		} `json:"compact"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("unmarshal last journal line: %v", err)
	}
	if last.Type != recCompact || last.Compact == nil {
		t.Fatalf("last journal line = %q, want a %q record", lines[len(lines)-1], recCompact)
	}
	if message.IsSyntheticOrphanID(last.Compact.LastID) {
		t.Fatalf("on-disk compact record last_id is synthetic: %q", last.Compact.LastID)
	}

	// Equivalence (the issue's core claim, verified): the synthetic message
	// never existed in raw replayed history, so folding it live while
	// journaling the last real id before it produces IDENTICAL kept
	// history on a fresh reload.
	reloaded, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("reload after compact: %v", err)
	}
	reloadedHistory := reloaded.History()
	if len(reloadedHistory) != len(liveAfter) {
		t.Fatalf("reloaded history = %d messages, want %d (live post-compact)", len(reloadedHistory), len(liveAfter))
	}
	for i := range liveAfter {
		if reloadedHistory[i].ID != liveAfter[i].ID ||
			reloadedHistory[i].Role != liveAfter[i].Role ||
			reloadedHistory[i].Parts.Text() != liveAfter[i].Parts.Text() {
			t.Errorf("reloaded history[%d] = %+v, want %+v", i, reloadedHistory[i], liveAfter[i])
		}
	}
}

// TestCompactNewRecordReplaysIdenticallyWithoutHealPath is the version-skew
// half of NEP-5292's fix: an OLD binary — one with no heal path at all,
// calling spliceCompact directly and never message.IsSyntheticOrphanID —
// must still replay a compact record written by the FIXED Compact
// correctly. This is what makes downgrading to an old binary after this fix
// safe: Part A never introduces a new record field or record type (the
// compactRecord shape is untouched), it only changes WHICH real, already-
// persisted message id LastID names. This test proves that value is always
// resolvable by the bare, unhealed spliceCompact function — simulating the
// old binary directly, never through LoadSession's own (new) heal path —
// and that doing so lands on the exact same kept history the live process
// already has.
//
// The named claim is "an old binary replays the JOURNALED record" — so this
// test must drive spliceCompact from the bytes persistCompactLocked actually
// wrote, not from CompactResult. CompactResult is populated independently
// (compact.go's return statement, not its persistCompactLocked call), so a
// bug that journals the wrong IDs while still returning the right
// CompactResult would slip past a version read from res. Read the raw
// on-disk line directly, the same way TestCompactNeverJournalsSyntheticOrphanID
// does, and use ITS FirstID/LastID/Summary.
func TestCompactNewRecordReplaysIdenticallyWithoutHealPath(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000003"
	writeNEP5292Fixture(t, dir, id)

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("SUMMARY", provider.Usage{InputTokens: 5}),
	}}
	cfg := Config{
		SessionDir: dir,
		Providers:  provider.Registry{"test": prov},
		Model:      message.ModelRef{Provider: "test", Model: "m1"},
	}
	s, err := LoadSession(cfg, id)
	if err != nil {
		t.Fatalf("LoadSession = %v", err)
	}
	if _, err := s.Compact(context.Background(), CompactOptions{KeepTurns: 2}); err != nil {
		t.Fatalf("Compact = %v", err)
	}
	liveAfter := s.History()

	// Read the raw on-disk line directly, bypassing both CompactResult and
	// LoadSession's own heal path entirely — this is the exact record an old
	// binary would read from disk.
	raw, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var last record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("unmarshal last journal line: %v", err)
	}
	if last.Type != recCompact || last.Compact == nil {
		t.Fatalf("last journal line = %q, want a %q record", lines[len(lines)-1], recCompact)
	}

	// The exact old-binary code path: plain spliceCompact against raw
	// pre-compact history, using the journaled ids/summary verbatim (read
	// from disk above, not from CompactResult), no heal function ever
	// called or even in scope.
	oldSpliced, err := spliceCompact(nep5292RawHistory(), last.Compact.FirstID, last.Compact.LastID, last.Compact.Summary)
	if err != nil {
		t.Fatalf("old-binary-equivalent spliceCompact = %v, want success (on-disk LastID must be a real, persisted id an old binary can find)", err)
	}
	oldFinal := message.ResolveOrphanToolCalls(oldSpliced)

	if len(oldFinal) != len(liveAfter) {
		t.Fatalf("old-binary-equivalent history = %d messages, want %d (live post-compact)", len(oldFinal), len(liveAfter))
	}
	for i := range liveAfter {
		if oldFinal[i].ID != liveAfter[i].ID ||
			oldFinal[i].Role != liveAfter[i].Role ||
			oldFinal[i].Parts.Text() != liveAfter[i].Parts.Text() {
			t.Errorf("old-binary-equivalent history[%d] = %+v, want %+v", i, oldFinal[i], liveAfter[i])
		}
	}
}

// TestLoadSessionHealsPhantomSyntheticCompactLastID is the red-first test
// for Part B of NEP-5292's fix: a journal ALREADY containing a phantom
// synthetic LastID (written by an unpatched build, before Part A existed)
// must still load — LoadSession re-derives the fold end from FirstID plus
// the record's own turns_folded count instead of failing outright. The
// journal here is written by hand, not produced by Session.Compact, to
// guarantee it exercises the phantom-id shape rather than whatever the
// (already fixed) live path would now produce.
func TestLoadSessionHealsPhantomSyntheticCompactLastID(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000002"
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines +
		`{"type":"compact","compact":{"first_id":"msg_1","last_id":"synthetic-orphan-tool-result-1-A","turns_folded":1,"summary":{"id":"msg_summary","role":"user","parts":[{"type":"text","text":"[compacted summary of earlier conversation]\n\nthe gist"}]}}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSession(Config{SessionDir: dir}, id)
	if err != nil {
		t.Fatalf("LoadSession = %v, want the phantom synthetic last_id healed instead of a hard error", err)
	}
	got := s.History()
	if len(got) != 5 {
		t.Fatalf("history length = %d, want 5 (summary + 4 kept messages: msg_3..msg_6)", len(got))
	}
	if got[0].ID != "msg_summary" {
		t.Errorf("history[0].ID = %q, want %q (the summary)", got[0].ID, "msg_summary")
	}
	if got[0].Role != message.RoleUser {
		t.Errorf("history[0].Role = %s, want RoleUser", got[0].Role)
	}
	wantKeptIDs := []string{"msg_3", "msg_4", "msg_5", "msg_6"}
	for i, want := range wantKeptIDs {
		if got[i+1].ID != want {
			t.Errorf("history[%d].ID = %q, want %q", i+1, got[i+1].ID, want)
		}
	}
}

// TestLoadSessionCompactPhantomLastIDFailsLoudlyWhenUnhealable is the
// explicit-error half of Part B: if the heal itself is impossible (here,
// first_id also does not name a real message), LoadSession must still fail
// loudly — never silently drop history — exactly as an un-healable corrupt
// range already does (see TestCompactCorruptRangeIsLoadError).
func TestLoadSessionCompactPhantomLastIDFailsLoudlyWhenUnhealable(t *testing.T) {
	dir := t.TempDir()
	id := "ses_5292000000000004"
	data := `{"type":"session","id":"` + id + `","created_at":"2025-01-02T03:04:05Z"}
` + nep5292FixtureLines +
		`{"type":"compact","compact":{"first_id":"msg_does_not_exist","last_id":"synthetic-orphan-tool-result-1-A","turns_folded":1,"summary":{"id":"msg_summary","role":"user","parts":[{"type":"text","text":"x"}]}}}
`
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadSession(Config{SessionDir: dir}, id); err == nil {
		t.Fatal("LoadSession succeeded on an unhealable phantom last_id (first_id also missing), want an error")
	}
}

// TestMaybeAutoCompactTriggersAndHysteresisPreventsThrash is the red-first
// test for §1's automatic trigger and §2's churn-guard hysteresis: crossing
// the threshold fires exactly one compaction; a second consecutive
// over-threshold turn (no intervening dip) does NOT re-fire; a dip below
// the threshold clears the guard so a later crossing can fire again.
func TestMaybeAutoCompactTriggersAndHysteresisPreventsThrash(t *testing.T) {
	over := provider.Usage{InputTokens: 900}
	under := provider.Usage{InputTokens: 100}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", under), // call 1: no lastUsage yet, no auto-compact possible
		compactTurn("t2", over),  // call 2: lastUsage(t1)=under, no trigger
		compactSummaryTurn("gist-1", provider.Usage{InputTokens: 5}), // triggered before call 3 (lastUsage(t2)=over)
		compactTurn("t3", over),  // call 3's own turn (post first compaction)
		compactTurn("t4", under), // call 4: lastUsage(t3)=over but on cooldown, no trigger
		compactTurn("t5", over),  // call 5: lastUsage(t4)=under, cooldown clears, no trigger (not over)
		compactSummaryTurn("gist-2", provider.Usage{InputTokens: 5}), // triggered before call 6 (lastUsage(t5)=over)
		compactTurn("t6", under),                                     // call 6's own turn (post second compaction)
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 6)

	if got := s.CompactionCount(); got != 2 {
		t.Fatalf("CompactionCount = %d, want exactly 2 (hysteresis must have suppressed a third)", got)
	}
	if len(prov.requests) != 8 {
		t.Fatalf("provider calls = %d, want 8 (6 worker turns + 2 compaction summaries)", len(prov.requests))
	}
}

// TestMaybeAutoCompactEmptySummaryLatchesHysteresis is the red-first test
// for the review follow-up on PR #136, Finding A: the empty-summary no-op
// costs a full, billed provider call but used to set no hysteresis (only
// TurnsFolded > 0 latched it). Once auto-compaction is armed, a session
// whose summarizer returns empty re-issued a full summarization call, at
// full input price, on EVERY subsequent over-threshold turn, indefinitely —
// a silent recurring-spend bug, not a free no-op. The fix: latch the churn
// guard on SkipReasonSummarizerEmpty too, so the second over-threshold turn
// after an empty-summary skip must NOT issue another summarization call.
func TestMaybeAutoCompactEmptySummaryLatchesHysteresis(t *testing.T) {
	over := provider.Usage{InputTokens: 900}
	under := provider.Usage{InputTokens: 100}

	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", under),                               // call 1: no lastUsage yet, no auto-compact possible
		compactTurn("t2", over),                                // call 2: lastUsage(t1)=under, no trigger; lastUsage becomes over
		compactSummaryTurn("", provider.Usage{InputTokens: 5}), // triggered before call 3 (lastUsage(t2)=over); the model returns nothing
		compactTurn("t3", over),                                // call 3's own turn; keeps lastUsage over threshold
		compactTurn("t4", over),                                // call 4's own turn: hysteresis must suppress a second summarization call here
		compactSummaryTurn("buffer-if-hysteresis-did-not-latch", provider.Usage{InputTokens: 5}), // spare slot: only consumed if the bug re-triggers a second summarizer call before call 4
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 4)

	var summaryCalls int
	for _, req := range prov.requests {
		if len(req.System) > 0 && req.System[0] == compactionSystemPrompt {
			summaryCalls++
		}
	}
	if summaryCalls != 1 {
		t.Errorf("summarization calls = %d, want 1 (an empty-summary no-op must latch hysteresis so a still-over-threshold turn never re-triggers it — review follow-up on PR #136, Finding A)", summaryCalls)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (the summarizer never returned anything usable in this test)", got)
	}
}

// TestMaybeAutoCompactDisabledByDefault is the red-first test for the
// opt-in gate: Config.ContextWindowTokens's zero value (a fresh Config)
// disables automatic compaction entirely, so a huge LastUsage never
// triggers it — no existing deployment changes behavior by upgrading (§5
// "Non-goals").
func TestMaybeAutoCompactDisabledByDefault(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", provider.Usage{InputTokens: 999_999}),
		compactTurn("t2", provider.Usage{InputTokens: 999_999}),
	}}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: "test", Model: "m1"},
	})
	runTurns(t, s, 2)
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (ContextWindowTokens unset)", got)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (no compaction summary calls)", len(prov.requests))
	}
}

// TestIncidentRecoverableByCompaction is the red-first regression test for
// the production incident: a goal session died at 205102 tokens > 200000
// maximum ("invalid_request_error: prompt is too long") and was
// unrecoverable afterward. With ContextWindowTokens configured, the
// automatic trigger must fold history BEFORE the next request would repeat
// that identical, deterministic failure — turning the incident's shape into
// a recoverable one instead of a dead session.
func TestIncidentRecoverableByCompaction(t *testing.T) {
	// Three prior worker turns, the last one landing at the incident's exact
	// input-token count, followed by the automatic compaction's own
	// summarization call, then a worker turn that must now succeed instead
	// of repeating the "prompt is too long" failure.
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("t1", provider.Usage{InputTokens: 50_000, OutputTokens: 500}),
		compactTurn("t2", provider.Usage{InputTokens: 120_000, OutputTokens: 500}),
		compactTurn("t3", provider.Usage{InputTokens: 205_102, OutputTokens: 500}), // the incident's exact figure
		compactSummaryTurn("summary of the first two turns", provider.Usage{InputTokens: 4_000, OutputTokens: 200}),
		compactTurn("t4", provider.Usage{InputTokens: 30_000, OutputTokens: 500}), // succeeds: history was trimmed first
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		ContextWindowTokens: 200_000, // the incident's exact maximum
		CompactionKeepTurns: 1,
	})
	runTurns(t, s, 3)

	last, ok := s.LastUsage()
	if !ok || last.InputTokens != 205_102 {
		t.Fatalf("LastUsage = %+v (ok=%v), want the incident's 205102 input tokens", last, ok)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount = %d before the 4th call, want 0", got)
	}

	// Pre-fix, this 4th call would resend the full, now-over-limit history
	// and die identically ("prompt is too long"). Post-fix, maybeAutoCompact
	// folds the oldest turns first, so the request this turn actually sends
	// is far smaller — the incident's exact failure mode never recurs.
	if _, err := s.Prompt(context.Background(), "keep going"); err != nil {
		t.Fatalf("Prompt on a session over the context-window threshold: %v (must be recoverable by compaction, not fatal)", err)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount after the 4th call = %d, want 1 (automatic compaction must have fired)", got)
	}
	finalReq := prov.requests[len(prov.requests)-1]
	if len(finalReq.Messages) >= 6 { // pre-compaction full history would have been >= 6 messages (3 turns)
		t.Errorf("final request carried %d messages, want a trimmed history (compaction folded the old turns)", len(finalReq.Messages))
	}
}

// goalCompactProvider serves the goal loop's worker turns, its independent
// tool-less evaluator, AND compaction's own tool-less summarization call —
// classifying by System content (the compaction system prompt is a unique
// marker no evaluator request ever carries) and, failing that, by whether
// Tools is empty (the evaluator's request, per goalProvider in
// goal_test.go). This is what "no goal.go changes needed" (docs/design/
// context-compaction.md §1) means in practice: PursueGoal drives everything
// through Prompt, so the automatic trigger fires mid-goal-loop with no
// special-casing at all — this test proves it end to end.
type goalCompactProvider struct {
	worker, eval, summary [][]provider.Event
	wi, ei, si            int
	requests              []*provider.Request
}

func (p *goalCompactProvider) Name() string { return "test" }

func (p *goalCompactProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	if len(req.System) == 1 && req.System[0] == compactionSystemPrompt {
		if p.si >= len(p.summary) {
			return nil, io.ErrUnexpectedEOF
		}
		ev := p.summary[p.si]
		p.si++
		return &scriptedStream{events: ev}, nil
	}
	if len(req.Tools) == 0 {
		if p.ei >= len(p.eval) {
			return &scriptedStream{}, nil
		}
		ev := p.eval[p.ei]
		p.ei++
		return &scriptedStream{events: ev}, nil
	}
	if p.wi >= len(p.worker) {
		return &scriptedStream{}, nil
	}
	ev := p.worker[p.wi]
	p.wi++
	return &scriptedStream{events: ev}, nil
}

// TestPursueGoalAutoCompactsMidLoop is the red-first test for §1's "no
// separate scheduler, no goal.go changes needed": a goal loop, driven
// entirely through the ordinary Prompt path, auto-compacts mid-loop exactly
// like a bare prompt_async session would, and still reaches its goal
// afterward.
func TestPursueGoalAutoCompactsMidLoop(t *testing.T) {
	prov := &goalCompactProvider{
		worker: [][]provider.Event{
			compactTurn("working turn 1", provider.Usage{InputTokens: 100}),
			compactTurn("working turn 2", provider.Usage{InputTokens: 900}), // over threshold
			compactTurn("working turn 3", provider.Usage{InputTokens: 100}), // proceeds post-compaction
		},
		eval: [][]provider.Event{
			evalTurn("NOT MET: keep going"),
			evalTurn("NOT MET: still going"),
			evalTurn("MET: done"),
		},
		summary: [][]provider.Event{
			compactSummaryTurn("gist of turn 1", provider.Usage{InputTokens: 20}),
		},
	}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: "test", Model: "m1"},
		Instructions:        &InstructionsConfig{Disabled: true},
		SkillsDirs:          []string{},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})

	res, err := s.PursueGoal(context.Background(), "finish the thing", GoalOptions{Evaluator: evalModel})
	if err != nil {
		t.Fatalf("PursueGoal: %v", err)
	}
	if !res.Achieved {
		t.Fatalf("PursueGoal result = %+v, want Achieved", res)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount = %d, want exactly 1 (mid-loop automatic compaction)", got)
	}
}

// seedDelegatedTurn appends one RoleUser/RoleAssistant pair directly to s's
// history, bypassing Prompt entirely — the shape a real claude-code
// delegated turn leaves behind (runClaudeCodeTurn appends plain messages as
// they stream in; only the terminal "result" event's usage, via
// applyClaudeCodeUsage, ever touches s.lastUsage). text is repeated to
// build up byte size cheaply.
func seedDelegatedTurn(s *Session, text string) {
	now := time.Now().UTC()
	s.append(message.Message{ID: ResolveMessageID(""), Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "continue"}}, CreatedAt: now})
	s.append(message.Message{ID: ResolveMessageID(""), Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: text}}, CreatedAt: now})
}

// TestMaybeAutoCompactForcedAfterClaudeCodeToNativeSwitch is the red-first
// regression test for the live incident (session
// ses_01m1kyhka3ewf8vcth0qbqm222): a session delegated to the Claude Code
// CLI for its entire life accumulates a huge harness journal purely as a
// passive record (harness's own automatic compaction is unconditionally
// skipped for a delegated turn — see PromptWithOrigin's early dispatch).
// applyClaudeCodeUsage DOES set s.lastUsage/haveLastUsage on every delegated
// turn, but from the CLI's OWN internal, self-managed context accounting —
// a number with no relationship to harness's own journal size, since the
// CLI runs its own compaction over its own history. When the session is
// switched to a harness-native model, maybeAutoCompact must not trust that
// stale, wrong-scale lastUsage figure: it must estimate straight from
// harness's actual journal (the thing a native request actually transcodes
// and sends) and compact BEFORE the next native provider call, regardless
// of the prior model's delegated flag.
//
// Named failure this pins: pre-fix, maybeAutoCompact reads
// lastUsage.InputTokens (here, a small CLI-reported figure standing in for
// the CLI's own compacted context) as "how big is the next request," sees
// it comfortably under threshold, and never compacts — so the native
// provider receives the full, uncompacted, over-threshold journal as its
// first request. This test fails today because CompactionCount() is 0 and
// the request the (test) native provider actually receives still carries
// every seeded turn.
func TestMaybeAutoCompactForcedAfterClaudeCodeToNativeSwitch(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("gist of the delegated run", provider.Usage{InputTokens: 5}),
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000, // explicit: survives the switch unchanged (SetModel never re-derives it)
		CompactionKeepTurns: 1,
	})

	// Five delegated turns, each long enough that the whole journal's crude
	// byte/4 estimate clears threshold*windowTokens (0.8*1000 = 800).
	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	preSwitchHistoryLen := len(s.History())

	// applyClaudeCodeUsage's real shape: it DOES set lastUsage/haveLastUsage
	// on a delegated turn, but from the CLI's own small internal context —
	// nothing like harness's actual journal size seeded above.
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt after claude-code-to-native switch: %v (must compact and succeed, not fail)", err)
	}

	if got := s.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount after the post-switch turn = %d, want 1 (forced compaction must have run before the native provider call)", got)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2 (1 compaction summary + 1 native worker turn)", len(prov.requests))
	}
	finalReq := prov.requests[len(prov.requests)-1]
	if len(finalReq.Messages) >= preSwitchHistoryLen {
		t.Errorf("final native request carried %d messages (pre-switch history was %d) — forced compaction must have trimmed it before the provider call",
			len(finalReq.Messages), preSwitchHistoryLen)
	}
}

// TestForcedCompactionErrorProceedsToNativeProviderAfterClaudeCodeSwitch is
// the red-first regression test for round-3 review BLOCKING A, using an
// UNCLASSIFIED provider error (a plain out-of-scripted-turns failure, not a
// classified provider.Error — see
// TestMaybeAutoCompactForcedCompactErrorTerminatesAndProceeds for that
// shape) to prove the fix does not depend on error classification: a
// forced compaction pass exists precisely because sending the pre-switch
// journal to a native provider would otherwise overflow it, but its own
// summarization call can fail for any reason. This test used to pin the
// OPPOSITE requirement — that such a failure blocks Prompt with a loud
// compaction error and the real turn's own provider call is NEVER
// attempted — which is exactly the permanent-brick shape BLOCKING A closed
// (see docs/design/context-compaction.md and
// TestForceCompactionCheckClearsAndStaysOffAfterFailedAttempt for the
// retry half). Post-fix, EventCompactionFailed still reports the failure,
// but the real turn's own native provider call IS attempted — it happens
// to fail too here, for its own separate reason (test setup has nothing
// scripted for it either), proving the forced check no longer swallows the
// real turn behind a permanent compaction block.
func TestForcedCompactionErrorProceedsToNativeProviderAfterClaudeCodeSwitch(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	// No scripted turns at all: the forced compaction's own summarization
	// call is the very first Stream call, and it exhausts p.turns
	// immediately (see scriptedProvider.Stream), so Compact fails for a
	// real, unclassified reason (not the benign empty-summary skip).
	prov := &scriptedProvider{name: "test"}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err == nil {
		t.Fatal("Prompt succeeded, want the native provider's own out-of-scripted-turns error (test setup, proves the real turn was reached)")
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (the summarization call itself errored, nothing folded)", got)
	}
	// BOTH calls must have been attempted: the failed compaction summary,
	// then the real turn's own native provider call — pre-fix, only the
	// first ever ran.
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (the failed compaction summary, then the native turn actually reaching the provider)", len(prov.requests))
	}
	var failedReason string
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed && strings.Contains(ev.Text, "forced compaction failed") {
			failedReason = ev.Text
		}
	}
	if failedReason == "" {
		t.Errorf("no EventCompactionFailed naming \"forced compaction failed\" among %d events, want the loud report preserved even though Prompt proceeds to the provider", len(evs))
	}
	s.mu.Lock()
	armed := s.forceCompactionCheck
	s.mu.Unlock()
	if armed {
		t.Error("forceCompactionCheck still true after the terminating Compact-error pass, want it cleared")
	}
}

// TestForceCompactionCheckSurvivesReload is the red-first regression test
// for BLOCKING 1 of the andybons/claude-code-compaction-forced-switch fix
// round: forceCompactionCheck used to be a memory-only Session field,
// deliberately excluded from the journal fold AND the snapshot. The stale
// signal it exists to distrust — a delegated turn's lastUsage, folded in by
// recClaudeCodeUsage — is durable, so any process restart or residency
// eviction between the SetModel switch and the next Prompt lost the arming
// flag while the stale figure survived intact: a reload took the ORDINARY
// branch, trusted the small CLI-reported lastUsage, and forwarded the full,
// never-compacted journal to the native provider — the original incident,
// on a cold session. This seeds a delegated session, switches to a native
// model, then RELOADS from the durable journal instead of continuing to
// use the live *Session (simulating exactly that gap), and prompts on the
// reloaded session.
//
// Named failure this pins: pre-fix, the reloaded session's
// forceCompactionCheck is false (never folded from recModel/recMessage,
// never restored from a snapshot), so CompactionCount stays 0 and the final
// native request still carries every seeded pre-switch message.
func TestForceCompactionCheckSurvivesReload(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("gist of the delegated run", provider.Usage{InputTokens: 5}),
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}}
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		SessionDir:          dir,
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	preSwitchHistoryLen := len(s.History())

	// applyClaudeCodeUsage's real shape (see
	// TestMaybeAutoCompactForcedAfterClaudeCodeToNativeSwitch's identical
	// setup): a small CLI-internal figure, nothing like harness's actual
	// journal size seeded above.
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	// Simulate a residency eviction or a process restart between the
	// switch and the next Prompt: reload from the durable journal instead
	// of continuing to use s.
	loaded, err := LoadSession(s.cfg, s.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := loaded.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt on the reloaded session after a claude-code-to-native switch: %v (must compact and succeed, not fail)", err)
	}

	if got := loaded.CompactionCount(); got != 1 {
		t.Fatalf("CompactionCount after the post-reload turn = %d, want 1 (the reload must have re-derived forceCompactionCheck from the durable recModel/recMessage fold and forced compaction before the native provider call)", got)
	}
	if len(prov.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2 (1 compaction summary + 1 native worker turn)", len(prov.requests))
	}
	finalReq := prov.requests[len(prov.requests)-1]
	if len(finalReq.Messages) >= preSwitchHistoryLen {
		t.Errorf("final native request carried %d messages (pre-switch history was %d) — forced compaction must have trimmed it before the provider call",
			len(finalReq.Messages), preSwitchHistoryLen)
	}
}

// TestForceCompactionCheckClearsAndStaysOffAfterFailedAttempt is the
// red-first regression test for the retry half of round-3 review BLOCKING
// A: a forced pass whose own Compact call fails for a real, unclassified
// reason (see TestForcedCompactionErrorProceedsToNativeProviderAfterClaudeCodeSwitch
// for that first attempt's own assertions) clears forceCompactionCheck via
// failForcedCompactionLoudly — it does NOT stay armed. This used to pin the
// OPPOSITE requirement (BLOCKING 2 of the original fix round): that the
// flag survive the failed attempt so a retry re-checks and re-forces
// compaction. Round 3 removed the growth-triggered re-arm entirely (see
// docs/design/context-compaction.md) precisely because that mechanism
// could not distinguish "the journal grew because a retry is due" from
// "the journal grew because the caller sent another prompt" — so the
// correct behavior for a retry after a terminated forced pass is now the
// ORDINARY (non-forced) path, which does not reissue the summarizer at
// all.
func TestForceCompactionCheckClearsAndStaysOffAfterFailedAttempt(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	// No scripted turns: the forced compaction's own summarization call is
	// the very first Stream call and exhausts prov.turns immediately, so
	// Compact fails for a real, unclassified reason.
	prov := &scriptedProvider{name: "test"}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}

	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err == nil {
		t.Fatal("first Prompt after the switch succeeded, want the native provider's own out-of-scripted-turns error (test setup)")
	}
	if got := s.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount after the failed first attempt = %d, want 0", got)
	}
	s.mu.Lock()
	armedAfterFirst := s.forceCompactionCheck
	s.mu.Unlock()
	if armedAfterFirst {
		t.Fatal("forceCompactionCheck still true after the terminating first attempt, want it cleared")
	}

	// The retry: pre-round-3, a still-armed flag would take the forced
	// branch again and re-invoke the summarizer even though it now has a
	// turn scripted. Post-fix, the flag is already cleared, so this call
	// takes the ORDINARY branch, reads the small stale delegated lastUsage
	// (well under threshold), and skips compaction — only the native
	// reply's own call reaches the provider.
	requestsBeforeRetry := len(prov.requests)
	prov.turns = [][]provider.Event{
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}
	prov.call = 0
	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("retry Prompt: %v", err)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount after the retry = %d, want 0 (no re-arm without a new SetModel)", got)
	}
	if got := len(prov.requests) - requestsBeforeRetry; got != 1 {
		t.Errorf("provider calls made during the retry = %d, want 1 (the native turn only, no additional summarizer call)", got)
	}
}

// TestMaybeAutoCompactForcedEmptySummaryTerminatesAndProceeds is the
// red-first regression test for NEW-BLOCKING 9's terminating requirement,
// the empty-summary shape: a forced pass's own summarization call that
// runs, completes, and returns nothing usable (SkipReasonSummarizerEmpty,
// TurnsFolded == 0) is a REAL, billed provider call that made no progress
// toward the one thing a forced pass exists to achieve. Pre-fix, this left
// forceCompactionCheck armed and failed Prompt loudly FOREVER — no retry of
// the identical journal shape could ever change the summarizer's answer, so
// every future Prompt call on this session failed without ever reaching a
// provider that might have accepted the request. This pins the fix instead:
// EventCompactionFailed still reports it (never silent), but Prompt
// SUCCEEDS — the request proceeds to the native provider for its own real
// verdict — and forceCompactionCheck is cleared, with no retry left armed
// (see TestMaybeAutoCompactStaysOffAfterExhaustionUntilModelSwitch for that
// half).
func TestMaybeAutoCompactForcedEmptySummaryTerminatesAndProceeds(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("", provider.Usage{InputTokens: 5}), // the model returns nothing usable
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt after a forced compaction with an empty summary: %v, want it to proceed to the provider instead of blocking forever", err)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (the summarizer never produced a usable fold)", got)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (the empty-summary compaction attempt, then the native turn proceeding uncompacted)", len(prov.requests))
	}
	var failedReason string
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed && strings.Contains(ev.Text, "made no progress") {
			failedReason = ev.Text
		}
	}
	if failedReason == "" {
		t.Errorf("no EventCompactionFailed naming \"made no progress\" among %d events, want the loud report preserved even though Prompt proceeds", len(evs))
	}
	s.mu.Lock()
	armed := s.forceCompactionCheck
	s.mu.Unlock()
	if armed {
		t.Error("forceCompactionCheck still true after the terminating empty-summary pass, want it cleared")
	}
}

// TestMaybeAutoCompactForcedStillOverAfterFoldTerminatesAndProceeds is the
// red-first regression test for NEW-BLOCKING 9's terminating requirement,
// the still-over-after-fold shape: a forced pass never re-checked its own
// work after a real fold, so a kept-turns tail holding one giant message
// left the journal over the window regardless. Pre-fix this failed Prompt
// loudly FOREVER on every subsequent call (folding the same kept turn again
// can never shrink it). keep_turns=1 keeps only the final (huge) delegated
// turn; folding away everything before it cannot relieve that turn's own
// size, so the post-fold re-estimate is still over the window —
// EventCompactionFailed reports it, but Prompt must SUCCEED, reaching the
// native provider for its own verdict instead of bricking the session.
func TestMaybeAutoCompactForcedStillOverAfterFoldTerminatesAndProceeds(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("gist", provider.Usage{InputTokens: 5}),
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	seedDelegatedTurn(s, strings.Repeat("x", 100))
	seedDelegatedTurn(s, strings.Repeat("x", 100))
	seedDelegatedTurn(s, strings.Repeat("x", 6000)) // kept verbatim (keep_turns=1); alone well over threshold*window (800 tokens)

	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt after a forced compaction that folded but left the journal still over the window: %v, want it to proceed to the provider instead of blocking forever", err)
	}
	if got := s.CompactionCount(); got != 1 {
		t.Errorf("CompactionCount = %d, want 1 (the fold itself succeeded)", got)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (the summarization call, then the native turn proceeding uncompacted)", len(prov.requests))
	}
	var failedReason string
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed && strings.Contains(ev.Text, "still over the window") {
			failedReason = ev.Text
		}
	}
	if failedReason == "" {
		t.Errorf("no EventCompactionFailed naming \"still over the window\" among %d events, want the loud report preserved even though Prompt proceeds", len(evs))
	}
	s.mu.Lock()
	armed := s.forceCompactionCheck
	s.mu.Unlock()
	if armed {
		t.Error("forceCompactionCheck still true after the terminating still-over-after-fold pass, want it cleared")
	}
}

// contextOverflowOnceProvider fails exactly its first Stream call with a
// classified provider.ErrKindContextOverflow error — the shape Session.
// Compact's own doc comment names explicitly ("a range too large to
// summarize in one call"), and the most likely error a forced pass's own
// summarization call hits when the fold range is the whole journal minus
// the kept turns — then serves scriptedProvider's scripted turns for every
// call after that.
type contextOverflowOnceProvider struct {
	scriptedProvider
	failedOnce bool
}

func (p *contextOverflowOnceProvider) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	if !p.failedOnce {
		p.failedOnce = true
		p.requests = append(p.requests, req)
		return nil, &provider.Error{Kind: provider.ErrKindContextOverflow, PromptTokens: 400000, TokenLimit: 200000}
	}
	return p.scriptedProvider.Stream(ctx, req)
}

// TestMaybeAutoCompactForcedCompactErrorTerminatesAndProceeds is the
// red-first regression test for round-3 review BLOCKING A: pre-fix, ANY
// error from a forced pass's own Compact call — not a skip, a real failure
// — left forceCompactionCheck armed and failed the Prompt call outright
// ("engine: forced compaction failed: %w"), forever: the next Prompt call
// reissued the identical oversized fold range against the identical
// summarizer, which is deterministic for a classified context-overflow
// error (retrying changes nothing about the request shape — the same
// precedent engine/goal.go:1173 already applies to a live native turn). A
// session whose delegated journal outgrew the summarizer model's OWN
// window at the moment of a claude-code-to-native switch was therefore
// permanently un-promptable on any native model, with no in-band escape —
// exactly the incident class NEW-9 exists to close, reached through the
// one branch that fix did not touch. This pins the fix instead:
// EventCompactionFailed still reports the error (never silent), but Prompt
// SUCCEEDS on the very call that hit it — the request proceeds to the
// native provider for its own real verdict — forceCompactionCheck is
// cleared, and (see the growth re-arm's removal, docs/design/
// context-compaction.md) a later Prompt call does not re-invoke the
// summarizer at all: the mechanism stays off until a native turn lands
// usage or SetModel switches again.
func TestMaybeAutoCompactForcedCompactErrorTerminatesAndProceeds(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &contextOverflowOnceProvider{scriptedProvider: scriptedProvider{name: "test", turns: [][]provider.Event{
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
		compactTurn("native reply 2", provider.Usage{InputTokens: 50}),
	}}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	if _, err := s.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt after a forced compaction whose Compact call errored: %v, want it to proceed to the provider instead of blocking forever", err)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount = %d, want 0 (the summarizer call itself errored; nothing folded)", got)
	}
	if len(prov.requests) != 2 {
		t.Errorf("provider calls = %d, want 2 (the errored compaction attempt, then the native turn proceeding uncompacted)", len(prov.requests))
	}
	var failedReason string
	for _, ev := range evs {
		if ev.Type == EventCompactionFailed && strings.Contains(ev.Text, "forced compaction failed") {
			failedReason = ev.Text
		}
	}
	if failedReason == "" {
		t.Errorf("no EventCompactionFailed naming \"forced compaction failed\" among %d events, want the loud report preserved even though Prompt proceeds", len(evs))
	}
	s.mu.Lock()
	armed := s.forceCompactionCheck
	s.mu.Unlock()
	if armed {
		t.Error("forceCompactionCheck still true after the terminating Compact-error pass, want it cleared")
	}

	// A second Prompt call, with no further model switch and no re-arm
	// mechanism left (the growth retry is gone), must NOT re-invoke the
	// summarizer: the first Prompt's own native reply already landed real
	// usage well under threshold, so the ordinary trigger correctly skips.
	if _, err := s.Prompt(context.Background(), "continue again"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount after the second Prompt = %d, want 0 (no re-arm without a new SetModel)", got)
	}
	if got := len(prov.requests); got != 3 {
		t.Errorf("provider calls after the second Prompt = %d, want 3 (one more native turn, no additional summarizer call)", got)
	}
}

// TestMaybeAutoCompactStaysOffAfterExhaustionUntilModelSwitch is the
// red-first regression test replacing NEW-BLOCKING 9's re-arm requirement:
// an earlier design's `forceCompactionExhaustedAt` gave a session another
// forced attempt once the journal had genuinely grown past the point a
// prior pass gave up at — the round-3 review measured that this could not
// actually tell "the journal grew because a retry is due" from "the journal
// grew because the caller sent another Prompt": maybeAutoCompact runs
// before the incoming user message is appended, so a terminated forced pass
// that lets the turn through grows the journal by construction on every
// single later Prompt call, which reissued the summarizer once per Prompt
// indefinitely while pressure persisted — the exact per-turn billed-call
// shape the design doc's own "never ... regardless of whether anything
// changed" claim said the mechanism prevented. This pins its removal
// instead: once a forced pass has terminated (see
// failForcedCompactionLoudly), the mechanism stays OFF — no summarizer call
// on a later Prompt call — until either a native turn lands real usage or
// the model is switched again via SetModel. The scripted provider has
// exactly ONE turn for the first Prompt call: the compaction summary itself
// (empty, so Compact reports SkipReasonSummarizerEmpty and maybeAutoCompact
// terminates and proceeds), then the native turn's OWN Stream call runs out
// of scripted turns and fails — so, unlike
// TestMaybeAutoCompactForcedEmptySummaryTerminatesAndProceeds, no usage
// lands, leaving forceCompactionCheck's clear (not a re-arm) as the only
// thing standing between the second Prompt call and another summarizer
// call.
func TestMaybeAutoCompactStaysOffAfterExhaustionUntilModelSwitch(t *testing.T) {
	nativeModel := message.ModelRef{Provider: "test", Model: "m1"}
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		compactSummaryTurn("", provider.Usage{InputTokens: 5}), // first attempt: no progress; the native turn after it has no scripted reply and fails
	}}
	s := NewSession(Config{
		Providers:           provider.Registry{"test": prov},
		Model:               message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
		ContextWindowTokens: 1000,
		CompactionKeepTurns: 1,
	})

	long := strings.Repeat("x", 800)
	for i := 0; i < 5; i++ {
		seedDelegatedTurn(s, long)
	}
	preFirstPromptHistoryLen := len(s.History())
	s.mu.Lock()
	s.lastUsage = provider.Usage{InputTokens: 50}
	s.haveLastUsage = true
	s.mu.Unlock()

	s.SetModel(nativeModel)

	// First Prompt call: the empty summary makes no progress and
	// terminates (see TestMaybeAutoCompactForcedEmptySummaryTerminatesAndProceeds
	// for that step's full assertion set), so maybeAutoCompact lets the
	// turn proceed — but the native provider itself has nothing scripted
	// and fails, so Prompt returns that error and no usage ever lands.
	if _, err := s.Prompt(context.Background(), "continue"); err == nil {
		t.Fatal("first Prompt succeeded, want the native provider's own out-of-scripted-turns error (test setup)")
	}
	if got := s.CompactionCount(); got != 0 {
		t.Fatalf("CompactionCount after the first (no-progress) attempt = %d, want 0", got)
	}
	s.mu.Lock()
	armedAfterFirst := s.forceCompactionCheck
	s.mu.Unlock()
	if armedAfterFirst {
		t.Fatal("forceCompactionCheck still true after the terminating first attempt (test setup)")
	}
	if got := len(s.History()); got <= preFirstPromptHistoryLen {
		t.Fatalf("history length after the first Prompt call = %d, want more than %d (the user message must persist even though the native call failed, test setup)", got, preFirstPromptHistoryLen)
	}

	// Second Prompt call: no SetModel, no new switch — the journal grew
	// only because the first call's own user message was appended before
	// its native provider call failed. There is no growth-triggered retry
	// left to notice that growth, so this call takes the ordinary
	// (non-forced) path, which reads the small stale lastUsage the
	// claude-code switch left behind and correctly finds nothing over
	// threshold — no additional summarizer call.
	prov.turns = [][]provider.Event{
		compactTurn("native reply", provider.Usage{InputTokens: 50}),
	}
	prov.call = 0
	requestsBeforeSecond := len(prov.requests)
	if _, err := s.Prompt(context.Background(), "continue again"); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if got := s.CompactionCount(); got != 0 {
		t.Errorf("CompactionCount after the second Prompt = %d, want 0 (no re-arm without a new SetModel)", got)
	}
	if got := len(prov.requests) - requestsBeforeSecond; got != 1 {
		t.Errorf("provider calls made during the second Prompt = %d, want 1 (the native turn only, no additional summarizer call)", got)
	}
}

// TestCompactRefusesCurrentlyDelegatedSession is the red-first regression
// test for the second half of SHOULD 5: server/handlers.go's
// rejectClaudeCodeDelegatedCompact and its post-claim re-check are both
// server-side conveniences — SetModel takes no run slot, so a native-to-
// claude-code switch can land in the window between either check and the
// actual Session.Compact call, and any OTHER future caller of Compact has
// neither check at all. Session.Compact itself must refuse outright for a
// session CURRENTLY delegated to the Claude Code CLI, independent of any
// caller's own guard.
func TestCompactRefusesCurrentlyDelegatedSession(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	s := NewSession(Config{
		Providers: provider.Registry{"test": prov},
		Model:     message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
	})
	seedDelegatedTurn(s, "hello")
	seedDelegatedTurn(s, "hello again")

	_, err := s.Compact(context.Background(), CompactOptions{})
	if err == nil {
		t.Fatal("Compact on a currently-delegated session succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "Claude Code CLI") {
		t.Errorf("Compact error = %q, want it to name the Claude Code CLI as the reason", err.Error())
	}
	if len(prov.requests) != 0 {
		t.Errorf("provider calls = %d, want 0 (Compact must refuse before ever calling the provider)", len(prov.requests))
	}
}

// TestSetModelClearsForceCompactionCheckOnSwitchBackToDelegated is the
// red-first regression test for NIT 1: forceCompactionCheck used to survive
// a switch BACK to claude-code delegation — SetModel's switch table cleared
// it on no branch. Harmless only because PromptWithOrigin's delegated
// dispatch returns before maybeAutoCompact ever runs for a delegated
// session, an invariant nothing pinned with a test before this one. Checks
// the field directly (white-box, same package).
func TestSetModelClearsForceCompactionCheckOnSwitchBackToDelegated(t *testing.T) {
	s := NewSession(Config{
		Providers: provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:     message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"},
	})

	s.SetModel(message.ModelRef{Provider: "test", Model: "m1"})
	s.mu.Lock()
	armed := s.forceCompactionCheck
	s.mu.Unlock()
	if !armed {
		t.Fatal("forceCompactionCheck not armed after a claude-code-to-native switch (test setup)")
	}

	s.SetModel(message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "opus"})
	s.mu.Lock()
	stillArmed := s.forceCompactionCheck
	s.mu.Unlock()
	if stillArmed {
		t.Error("forceCompactionCheck still true after switching BACK to claude-code delegation, want it cleared — nothing to force-check while delegated")
	}
}
