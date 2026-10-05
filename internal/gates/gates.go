// Package gates measures Go files and AGENTS.md files and compares the
// measurement with the merge base of the branch under test.
package gates

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	warnShare     = 0.15
	maxShare      = 0.25
	maxTestRatio  = 1.5
	maxFileLines  = 800
	maxFuncLines  = 80
	rootAgentsMax = 80
	agentsMax     = 25

	exceptionsFile = "testdata/test-exceptions.txt"
)

// testGrowthDirs holds the directories whose tests may grow: the contract
// suite, the gate itself, and the packages of pure code.
var testGrowthDirs = []string{
	"e2e",
	"internal/eventlog",
	"config",
	"message",
	"internal/gates",
}

// testGrowthFiles holds the test files of the wire mapping that may grow. A
// directory prefix cannot name them: the packages around them drive processes.
var testGrowthFiles = map[string]bool{
	"internal/backend/modelapi/convert_test.go":  true,
	"internal/backend/claudecode/frames_test.go": true,
}

// FileMetrics holds the measured line counts and rule hits of one Go file.
type FileMetrics struct {
	Lines          int
	CommentLines   int
	CodeLines      int
	LongFuncs      int
	HistoryMarkers int
	SleepAfter     int
}

// PackageMetrics holds the summed test and code lines of one package.
type PackageMetrics struct {
	TestLines int
	CodeLines int
}

// Report is the full measurement of a tree: files, packages, AGENTS.md sizes,
// and the test exceptions file.
type Report struct {
	Files    map[string]FileMetrics
	Packages map[string]PackageMetrics
	Agents   map[string]int
	// Exceptions maps each path listed in the exceptions file to its reason.
	Exceptions map[string]string
	// BadExceptions holds the lines of the exceptions file that name no reason.
	BadExceptions []string
}

// Violation names one rule broken by one path.
type Violation struct{ Path, Rule, Detail string }

var (
	historyRE  = regexp.MustCompile(`(?i)#[0-9]+|\b(19|20)[0-9]{2}-[0-9]{2}-[0-9]{2}\b|\b(previously|no longer|red-verified|confirmed live|an earlier version|before this change|(fix|review) rounds?|round [0-9]+|copilot)\b`)
	skipDirs   = map[string]bool{"testdata": true, ".worktrees": true, ".claude": true, ".git": true, "node_modules": true}
	directives = []string{"//go:", "//nolint", "//lint:", "//line "}
)

// Collect measures every Go file and AGENTS.md file in fsys.
func Collect(fsys fs.FS) (Report, error) {
	r := Report{
		Files:    map[string]FileMetrics{},
		Packages: map[string]PackageMetrics{},
		Agents:   map[string]int{},
	}
	r.Exceptions, r.BadExceptions = loadExceptions(fsys)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (skipDirs[d.Name()] || nestedRoot(fsys, p)) {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.Name() == "AGENTS.md":
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			r.Agents[p] = bytes.Count(data, []byte("\n"))
			if len(data) > 0 && data[len(data)-1] != '\n' {
				r.Agents[p]++
			}
		case strings.HasSuffix(p, ".go"):
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			if generated(p, data) {
				return nil
			}
			m, err := measure(p, data)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			r.Files[p] = m
			pm := r.Packages[path.Dir(p)]
			if strings.HasSuffix(p, "_test.go") {
				pm.TestLines += m.CodeLines
			} else {
				pm.CodeLines += m.CodeLines
			}
			r.Packages[path.Dir(p)] = pm
		}
		return nil
	})
	return r, err
}

func loadExceptions(fsys fs.FS) (map[string]string, []string) {
	listed := map[string]string{}
	var bad []string
	data, err := fs.ReadFile(fsys, exceptionsFile)
	if err != nil {
		return listed, nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, reason, _ := strings.Cut(line, " ")
		if reason = strings.TrimSpace(reason); reason == "" {
			bad = append(bad, line)
			continue
		}
		listed[name] = reason
	}
	return listed, bad
}

func generated(name string, src []byte) bool {
	f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.PackageClauseOnly|parser.ParseComments)
	return err == nil && ast.IsGenerated(f)
}

