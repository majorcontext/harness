package prompt

import (
	"strings"
	"testing"
)

// Input: a session with no project instructions. Wrong output: the base
// prompt asks only for a "short" final message, so user-visible answer length
// falls back to model default.
func TestBaseBehaviorGuidanceSetsAnAdaptiveBrevityDefault(t *testing.T) {
	got := baseBehaviorGuidance()

	for _, want := range []string{
		"concise, direct, and friendly",
		"10 lines",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("base behavior guidance missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "unless") {
		t.Errorf("brevity ceiling has no stated exception, so it reads as an absolute cap:\n%s", got)
	}
	for _, unwanted := range []string{"4 lines", "four lines", "one-word", "single word"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("base behavior guidance carries a second, tighter brevity cap %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "Cite paths") {
		t.Errorf("base behavior guidance lost the cite-a-path-instead-of-pasting rule:\n%s", got)
	}
}

// Input: a session with no project instructions. Wrong output: the base
// prompt constrains user-visible prose but says nothing about comments or
// docs, so a model narrates change history beside the code it writes.
func TestBaseBehaviorGuidanceGovernsCommentsAndDocs(t *testing.T) {
	got := baseBehaviorGuidance()

	if !strings.Contains(got, "Do not add comments unless") {
		t.Errorf("base behavior guidance does not prohibit comments by default:\n%s", got)
	}
	for _, want := range []string{"explicitly requested", "project instructions"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment prohibition missing its %q exception, so a repo cannot opt in:\n%s", want, got)
		}
	}
	for _, want := range []string{"docs only when needed", "concise and current"} {
		if !strings.Contains(got, want) {
			t.Errorf("base behavior guidance missing docs rule %q:\n%s", want, got)
		}
	}
	for _, want := range []string{"change history", "incidents", "reviews", "commits", "issues"} {
		if !strings.Contains(got, want) {
			t.Errorf("base behavior guidance does not route %q off the code surface:\n%s", want, got)
		}
	}

	for _, want := range []string{"concise, direct, and friendly", "10 lines", "unless"} {
		if !strings.Contains(got, want) {
			t.Errorf("base behavior guidance lost the adaptive response default %q:\n%s", want, got)
		}
	}
}

// TestBaseBehaviorGuidanceStaysUnderBudget pins the line/word ceiling set
// for the addition ("the prose is minimal and not too crazy long") so a later
// clause-by-clause addition cannot silently balloon it back into a
// Codex-sized style guide.
func TestBaseBehaviorGuidanceStaysUnderBudget(t *testing.T) {
	block := baseBehaviorGuidance()
	if lines := strings.Count(block, "\n") + 1; lines > baseBehaviorGuidanceMaxLines {
		t.Errorf("baseBehaviorGuidance = %d lines, want at most %d", lines, baseBehaviorGuidanceMaxLines)
	}
	if words := len(strings.Fields(block)); words > baseBehaviorGuidanceMaxWords {
		t.Errorf("baseBehaviorGuidance = %d words, want at most %d", words, baseBehaviorGuidanceMaxWords)
	}
}
