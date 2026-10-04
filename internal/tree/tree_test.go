package tree

import (
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
)

func TestNarrowKeepsTheNamesThatBothAllow(t *testing.T) {
	for _, tc := range []struct{ name, profile, parent, want []string }{
		{name: []string{"a profile that names none allows the parent's names"}, parent: []string{"ls"}, want: []string{"ls"}},
		{name: []string{"a parent that names none allows the profile's names"}, profile: []string{"ls"}, want: []string{"ls"}},
		{name: []string{"both name some"}, profile: []string{"ls", "grep"}, parent: []string{"grep", "bash"}, want: []string{"grep"}},
		{name: []string{"no name is in both"}, profile: []string{"ls"}, parent: []string{"bash"}, want: []string{}},
	} {
		t.Run(tc.name[0], func(t *testing.T) {
			if got := narrow(tc.profile, tc.parent); !slices.Equal(got, tc.want) {
				t.Errorf("narrow(%v, %v) = %v, want %v", tc.profile, tc.parent, got, tc.want)
			}
		})
	}
}

func TestRenderLogKeepsTheNewestEntriesUnderTheBudget(t *testing.T) {
	msg := func(text string) eventlog.Message {
		return eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}}
	}
	long := strings.Repeat("x", logEntryCap)
	var h []eventlog.Message
	for range 12 {
		h = append(h, msg(long))
	}
	got := renderLog(h)
	if len(got) != logTotalCap/logEntryCap || got[len(got)-1].Text != long {
		t.Errorf("renderLog kept %d entries, want %d ending with the newest", len(got), logTotalCap/logEntryCap)
	}
	if e := renderEntry(msg(long + "y")); !e.Truncated || !strings.HasSuffix(e.Text, cutMark) {
		t.Errorf("an entry over the cap = %+v, want it cut and marked", e)
	}
}
