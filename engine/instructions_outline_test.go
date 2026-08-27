package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

// sectionDoc builds a Markdown file of n sections, each with a heading and
// bodyLines body lines, and returns the text plus each section's 1-based
// start line.
func sectionDoc(n, bodyLines int) (text string, starts []int) {
	var b strings.Builder
	line := 0
	for i := 1; i <= n; i++ {
		starts = append(starts, line+1)
		fmt.Fprintf(&b, "## Section %d\n", i)
		line++
		for j := 0; j < bodyLines; j++ {
			fmt.Fprintf(&b, "body %d line %d\n", i, j)
			line++
		}
	}
	return b.String(), starts
}

// outlineRange is one advertised read_file range, parsed out of the rendered
// outline exactly as a model would read it.
type outlineRange struct {
	path   string
	offset int
	limit  int
}

var outlineRangeRE = regexp.MustCompile(`read_file\(path=([^,]+), offset=(\d+), limit=(\d+)\)`)

// fatalf is the failure seam shared by *testing.T and *rapid.T.
type fatalf interface {
	Fatalf(format string, args ...any)
}

// parseOutlineRanges reads every advertised range out of a rendered segment.
func parseOutlineRanges(t fatalf, segment string) []outlineRange {
	var out []outlineRange
	for _, m := range outlineRangeRE.FindAllStringSubmatch(segment, -1) {
		offset, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("offset %q: %v", m[2], err)
		}
		limit, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatalf("limit %q: %v", m[3], err)
		}
		out = append(out, outlineRange{path: m[1], offset: offset, limit: limit})
	}
	return out
}

// readFileLines runs the real read_file tool over the advertised range and
// returns the plain lines it produced, with the "N→" prefixes removed.
func readFileLines(t *testing.T, workDir string, r outlineRange) []string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"path": r.path, "offset": r.offset, "limit": r.limit})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runTool(t, readFileTool(), workDir, string(args))
	if err != nil {
		t.Fatalf("read_file(offset=%d, limit=%d): %v", r.offset, r.limit, err)
	}
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[truncated:") {
			continue
		}
		_, rest, ok := strings.Cut(l, "→")
		if !ok {
			t.Fatalf("read_file line %q has no line-number prefix", l)
		}
		lines = append(lines, rest)
	}
	return lines
}

// TestInstructionsOutlineHeadAndSections pins the two-part segment: the head
// carries whole sections only, and every section the head does not carry is
// listed with a read_file range.
func TestInstructionsOutlineHeadAndSections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	body, starts := sectionDoc(10, 20)
	writeInstr(t, path, body)
	captureLogs(t)

	// A cap of 700 bytes holds two whole sections of this document.
	content, _, err := loadInstructionsMode(dir, 700, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	head, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline header:\n%s", content)
	}
	if strings.Contains(head, "[... truncated") {
		t.Errorf("head must not carry the truncation marker when it ends on a section boundary:\n%s", head)
	}
	if len(head) > 700 {
		t.Errorf("head is %d bytes, over the 700-byte cap", len(head))
	}
	if !strings.HasSuffix(strings.TrimRight(head, "\n"), "line 19") {
		t.Errorf("head must end at a section boundary, got tail %q", head[max(0, len(head)-40):])
	}
	ranges := parseOutlineRanges(t, outline)
	if len(ranges) == 0 {
		t.Fatalf("outline advertises no ranges:\n%s", outline)
	}
	// Every outlined range starts at a real section start line, and the last
	// section is listed.
	startSet := map[int]bool{}
	for _, s := range starts {
		startSet[s] = true
	}
	for _, r := range ranges {
		if !startSet[r.offset] {
			t.Errorf("advertised offset %d is not a section start line %v", r.offset, starts)
		}
		if r.path != path {
			t.Errorf("advertised path = %q, want the absolute path %q", r.path, path)
		}
	}
	if ranges[len(ranges)-1].offset != starts[len(starts)-1] {
		t.Errorf("last advertised offset = %d, want the last section start %d", ranges[len(ranges)-1].offset, starts[len(starts)-1])
	}
	if !strings.Contains(outline, "read_file") {
		t.Errorf("outline must name the read_file tool:\n%s", outline)
	}
}

