// Progressive disclosure for an oversize project-instruction file.
//
// The loud-truncation cap tells the model that content was dropped, but the
// dropped content stays out of reach: the model can only guess what it lost.
// This file splits an oversize file into two parts instead — a HEAD the model
// always reads, and an OUTLINE of every section the head does not carry, each
// line naming the exact read_file range that reads it.
//
// The shape is the engine's Agent Skills stage-1/stage-2 split (see
// skillsSegment) applied to one file: an index the model MUST read through
// before it relies on a section. The retrieval tool is read_file itself,
// whose offset/limit are already 1-based line numbers (engine/filetools.go),
// so this adds no tool and no new schema to any request.
//
// Nothing is ever dropped in silence. Every section outside the head is
// listed, and a head that had to be cut mid-section still carries the loud
// truncation marker and its WARN log line.

package engine

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// InstructionsMode selects how an oversize instruction file is rendered.
type InstructionsMode string

const (
	// InstructionsModeAuto (the zero value) outlines an oversize file that
	// has usable section headings, and falls back to the loud truncation
	// marker for one that does not.
	InstructionsModeAuto InstructionsMode = ""
	// InstructionsModeFull keeps the head-plus-marker rendering for every
	// oversize file: no outline, whatever the headings look like.
	InstructionsModeFull InstructionsMode = "full"
)

// instructionsOutlineHeader opens the outline block. It is also the marker
// callers and tests split the segment on, so it must stay a single literal.
const instructionsOutlineHeader = "[instructions outline]"

// outlineMaxBytes bounds the outline block itself, so a file of thousands of
// tiny sections cannot spend the prompt on an index. The budget drops the
// per-section teasers first, then lists headings and ranges only.
const outlineMaxBytes = 8 * 1024

// outlineTeaserBytes caps one section's teaser text.
const outlineTeaserBytes = 120

// section is one Markdown section: its heading, the 1-based INCLUSIVE line
// range it spans, and the byte range it spans as a Go slice (start inclusive,
// end exclusive, so data[startByte:endByte] is the section).
//
// The two conventions differ because their consumers differ: read_file takes
// inclusive 1-based line numbers, and Go slicing is half-open.
type section struct {
	title     string
	startLine int // 1-based, inclusive
	endLine   int // 1-based, inclusive
	startByte int // inclusive
	endByte   int // exclusive
}

// scanSections splits Markdown into sections at ATX headings (levels 1-4).
// Text before the first heading, if any, is its own leading section so the
// line and byte accounting stays complete.
//
// The scan tracks fenced code blocks: a "# ..." line inside a ``` fence is
// body text, never a heading. This repository's own AGENTS.md holds bash
// blocks whose comments start with '#', and reading one as a section would
// advertise a range that points at a shell comment.
func scanSections(data []byte) []section {
	lines := strings.Split(string(data), "\n")
	// A trailing newline produces one empty final element; it is not a line.
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
		if fence.step(line) {
			// The line opened or closed a fence; it is never a heading.
		} else if !fence.open && headingTitle(line) != "" {
			if len(out) > 0 {
				out[len(out)-1].endLine = lineNo - 1
				out[len(out)-1].endByte = offset
			} else if offset > 0 {
				// Preamble before the first heading.
				out = append(out, section{title: "(preamble)", startLine: 1, endLine: lineNo - 1, startByte: 0, endByte: offset})
			}
			out = append(out, section{title: headingTitle(line), startLine: lineNo, startByte: offset})
		}
		offset += len(line) + 1 // the '\n' this line ends with
	}
	if len(out) > 0 {
		out[len(out)-1].endLine = len(lines)
		out[len(out)-1].endByte = len(data)
	}
	return out
}

// fenceState tracks one fenced code block across lines, following the
// CommonMark rules a single boolean cannot express: a fence closes only on the
// SAME character it opened with, and only with AT LEAST as many of them.
//
// Both rules are load-bearing for an instruction file that documents
// Markdown: such a file wraps a three-backtick example in a four-backtick
// fence, and a naive toggle closes the OUTER fence on the inner one, then
// reads the rest of the document as headings. This repository's own AGENTS.md
// does not currently hold such a fence — it holds bash blocks, which the
// simpler rule already handles — so this is hardening against a shape any
// documentation-heavy project produces, not a fix for an observed break.
type fenceState struct {
	open  bool
	char  byte
	count int
}

// step folds one line into the fence state and reports whether that line was a
// fence delimiter (which is therefore never a heading).
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
		f.open, f.char, f.count = false, 0, 0
		return true
	}
	// A shorter run, the other fence character, or an info string inside an
	// open fence is ordinary code content.
	return false
}