func nestedRoot(fsys fs.FS, dir string) bool {
	for _, marker := range []string{"go.mod", ".git"} {
		if _, err := fs.Stat(fsys, path.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

func measure(name string, src []byte) (FileMetrics, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return FileMetrics{}, err
	}
	lines := bytes.Split(src, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	m := FileMetrics{Lines: len(lines)}
	commentLine := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if hasDirective(c.Text) {
				continue
			}
			m.HistoryMarkers += len(historyRE.FindAllString(c.Text, -1))
			start, end := fset.PositionFor(c.Pos(), false), fset.PositionFor(c.End(), false)
			if len(bytes.TrimSpace(lines[start.Line-1][:start.Column-1])) > 0 {
				continue
			}
			for l := start.Line; l <= end.Line; l++ {
				commentLine[l] = true
			}
		}
	}
	exempt := func(line int) bool {
		return f.Doc != nil && path.Base(name) == "doc.go" &&
			line >= fset.PositionFor(f.Doc.Pos(), false).Line && line <= fset.PositionFor(f.Doc.End(), false).Line
	}
	for i, l := range lines {
		if len(bytes.TrimSpace(l)) == 0 || exempt(i+1) {
			continue
		}
		if commentLine[i+1] {
			m.CommentLines++
		} else {
			m.CodeLines++
		}
	}
	timePkg := ""
	if strings.HasSuffix(name, "_test.go") {
		timePkg = timeImportName(f)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Body != nil && fset.PositionFor(n.Body.End(), false).Line-fset.PositionFor(n.Body.Pos(), false).Line > maxFuncLines {
				m.LongFuncs++
			}
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && timePkg != "" && id.Name == timePkg && (n.Sel.Name == "Sleep" || n.Sel.Name == "After") {
				m.SleepAfter++
			}
		}
		return true
	})
	return m, nil
}

func timeImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if imp.Path.Value != `"time"` {
			continue
		}
		switch {
		case imp.Name == nil:
			return "time"
		case imp.Name.Name != "_" && imp.Name.Name != ".":
			return imp.Name.Name
		}
	}
	return ""
}

func hasDirective(text string) bool {
	for _, d := range directives {
		if strings.HasPrefix(text, d) {
			return true
		}
	}
	return false
}

func commentShare(m FileMetrics) float64 {
	if n := m.CommentLines + m.CodeLines; n > 0 {
		return float64(m.CommentLines) / float64(n)
	}
	return 0
}

// within reports whether head meets limit, or has not grown past a base that
// already broke it.
func within(head, base, limit int) bool { return head <= max(limit, base) }

func fileViolations(h, b FileMetrics) []Violation {
	var vs []Violation
	add := func(rule, format string, a ...any) {
		vs = append(vs, Violation{Rule: rule, Detail: fmt.Sprintf(format, a...)})
	}
	deletion := h.CodeLines < b.CodeLines && h.CommentLines <= b.CommentLines
	if limit := max(maxShare, commentShare(b)); !deletion && commentShare(h) > limit {
		add("comment_share", "%d of %d lines are comments, limit %.1f%%", h.CommentLines, h.CommentLines+h.CodeLines, limit*100)
	}
	if !within(h.Lines, b.Lines, maxFileLines) {
		add("file_size", "%d lines, limit %d", h.Lines, max(maxFileLines, b.Lines))
	}
	if !within(h.LongFuncs, b.LongFuncs, 0) {
		add("long_func", "%d functions over %d lines, limit %d", h.LongFuncs, maxFuncLines, b.LongFuncs)
	}
	if !within(h.HistoryMarkers, b.HistoryMarkers, 0) {
		add("history", "%d history markers in comments, limit %d", h.HistoryMarkers, b.HistoryMarkers)
	}
	if !within(h.SleepAfter, b.SleepAfter, 0) {
		add("sleep_after", "%d time.Sleep or time.After calls in a test, limit %d", h.SleepAfter, b.SleepAfter)
	}
	return vs
}

