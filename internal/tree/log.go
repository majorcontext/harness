package tree

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/session"
)

// The bounds of the log action. Its reply stays in the context of the
// parent for every later turn.
const (
	logTail     = 20
	logMaxTail  = 100
	logEntryCap = 2000
	logTotalCap = 20000
	logArgsCap  = 300
	cutMark     = session.CutMark
)

// logEntry is one message of a child transcript, flattened to text.
// Truncated marks an entry that lost content to any cap.
type logEntry struct {
	Role      string `json:"role"`
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// renderLog renders h under logTotalCap. It fills the newest entries
// first, so the messages nearest the end of a child survive, and always
// keeps the newest one.
func renderLog(h []protocol.LogMessage) []logEntry {
	out := []logEntry{}
	budget := logTotalCap
	for _, m := range slices.Backward(h) {
		e := renderEntry(m)
		n := utf8.RuneCountInString(e.Text)
		if n > budget && len(out) > 0 {
			break
		}
		budget -= n
		out = append(out, e)
	}
	slices.Reverse(out)
	return out
}

func renderEntry(m protocol.LogMessage) logEntry {
	cut := false
	capped := func(s string, n int) string {
		s, c := session.CapRunes(s, n)
		cut = cut || c
		return s
	}
	var lines []string
	for _, p := range m.Parts {
		switch {
		case (p.Type == protocol.LogPartText || p.Type == protocol.LogPartEngineContext) && p.Text != "":
			lines = append(lines, capped(p.Text, logEntryCap))
		case p.Type == protocol.LogPartReasoning && p.Text != "":
			lines = append(lines, "[reasoning] "+capped(p.Text, logEntryCap))
		case p.Type == protocol.LogPartWebSearch:
			lines = append(lines, "[web_search] "+capped(p.Query, logArgsCap))
		case p.Type == protocol.LogPartToolCall:
			lines = append(lines, "[tool_call] "+p.Name+"("+capped(string(p.Arguments), logArgsCap)+")")
		case p.Type == protocol.LogPartToolResult && p.IsError:
			lines = append(lines, "[tool_result error] "+capped(p.Text, logEntryCap))
		case p.Type == protocol.LogPartToolResult:
			lines = append(lines, "[tool_result] "+capped(p.Text, logEntryCap))
		}
	}
	return logEntry{Role: m.Role, Text: capped(strings.Join(lines, "\n"), logEntryCap), Truncated: cut}
}