// fenceLine parses a fence delimiter: its character, its run length, whether
// it could CLOSE a fence (no info string after the run), and whether the line
// is a fence at all. Indentation over three spaces makes an indented code
// block, not a fence, so it is not one here either.
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

// headingTitle returns the text of an ATX heading (levels 1-4), or "" when
// line is not a heading. An empty heading ("## " with no text) reads as body
// text: CommonMark allows it, but a section with no name cannot be advertised
// in the outline, and real instruction files do not carry one.
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

// renderInstructions renders a validated instruction file for the system
// prompt: the whole file when it fits (or when the cap is disabled), a head
// plus an outline when it does not, and the loud head-plus-marker rendering
// when the file has no section to cut on or mode is InstructionsModeFull.
//
// path is the absolute path, because every advertised range is a read_file
// argument and an absolute path resolves the same from any working directory.
func renderInstructions(path string, data []byte, maxBytes int, mode InstructionsMode) string {
	if maxBytes < 0 || len(data) <= maxBytes {
		return string(data)
	}
	if mode == InstructionsModeFull {
		return truncateInstructions(path, data, maxBytes)
	}
	secs := scanSections(data)
	if len(secs) < 2 {
		// Nothing to pull on demand: one section (or none) means the outline
		// would list what the head already holds, or nothing at all.
		return truncateInstructions(path, data, maxBytes)
	}

	// The head holds every section that fits whole. keep is the number of
	// sections the head carries.
	keep := 0
	for keep < len(secs) && secs[keep].endByte <= maxBytes {
		keep++
	}

	var head string
	if keep == 0 {
		// The first section alone exceeds the cap, so there is no boundary to
		// cut on. Truncate that section and keep the cut loud: the marker and
		// the WARN line both fire, exactly as they do with no outline.
		head = truncateInstructionsOf(path, data[:secs[1].startByte], maxBytes, len(data))
		keep = 1
	} else {
		head = string(data[:secs[keep-1].endByte])
	}

	// outlined is never empty: the last section ends at len(data), which is
	// over the cap here, so the keep loop always stops before it.
	outlined := secs[keep:]
	slog.Warn("engine: instructions outlined",
		"path", path,
		"original_bytes", len(data),
		"head_bytes", len(head),
		"sections_total", len(secs),
		"sections_outlined", len(outlined),
		"limit_bytes", maxBytes,
	)
	return strings.TrimRight(head, "\n") + "\n\n" + formatOutline(path, data, secs, outlined)
}

// formatOutline renders the outline block: a notice naming how much of the
// file is absent, then one line per outlined section with its read_file
// range. Teasers are included while the block stays inside outlineMaxBytes,
// and dropped for the whole block when it does not.
func formatOutline(path string, data []byte, all, outlined []section) string {
	withTeasers := outlineBlock(path, data, all, outlined, true)
	if len(withTeasers) <= outlineMaxBytes {
		return withTeasers
	}
	return outlineBlock(path, data, all, outlined, false)
}

// outlineBlock builds the outline text, with or without teasers.
func outlineBlock(path string, data []byte, all, outlined []section, teasers bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d of the %d sections of %s are not in this prompt. You MUST read a section with the read_file tool before you rely on it:\n",
		instructionsOutlineHeader, len(outlined), len(all), path)
	for _, s := range outlined {
		fmt.Fprintf(&b, "  %s — read_file(path=%s, offset=%d, limit=%d)",
			s.title, path, s.startLine, s.endLine-s.startLine+1)
		if teasers {
			if t := sectionTeaser(data, s); t != "" {
				fmt.Fprintf(&b, " — %s", t)
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// sectionTeaser returns the first prose of a section's body, collapsed to one
// line and capped at outlineTeaserBytes, so the outline reads like the Agent
// Skills index (name — description).
func sectionTeaser(data []byte, s section) string {
	body := string(data[s.startByte:s.endByte])
	_, rest, ok := strings.Cut(body, "\n")
	if !ok {
		return ""
	}
	var words []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if _, _, _, isFence := fenceLine(line); line == "" || isFence {
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
	teaser := strings.Join(words, " ")
	if len(teaser) > outlineTeaserBytes {
		// Cut on a rune boundary: a heading or body in any non-ASCII script
		// would otherwise put a partial rune in the system prompt.
		cut := outlineTeaserBytes
		for cut > 0 && !utf8.ValidString(teaser[:cut]) {
			cut--
		}
		teaser = strings.TrimSpace(teaser[:cut]) + "…"
	}
	return teaser
}
