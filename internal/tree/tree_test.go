package tree

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/eventlog"
)

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