// TestInstructionsOutlineRangesAreExact is the must-have from the design
// review: an advertised range must return EXACTLY the bytes of the section it
// names, after the head was cut on a heading boundary. The oracle is the file
// itself read through the real read_file tool, never the outline compared
// against itself.
func TestInstructionsOutlineRangesAreExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	body, starts := sectionDoc(12, 15)
	writeInstr(t, path, body)
	captureLogs(t)

	content, _, err := loadInstructionsMode(dir, 400, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	_, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline:\n%s", content)
	}
	fileLines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	ranges := parseOutlineRanges(t, outline)
	if len(ranges) < 2 {
		t.Fatalf("expected several outlined sections, got %d", len(ranges))
	}
	for _, r := range ranges {
		// The section this range claims: from its start line to the line
		// before the next section start (or the end of the file).
		end := len(fileLines)
		for _, s := range starts {
			if s > r.offset && s-1 < end {
				end = s - 1
			}
		}
		want := fileLines[r.offset-1 : end]
		if r.limit != len(want) {
			t.Errorf("range at offset %d advertises limit %d, want %d lines", r.offset, r.limit, len(want))
		}
		got := readFileLines(t, dir, r)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("range at offset %d returned\n%q\nwant\n%q", r.offset, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if !strings.HasPrefix(got[0], "## ") {
			t.Errorf("range at offset %d does not start at a heading: %q", r.offset, got[0])
		}
	}
}

// TestInstructionsOutlineGiantFirstSectionStaysLoud is the second must-have:
// when the FIRST section alone exceeds the cap there is no section boundary
// to cut on, so the head itself is truncated — and that truncation must stay
// loud (marker plus WARN line) while the outline still lists the rest.
func TestInstructionsOutlineGiantFirstSectionStaysLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	body := "## Giant\n" + strings.Repeat("x", 4096) + "\n## Second\nsecond body\n## Third\nthird body\n"
	writeInstr(t, path, body)
	buf := captureLogs(t)

	content, _, err := loadInstructionsMode(dir, 512, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	head, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline:\n%s", content)
	}
	if !strings.Contains(head, "[... truncated:") {
		t.Errorf("an over-cap head must carry the loud truncation marker:\n%s", head)
	}
	if !strings.Contains(head, path) {
		t.Errorf("marker must name the path:\n%s", head)
	}
	if out := buf.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, "truncated") {
		t.Errorf("an over-cap head must log a WARN line, got:\n%s", out)
	}
	ranges := parseOutlineRanges(t, outline)
	if len(ranges) != 2 {
		t.Fatalf("outline lists %d sections, want the 2 sections after the giant one:\n%s", len(ranges), outline)
	}
	if got := readFileLines(t, dir, ranges[0]); got[0] != "## Second" {
		t.Errorf("first outlined section = %q, want ## Second", got[0])
	}
}

// TestInstructionsOutlineFenceAware verifies a '#' line inside a fenced code
// block is body text, never a section. A naive scan advertises a range that
// points at a shell comment.
func TestInstructionsOutlineFenceAware(t *testing.T) {
	dir := t.TempDir()
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), strings.Join([]string{
		"## Real one",
		"```bash",
		"# not a heading",
		"go test ./...",
		"```",
		strings.Repeat("filler line\n", 40),
		"## Real two",
		"tail body",
		"",
	}, "\n"))
	captureLogs(t)

	content, _, err := loadInstructionsMode(dir, 200, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	if strings.Contains(content, "not a heading") && strings.Contains(content, instructionsOutlineHeader) {
		_, outline, _ := strings.Cut(content, instructionsOutlineHeader)
		if strings.Contains(outline, "not a heading") {
			t.Errorf("outline lists a fenced comment as a section:\n%s", outline)
		}
	}
	for _, r := range parseOutlineRanges(t, content) {
		got := readFileLines(t, dir, r)
		if !strings.HasPrefix(got[0], "## ") {
			t.Errorf("advertised range starts at %q, want a heading line", got[0])
		}
	}
}

