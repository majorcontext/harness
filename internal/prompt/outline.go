package prompt

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

const (
	outlineHeader = "[instructions outline]"
	// outlineMaxBytes bounds the outline block. Over it, the block drops the
	// teaser of each section and lists headings and ranges only.
	outlineMaxBytes    = 8 << 10
	outlineTeaserBytes = 120
)

// section is a Markdown section: its heading, its 1-based inclusive line
// range, and its byte range as a slice (start inclusive, end exclusive).
type section struct {
	title              string
	startLine, endLine int
	startByte, endByte int
}

// scanSections splits data at ATX headings of levels 1 to 4. Text before the
// first heading is a "(preamble)" section. A line inside a fenced code block
// is never a heading.
func scanSections(data []byte) []section {
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	var (
		out    []section
		fence  fenceState
		offset int
	)
	for i, line := range lines {
		lineNo := i + 1
		if !fence.step(line) && !fence.open && headingTitle(line) != "" {
			if len(out) > 0 {
				out[len(out)-1].endLine = lineNo - 1
				out[len(out)-1].endByte = offset
			} else if offset > 0 {
				out = append(out, section{title: "(preamble)", startLine: 1, endLine: lineNo - 1, endByte: offset})
			}
			out = append(out, section{title: headingTitle(line), startLine: lineNo, startByte: offset})
		}
		offset += len(line) + 1
	}
	if len(out) > 0 {
		out[len(out)-1].endLine = len(lines)
		out[len(out)-1].endByte = len(data)
	}
	return out
}

// fenceState follows one fenced code block across lines by the CommonMark
// rule: a fence closes on the character that opened it, with at least as many
// of them and no info string.
type fenceState struct {
	open  bool
	char  byte
	count int
}

// step folds line into the state and reports whether line is a fence
// delimiter.
func (f *fenceState) step(line string) bool {
	char, count, closing, ok := fenceLine(line)
	if !ok {
		return false
	}
	if !f.open {
		f.open, f.char, f.count = true, char, count
		return true
	}
	if char == f.char && count >= f.count && closing {
		*f = fenceState{}
		return true
	}
	return false
}

// fenceLine parses a fence delimiter: its character, its run length, and
// whether it can close a fence. An indent over three spaces is a code block.
func fenceLine(line string) (char byte, count int, closing, ok bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > 3 || indent >= len(line) {
		return 0, 0, false, false
	}
	rest := line[indent:]
	c := rest[0]
	if c != '`' && c != '~' {
		return 0, 0, false, false
	}
	n := 0
	for n < len(rest) && rest[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false, false
	}
	return c, n, strings.TrimSpace(rest[n:]) == "", true
}

// headingTitle returns the text of an ATX heading of level 1 to 4, or "" when
// line is not one. A heading with no text is body text.
func headingTitle(line string) string {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 4 || level >= len(line) || line[level] != ' ' {
		return ""
	}
	return strings.TrimSpace(line[level+1:])
}

// outlined renders a file over limit as the sections that fit whole plus an
// outline of the rest, each line naming the read_file range that reads it. A
// first section over limit is cut and gets the truncation marker. A file with
// fewer than two sections has nothing to outline and gets the marker.
func outlined(path string, data []byte, limit int) string {
	secs := scanSections(data)
	if len(secs) < 2 {
		return truncated(path, data, limit, len(data))
	}
	keep := 0
	for keep < len(secs) && secs[keep].endByte <= limit {
		keep++
	}
	var head string
	if keep == 0 {
		head = truncated(path, data[:secs[1].startByte], limit, len(data))
		keep = 1
	} else {
		head = string(data[:secs[keep-1].endByte])
	}
	rest := secs[keep:]
	slog.Warn("prompt: instructions outlined", "path", path, "bytes", len(data), "head", len(head),
		"sections", len(secs), "outlined", len(rest), "limit", limit)
	block := outlineBlock(path, data, len(secs), rest, true)
	if len(block) > outlineMaxBytes {
		block = outlineBlock(path, data, len(secs), rest, false)
	}
	return strings.TrimRight(head, "\n") + "\n\n" + block
}

func outlineBlock(path string, data []byte, total int, rest []section, teasers bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d of the %d sections of %s are not in this prompt. You MUST read a section with the read_file tool before you rely on it:\n",
		outlineHeader, len(rest), total, path)
	for _, s := range rest {
		fmt.Fprintf(&b, "  %s — read_file(path=%s, offset=%d, limit=%d)", s.title, path, s.startLine, s.endLine-s.startLine+1)
		if teasers {
			if t := teaser(data, s); t != "" {
				fmt.Fprintf(&b, " — %s", t)
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// teaser returns the first paragraph of the body of s on one line, cut at
// outlineTeaserBytes on a rune boundary.
func teaser(data []byte, s section) string {
	_, body, ok := strings.Cut(string(data[s.startByte:s.endByte]), "\n")
	if !ok {
		return ""
	}
	var words []string
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if _, _, _, fence := fenceLine(line); line == "" || fence {
			if len(words) > 0 {
				break
			}
			continue
		}
		words = append(words, line)
		if len(strings.Join(words, " ")) >= outlineTeaserBytes {
			break
		}
	}
	t := strings.Join(words, " ")
	if len(t) > outlineTeaserBytes {
		cut := outlineTeaserBytes
		for cut > 0 && !utf8.ValidString(t[:cut]) {
			cut--
		}
		t = strings.TrimSpace(t[:cut]) + "…"
	}
	return t
}