// ratioFailure explains a test:code ratio violation, or returns "". A package
// without code has no gate. A ratio up to maxTestRatio always passes. A
// package absent from base may not pass it. A change that removes code and
// adds no test lines always passes. Any other package may not raise its ratio
// above the merge base.
func ratioFailure(h, b PackageMetrics, inBase bool) string {
	if h.CodeLines == 0 || h == b || h.TestLines*2 <= h.CodeLines*3 {
		return ""
	}
	if inBase && h.CodeLines < b.CodeLines && h.TestLines <= b.TestLines {
		return ""
	}
	if !inBase || b == (PackageMetrics{}) {
		return fmt.Sprintf("test:code %d:%d exceeds limit %.1f", h.TestLines, h.CodeLines, maxTestRatio)
	}
	if h.TestLines*b.CodeLines > b.TestLines*h.CodeLines {
		return fmt.Sprintf("test:code %d:%d exceeds merge base %d:%d", h.TestLines, h.CodeLines, b.TestLines, b.CodeLines)
	}
	return ""
}

// oldPackage names the base package that most of the renamed files of the
// head package dir came from, or dir when no file moved in.
func oldPackage(dir string, head Report, renames map[string]string) string {
	from := map[string]int{}
	for p, old := range renames {
		if _, ok := head.Files[p]; ok && path.Dir(p) == dir {
			from[path.Dir(old)]++
		}
	}
	best := dir
	for d, n := range from {
		if n > from[best] || n == from[best] && d < best {
			best = d
		}
	}
	return best
}

func testMayGrow(p string) bool {
	if testGrowthFiles[p] {
		return true
	}
	if rest, ok := strings.CutPrefix(p, "provider/"); ok && strings.Contains(rest, "/") {
		return true
	}
	for _, d := range testGrowthDirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// Check returns the violations of head against base. Only files in changed
// are checked. A changed _test.go file may not add test lines unless
// testMayGrow names it or the exceptions file lists it with a reason. The
// count is net per file: a change that deletes and adds the same number of
// test lines passes, as it does in the other merge-base rules.
// A file absent from base is new and meets the absolute limits,
// unless renames maps it to a base path. A package absent from base compares
// with the base package that its files came from.
func Check(head, base Report, changed map[string]bool, renames map[string]string) []Violation {
	baseOf := func(p string) string {
		if old, ok := renames[p]; ok {
			return old
		}
		return p
	}
	var vs []Violation
	for p, m := range head.Files {
		if !changed[p] {
			continue
		}
		for _, v := range fileViolations(m, base.Files[baseOf(p)]) {
			v.Path = p
			vs = append(vs, v)
		}
	}
	for p, m := range head.Files {
		if !changed[p] || !strings.HasSuffix(p, "_test.go") || m.CodeLines <= base.Files[baseOf(p)].CodeLines {
			continue
		}
		if _, ok := head.Exceptions[p]; !ok && !testMayGrow(p) {
			vs = append(vs, Violation{p, "contract_tests", fmt.Sprintf("%d test lines added outside the contract suite and pure code; write a contract row in e2e/ or list the file with a reason in %s", m.CodeLines-base.Files[baseOf(p)].CodeLines, exceptionsFile)})
		}
	}
	for _, line := range head.BadExceptions {
		if !changed[exceptionsFile] {
			break
		}
		vs = append(vs, Violation{exceptionsFile, "test_exceptions", fmt.Sprintf("%q names no reason", line)})
	}
	for p, n := range head.Agents {
		limit := agentsMax
		if p == "AGENTS.md" {
			limit = rootAgentsMax
		}
		if b := base.Agents[baseOf(p)]; changed[p] && !within(n, b, limit) {
			vs = append(vs, Violation{p, "agents_cap", fmt.Sprintf("%d lines, limit %d", n, max(limit, b))})
		}
	}
	for p, m := range head.Packages {
		b, ok := base.Packages[p]
		if !ok {
			b, ok = base.Packages[oldPackage(p, head, renames)]
		}
		if d := ratioFailure(m, b, ok); d != "" {
			vs = append(vs, Violation{p, "test_ratio", d})
		}
	}
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Path != vs[j].Path {
			return vs[i].Path < vs[j].Path
		}
		return vs[i].Rule < vs[j].Rule
	})
	return vs
}

// Warnings lists changed files whose comment share is above warnShare and
// within maxShare.
func Warnings(head Report, changed map[string]bool) []string {
	var out []string
	for p, m := range head.Files {
		if s := commentShare(m); changed[p] && s > warnShare && s <= maxShare {
			out = append(out, fmt.Sprintf("%s: comment share %.1f%% is above %.0f%%", p, s*100, warnShare*100))
		}
	}
	sort.Strings(out)
	return out
}
