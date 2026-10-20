package eventlog

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/majorcontext/harness/protocol"
)

const (
	derivedTitleWords = 8
	derivedTitleBytes = 100
)

// derivedTitle is the title that an input gives: the first line of its first
// text part, cut to a few words. A slash command and an input with no text give none.
func derivedTitle(parts []protocol.LogPart) string {
	for _, p := range parts {
		if p.Type != protocol.LogPartText {
			continue
		}
		text := strings.TrimSpace(p.Text)
		if strings.HasPrefix(text, "/") {
			return ""
		}
		line, _, _ := strings.Cut(text, "\n")
		words := strings.Fields(line)
		title := strings.Join(words[:min(len(words), derivedTitleWords)], " ")
		if len(title) > derivedTitleBytes {
			cut := derivedTitleBytes
			for !utf8.RuneStart(title[cut]) {
				cut--
			}
			title = title[:cut]
		}
		return title
	}
	return ""
}

func (s *State) applyTitled(e protocol.SessionTitled) error {
	if e.Title == "" || strings.ContainsFunc(e.Title, unicode.IsControl) {
		return illegal("session.titled has an empty title or a title with a control character")
	}
	s.renamed = e.Title
	return nil
}
