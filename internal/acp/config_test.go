package acp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestShortDescriptionCutsAtAWordBoundaryInRunes(t *testing.T) {
	long := strings.Repeat("é", 60) + " " + strings.Repeat("é", 60)
	got := shortDescription(long)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) > descriptionLimit+1 {
		t.Errorf("shortDescription = %q, want valid text of at most %d characters", got, descriptionLimit+1)
	}
	if got := shortDescription("First one. Second one."); got != "First one" {
		t.Errorf("shortDescription = %q, want the first sentence", got)
	}
	if got := shortDescription(strings.Repeat("é", 150)); !utf8.ValidString(got) {
		t.Errorf("shortDescription of a text with no space = %q, want valid text", got)
	}
}

func TestProviderLabelCapitalizesEachWord(t *testing.T) {
	for in, want := range map[string]string{"claude-code": "Claude Code", "éclair": "Éclair", "gateway": "Gateway"} {
		if got := providerLabel(in); got != want {
			t.Errorf("providerLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
