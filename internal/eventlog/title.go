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
		return cutTitle(text)
	}
	return ""
}

// cutTitle cuts the first line of text to the words and bytes of a derived title.
func cutTitle(text string) string {
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

// TitleInput returns the first text part of parts when the input gives a derived title, else "".
func TitleInput(parts []protocol.LogPart) string {
	if derivedTitle(parts) == "" {
		return ""
	}
	for _, p := range parts {
		if p.Type == protocol.LogPartText {
			return p.Text
		}
	}
	return ""
}

// ModelTitle cleans the answer of a title model: its first non-empty line without
// surrounding quotes and a trailing period, cut as a derived title is. It is empty for no title.
func ModelTitle(answer string) string {
	for line := range strings.SplitSeq(answer, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			line = strings.TrimSpace(strings.Trim(line, `"'`))
			return cutTitle(strings.TrimSpace(strings.TrimSuffix(line, ".")))
		}
	}
	return ""
}

func (s *State) applyTitled(e protocol.SessionTitled) error {
	if e.Title == "" || strings.ContainsFunc(e.Title, unicode.IsControl) {
		return illegal("session.titled has an empty title or a title with a control character")
	}
	switch e.Source {
	case protocol.TitleUser:
		s.renamed = e.Title
	case protocol.TitleModel:
		if s.renamed == "" {
			s.modeled = e.Title
		}
	default:
		return illegal("session.titled has source %q", e.Source)
	}
	return nil
}
