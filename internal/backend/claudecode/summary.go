package claudecode

import (
	"regexp"
	"strings"
)

var (
	analysisLines    = regexp.MustCompile(`(?im)^[ \t]*<analysis>[ \t]*\r?\n[\s\S]*?\n[ \t]*</analysis>[ \t]*\r?$`)
	analysisLeader   = regexp.MustCompile(`(?i)^\s*<analysis>[\s\S]*?</analysis>`)
	summaryOpen      = regexp.MustCompile(`(?im)^[ \t]*<summary>[ \t]*\r?\n`)
	summaryClose     = regexp.MustCompile(`(?im)^[ \t]*</summary>[ \t]*\r?$`)
	summaryOpenTag   = regexp.MustCompile(`(?i)<summary>`)
	continuationLead = regexp.MustCompile(`(?i)^\s*This session is being continued from a previous conversation`)
	persistedLead    = regexp.MustCompile(`^\s*This session is being continued from a previous conversation[^\n]*\n+Summary:[ \t]*\r?\n`)
	persistedTail    = []*regexp.Regexp{
		regexp.MustCompile(`\n+If you need specific details from before compaction[\s\S]*$`),
		regexp.MustCompile(`\n+(?:Please )?[Cc]ontinue the conversation from where it left (?:it )?off[\s\S]*$`),
	}
)

// plainSummary returns the text of a summary that the CLI wrote, without the
// reasoning block, the summary tags, or the continuation framing that the CLI
// adds for the model. Text that it does not know stays as it is, because the
// model reads the summary after a switch to another backend.
func plainSummary(raw string) string {
	if continuationLead.MatchString(raw) {
		return persistedSummary(raw)
	}
	text := analysisLines.ReplaceAllString(raw, "")
	text = analysisLeader.ReplaceAllString(text, "")
	if tagged, ok := taggedSummary(text); ok {
		text = tagged
	}
	return strings.TrimSpace(text)
}

// persistedSummary returns the body of a continuation prompt without the
// closing that the adapter knows. A prompt with an opening that it does not
// know is returned whole.
func persistedSummary(raw string) string {
	loc := persistedLead.FindStringIndex(raw)
	if loc == nil {
		return strings.TrimSpace(raw)
	}
	text := raw[loc[1]:]
	for _, tail := range persistedTail {
		if loc := tail.FindStringIndex(text); loc != nil {
			text = text[:loc[0]]
		}
	}
	return strings.TrimSpace(text)
}

// taggedSummary returns the text between the summary tags. Tags on their own
// lines win over inline tags, so a summary that quotes a tag is not cut at it.
func taggedSummary(raw string) (string, bool) {
	if closes := summaryClose.FindAllStringIndex(raw, -1); len(closes) > 0 {
		last := closes[len(closes)-1]
		if opens := summaryOpen.FindAllStringIndex(raw[:last[0]], -1); len(opens) > 0 {
			return raw[opens[len(opens)-1][1]:last[0]], true
		}
	}
	open := summaryOpenTag.FindStringIndex(raw)
	end := strings.LastIndex(strings.ToLower(raw), "</summary>")
	if open != nil && end > open[0] && end >= open[1] {
		return raw[open[1]:end], true
	}
	return "", false
}