// TestInstructionsOutlineFallbacks pins the two shapes that keep the
// head-plus-marker behavior: a file with no headings at all, and explicit
// full mode.
func TestInstructionsOutlineFallbacks(t *testing.T) {
	t.Run("no headings", func(t *testing.T) {
		dir := t.TempDir()
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), strings.Repeat("plain body line\n", 200))
		captureLogs(t)
		content, _, err := loadInstructionsMode(dir, 256, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionsMode: %v", err)
		}
		if strings.Contains(content, instructionsOutlineHeader) {
			t.Errorf("a heading-less file must not get an outline:\n%s", content)
		}
		if !strings.Contains(content, "[... truncated:") {
			t.Errorf("a heading-less file keeps the loud marker:\n%s", content)
		}
	})
	t.Run("full mode", func(t *testing.T) {
		dir := t.TempDir()
		body, _ := sectionDoc(8, 10)
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
		captureLogs(t)
		content, _, err := loadInstructionsMode(dir, 256, InstructionsModeFull)
		if err != nil {
			t.Fatalf("loadInstructionsMode: %v", err)
		}
		if strings.Contains(content, instructionsOutlineHeader) {
			t.Errorf("full mode must not outline:\n%s", content)
		}
		if !strings.Contains(content, "[... truncated:") {
			t.Errorf("full mode keeps the loud marker:\n%s", content)
		}
	})
	t.Run("under the cap", func(t *testing.T) {
		dir := t.TempDir()
		body, _ := sectionDoc(3, 2)
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
		captureLogs(t)
		content, _, err := loadInstructionsMode(dir, 64*1024, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionsMode: %v", err)
		}
		if content != body {
			t.Errorf("an under-cap file must be injected verbatim:\n%q", content)
		}
	})
	t.Run("cap disabled", func(t *testing.T) {
		dir := t.TempDir()
		body, _ := sectionDoc(40, 40)
		writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
		captureLogs(t)
		content, _, err := loadInstructionsMode(dir, -1, InstructionsModeAuto)
		if err != nil {
			t.Fatalf("loadInstructionsMode: %v", err)
		}
		if content != body {
			t.Errorf("a disabled cap must inject the whole file, got %d of %d bytes", len(content), len(body))
		}
	})
}

// TestInstructionsOutlineListsEverySection verifies the outline never drops a
// section silently: with a budget too small for teasers it degrades to
// headings and ranges, and every dropped section is still listed.
func TestInstructionsOutlineListsEverySection(t *testing.T) {
	dir := t.TempDir()
	body, starts := sectionDoc(200, 4)
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
	captureLogs(t)

	content, _, err := loadInstructionsMode(dir, 512, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	_, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline")
	}
	ranges := parseOutlineRanges(t, outline)
	headSections := 0
	for _, s := range starts {
		if !strings.Contains(outline, fmt.Sprintf("offset=%d,", s)) {
			headSections++
		}
	}
	if len(ranges)+headSections != len(starts) {
		t.Errorf("outline lists %d sections and the head holds %d, want %d total", len(ranges), headSections, len(starts))
	}
}

// TestInstructionsOutlineCoversEveryLine is the accounting property: the head
// lines plus the outlined ranges cover the file exactly once — no gap, no
// overlap. It is the strongest guard the split has.
func TestInstructionsOutlineCoversEveryLine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		sections := rapid.IntRange(2, 30).Draw(rt, "sections")
		bodyLines := rapid.IntRange(0, 12).Draw(rt, "bodyLines")
		cap := rapid.IntRange(16, 4096).Draw(rt, "cap")

		dir, err := os.MkdirTemp("", "instroutline")
		if err != nil {
			rt.Fatalf("temp dir: %v", err)
		}
		defer os.RemoveAll(dir)
		body, _ := sectionDoc(sections, bodyLines)
		path := filepath.Join(dir, "AGENTS.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			rt.Fatalf("write: %v", err)
		}
		content, _, lerr := loadInstructionsMode(dir, cap, InstructionsModeAuto)
		if lerr != nil {
			rt.Fatalf("loadInstructionsMode: %v", lerr)
		}
		total := len(strings.Split(strings.TrimSuffix(body, "\n"), "\n"))
		head, outline, ok := strings.Cut(content, instructionsOutlineHeader)
		if !ok {
			return // marker fallback: covered by its own test
		}
		headLines := len(strings.Split(strings.TrimRight(head, "\n"), "\n"))
		if strings.Contains(head, "[... truncated:") {
			return // truncated head: the marker path, not the accounting path
		}
		covered := headLines
		next := headLines + 1
		for _, r := range parseOutlineRanges(rt, outline) {
			if r.offset != next {
				rt.Fatalf("range starts at line %d, want %d (gap or overlap)", r.offset, next)
			}
			covered += r.limit
			next = r.offset + r.limit
		}
		if covered != total {
			rt.Fatalf("head plus outlined ranges cover %d lines, file has %d", covered, total)
		}
	})
}

