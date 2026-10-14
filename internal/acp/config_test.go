package acp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOptionDescriptionLimitsTheWholeTextAtAWordBoundaryInRunes(t *testing.T) {
	long := strings.Repeat("é", 60) + " " + strings.Repeat("é", 60)
	got := optionDescription("openai/gpt-daybreak-red-latest", long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > descriptionLimit {
		t.Errorf("optionDescription = %q (%d characters), want valid text of at most %d", got, utf8.RuneCountInString(got), descriptionLimit)
	}
	if !strings.HasPrefix(got, "openai/gpt-daybreak-red-latest · ") {
		t.Errorf("optionDescription = %q, want the reference first", got)
	}
	if got := optionDescription("a/b", "First one. Second one."); got != "a/b · First one" {
		t.Errorf("optionDescription = %q, want the reference and the first sentence", got)
	}
	if got := optionDescription("a/b", ""); got != "a/b" {
		t.Errorf("optionDescription = %q, want the reference alone", got)
	}
	if got := optionDescription("a/b", strings.Repeat("é", 150)); !utf8.ValidString(got) || utf8.RuneCountInString(got) > descriptionLimit {
		t.Errorf("optionDescription of a text with no space = %q, want valid text of at most %d characters", got, descriptionLimit)
	}
}

func TestProviderLabelCapitalizesEachWord(t *testing.T) {
	for in, want := range map[string]string{"claude-code": "Claude Code", "éclair": "Éclair", "gateway": "Gateway"} {
		if got := providerLabel(in); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