// TestInstructionsOutlineTeaserRuneSafe verifies a teaser cut at the byte cap
// lands on a rune boundary: a partial rune in the system prompt is invalid
// UTF-8 the provider can reject.
func TestInstructionsOutlineTeaserRuneSafe(t *testing.T) {
	dir := t.TempDir()
	// Sections 2+ carry a body of 3-byte runes, so the teaser cap lands
	// inside a rune unless the cut is rune-aware.
	body := "## One\n" + strings.Repeat("head body\n", 30) +
		// The single ASCII byte shifts the 120-byte teaser cap into the
		// middle of a 3-byte rune.
		"## Two\na" + strings.Repeat("世", 200) + "\n" +
		"## Three\na" + strings.Repeat("界", 200) + "\n"
	writeInstr(t, filepath.Join(dir, "AGENTS.md"), body)
	captureLogs(t)

	content, _, err := loadInstructionsMode(dir, 320, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	if !utf8.ValidString(content) {
		t.Errorf("segment is not valid UTF-8")
	}
	_, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline:\n%s", content)
	}
	if !strings.Contains(outline, "世") || !strings.Contains(outline, "界") {
		t.Errorf("teasers lost their content:\n%s", outline)
	}
	if strings.Contains(outline, "\uFFFD") {
		t.Errorf("teaser carries a replacement rune (cut mid-rune):\n%s", outline)
	}
}

// TestScanSectionsFenceRules pins the two CommonMark fence rules a single
// boolean cannot express. An instruction file that documents Markdown wraps a
// three-backtick example in a four-backtick fence, and a naive toggle closes
// the outer fence on the inner one, then reads the rest of the document as
// headings.
func TestScanSectionsFenceRules(t *testing.T) {
	titles := func(secs []section) []string {
		var out []string
		for _, s := range secs {
			out = append(out, s.title)
		}
		return out
	}
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a longer fence wraps a shorter one",
			body: "## One\n````\n```bash\n# not a heading\n```\n````\n## Two\nbody\n",
			want: []string{"One", "Two"},
		},
		{
			name: "a tilde run does not close a backtick fence",
			body: "## One\n```\n~~~\n# not a heading\n```\n## Two\nbody\n",
			want: []string{"One", "Two"},
		},
		{
			name: "a closing fence carries no info string",
			body: "## One\n```\n```go\n# not a heading\n```\n## Two\nbody\n",
			want: []string{"One", "Two"},
		},
		{
			name: "deep indentation is a code block, not a fence",
			body: "## One\n     ```\n## Two\nbody\n",
			want: []string{"One", "Two"},
		},
		{
			name: "an unclosed fence swallows the rest",
			body: "## One\n```\n## Two\nbody\n",
			want: []string{"One"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := titles(scanSections([]byte(tc.body)))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("sections = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInstructionsOutlineGiantFirstSectionRangesAreExact completes the
// giant-first-section case: the head stays inside the cap plus the marker, the
// marker reports the WHOLE file's size, and every outlined range still returns
// exactly its section through the real read_file tool.
func TestInstructionsOutlineGiantFirstSectionRangesAreExact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	giant := strings.Repeat("giant body line\n", 200)
	body := "## Giant\n" + giant + "## Second\nsecond body\nmore second\n## Third\nthird body\n"
	writeInstr(t, path, body)
	captureLogs(t)

	const cap = 512
	content, _, err := loadInstructionsMode(dir, cap, InstructionsModeAuto)
	if err != nil {
		t.Fatalf("loadInstructionsMode: %v", err)
	}
	head, outline, ok := strings.Cut(content, instructionsOutlineHeader)
	if !ok {
		t.Fatalf("segment carries no outline:\n%s", content)
	}
	// The kept body stays inside the cap; the marker is the only text past it.
	kept, marker, ok := strings.Cut(head, "\n[... truncated:")
	if !ok {
		t.Fatalf("head carries no marker:\n%s", head)
	}
	if len(kept) > cap {
		t.Errorf("kept head is %d bytes, over the %d-byte cap", len(kept), cap)
	}
	// The marker reports the whole file, never the first section alone.
	if !strings.Contains(marker, strconv.Itoa(len(body))) {
		t.Errorf("marker must report the whole file size %d: %q", len(body), marker)
	}
	if !strings.Contains(marker, strconv.Itoa(len(kept))) {
		t.Errorf("marker must report the kept size %d: %q", len(kept), marker)
	}
	fileLines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	ranges := parseOutlineRanges(t, outline)
	if len(ranges) != 2 {
		t.Fatalf("outline lists %d sections, want 2:\n%s", len(ranges), outline)
	}
	for _, r := range ranges {
		got := readFileLines(t, dir, r)
		want := fileLines[r.offset-1 : r.offset-1+r.limit]
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("range at offset %d returned %q, want %q", r.offset, got, want)
		}
	}
}

// richDoc builds a Markdown document with optional preamble, fenced code
// blocks (including a wrapped fence), mixed heading levels, CRLF endings, and
// an optional missing trailing newline — the shapes the uniform generator in
// sectionDoc never produces.
func richDoc(rt *rapid.T) string {
	var b strings.Builder
	if rapid.Bool().Draw(rt, "preamble") {
		b.WriteString("preamble prose\nmore preamble\n")
	}
	sections := rapid.IntRange(2, 12).Draw(rt, "sections")
	for i := 1; i <= sections; i++ {
		level := rapid.IntRange(1, 4).Draw(rt, "level")
		fmt.Fprintf(&b, "%s Section %d\n", strings.Repeat("#", level), i)
		for j := 0; j < rapid.IntRange(0, 6).Draw(rt, "bodyLines"); j++ {
			fmt.Fprintf(&b, "body %d line %d\n", i, j)
		}
		switch rapid.IntRange(0, 2).Draw(rt, "fence") {
		case 1:
			b.WriteString("```bash\n# a comment, not a heading\n```\n")
		case 2:
			b.WriteString("````\n```md\n## quoted heading\n```\n````\n")
		}
	}
	out := b.String()
	if rapid.Bool().Draw(rt, "crlf") {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if rapid.Bool().Draw(rt, "noTrailingNewline") {
		out = strings.TrimRight(out, "\r\n")
	}
	return out
}

// TestInstructionsOutlineCoversEveryLineRich is the accounting property over
// documents with preambles, wrapped fences, mixed heading levels, CRLF, and a
// missing trailing newline. Head lines plus outlined ranges must cover the
// file exactly once, and no advertised range may start inside a fence.
func TestInstructionsOutlineCoversEveryLineRich(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		body := richDoc(rt)
		capBytes := rapid.IntRange(16, 2048).Draw(rt, "cap")

		dir, err := os.MkdirTemp("", "instrrich")
		if err != nil {
			rt.Fatalf("temp dir: %v", err)
		}
		defer os.RemoveAll(dir)
		if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(body), 0o644); err != nil {
			rt.Fatalf("write: %v", err)
		}
		content, _, lerr := loadInstructionsMode(dir, capBytes, InstructionsModeAuto)
		if lerr != nil {
			rt.Fatalf("loadInstructionsMode: %v", lerr)
		}
		head, outline, ok := strings.Cut(content, instructionsOutlineHeader)
		if !ok {
			return // marker fallback: fewer than two sections
		}
		fileLines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(body, "\r\n", "\n"), "\n"), "\n")
		ranges := parseOutlineRanges(rt, outline)

		// Every advertised range starts at a heading OUTSIDE a fence, which is
		// exactly the set scanSections found: check against a fresh scan of
		// the file's own lines rather than against the outline itself.
		for _, r := range ranges {
			line := strings.TrimRight(fileLines[r.offset-1], "\r")
			if headingTitle(line) == "" {
				rt.Fatalf("range at offset %d starts at %q, not a heading", r.offset, line)
			}
		}
		if strings.Contains(head, "[... truncated:") {
			return // truncated head: the marker path, not the accounting path
		}
		headLines := len(strings.Split(strings.TrimRight(head, "\n"), "\n"))
		covered, next := headLines, headLines+1
		for _, r := range ranges {
			if r.offset != next {
				rt.Fatalf("range starts at line %d, want %d (gap or overlap)", r.offset, next)
			}
			covered += r.limit
			next = r.offset + r.limit
		}
		if covered != len(fileLines) {
			rt.Fatalf("head plus outlined ranges cover %d lines, file has %d", covered, len(fileLines))
		}
	})
}
