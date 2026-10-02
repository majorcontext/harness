// Package gates measures Go files and AGENTS.md files against a ratchet
// baseline that can only go down.
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
	warnShare          = 0.15
	maxShare           = 0.25
	maxNewPackageRatio = 1.5
	maxFileLines       = 800
	maxFuncLines       = 80
	rootAgentsMax      = 80
	agentsMax          = 25
)

// FileMetrics holds the measured line counts and rule hits of one Go file.
type FileMetrics struct {
	Lines          int `json:"lines"`
	CommentLines   int `json:"comment_lines"`
	CodeLines      int `json:"code_lines"`
	LongFuncs      int `json:"long_funcs"`
	HistoryMarkers int `json:"history_markers"`
	SleepAfter     int `json:"sleep_after"`
}

// PackageMetrics holds the summed test and code lines of one package.
type PackageMetrics struct {
	TestLines int `json:"test_lines"`
	CodeLines int `json:"code_lines"`
}

// Report is the full measurement of a tree: files, packages, and AGENTS.md sizes.
type Report struct {
	Files    map[string]FileMetrics    `json:"files"`
	Packages map[string]PackageMetrics `json:"packages"`
	Agents   map[string]int            `json:"agents,omitempty"`
}

// Violation names one rule broken by one path.
type Violation struct{ Path, Rule, Detail string }

var (
	historyRE   = regexp.MustCompile(`(?i)#[0-9]{2,}|\b(19|20)[0-9]{2}-[0-9]{2}-[0-9]{2}\b|\b(previously|no longer|used to|before this change|red-verified|confirmed live|an earlier version|findings?|(fix|review) rounds?|review caught|copilot|round [0-9]+)\b`)
	generatedRE = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
	skipDirs    = map[string]bool{"testdata": true, ".worktrees": true, ".claude": true, ".git": true, "node_modules": true}
	directives  = []string{"//go:", "//nolint", "//lint:", "//line "}
)

// Collect measures every Go file and AGENTS.md file in fsys.
func Collect(fsys fs.FS) (Report, error) {
	r := Report{
		Files:    map[string]FileMetrics{},
		Packages: map[string]PackageMetrics{},
		Agents:   map[string]int{},
	}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
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
			first, _, _ := bytes.Cut(data, []byte("\n"))
			if generatedRE.Match(first) {
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
	for i, l := range lines {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if commentLine[i+1] {
			m.CommentLines++
		} else {
			m.CodeLines++
		}
	}
	isTest := strings.HasSuffix(name, "_test.go")
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Body != nil && fset.PositionFor(n.Body.End(), false).Line-fset.PositionFor(n.Body.Pos(), false).Line > maxFuncLines {
				m.LongFuncs++
			}
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && isTest && id.Name == "time" && (n.Sel.Name == "Sleep" || n.Sel.Name == "After") {
				m.SleepAfter++
			}
		}
		return true
	})
	return m, nil
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

func absolutes(m FileMetrics) []Violation {
	var vs []Violation
	add := func(rule, format string, a ...any) {
		vs = append(vs, Violation{Rule: rule, Detail: fmt.Sprintf(format, a...)})
	}
	if s := commentShare(m); s > maxShare {
		add("comment_share", "%d of %d lines are comments, limit %.0f%%", m.CommentLines, m.CommentLines+m.CodeLines, maxShare*100)
	}
	if m.Lines > maxFileLines {
		add("file_size", "%d lines, limit %d", m.Lines, maxFileLines)
	}
	if m.LongFuncs > 0 {
		add("long_func", "%d functions over %d lines", m.LongFuncs, maxFuncLines)
	}
	if m.HistoryMarkers > 0 {
		add("history", "%d history markers in comments", m.HistoryMarkers)
	}
	if m.SleepAfter > 0 {
		add("sleep_after", "%d time.Sleep or time.After calls in a test", m.SleepAfter)
	}
	return vs
}

func exceeds(m, b FileMetrics) bool {
	return m.Lines > b.Lines || m.CommentLines > b.CommentLines || m.CodeLines > b.CodeLines ||
		m.LongFuncs > b.LongFuncs || m.HistoryMarkers > b.HistoryMarkers || m.SleepAfter > b.SleepAfter
}

func ratioRises(m, b PackageMetrics) bool {
	return m.TestLines*b.CodeLines > b.TestLines*m.CodeLines
}

// ratioFailure explains a test:code ratio violation, or returns "". A package
// without code has no gate. A package absent from base, or with no baseline
// test lines, may not pass maxNewPackageRatio. Any other package may not add
// test lines while its ratio rises above its baseline.
func ratioFailure(m, b PackageMetrics, inBase bool) string {
	if m.CodeLines == 0 {
		return ""
	}
	if !inBase || b.TestLines == 0 {
		if float64(m.TestLines) > maxNewPackageRatio*float64(m.CodeLines) {
			return fmt.Sprintf("test:code %d:%d exceeds limit %.1f", m.TestLines, m.CodeLines, maxNewPackageRatio)
		}
		return ""
	}
	if m.TestLines > b.TestLines && ratioRises(m, b) {
		return fmt.Sprintf("test:code %d:%d exceeds baseline %d:%d", m.TestLines, m.CodeLines, b.TestLines, b.CodeLines)
	}
	return ""
}

// Check returns the violations of absolute rules and of the ratchet against base.
func Check(r Report, base Report) []Violation {
	var vs []Violation
	for p, m := range r.Files {
		if b, ok := base.Files[p]; ok {
			if exceeds(m, b) {
				vs = append(vs, Violation{p, "ratchet", fmt.Sprintf("%+v exceeds baseline %+v", m, b)})
			}
			continue
		}
		for _, v := range absolutes(m) {
			v.Path = p
			vs = append(vs, v)
		}
	}
	for p, m := range r.Packages {
		b, ok := base.Packages[p]
		if d := ratioFailure(m, b, ok); d != "" {
			vs = append(vs, Violation{p, "test_ratio", d})
		}
	}
	for p, n := range r.Agents {
		limit := agentsMax
		if p == "AGENTS.md" {
			limit = rootAgentsMax
		}
		if n > limit {
			vs = append(vs, Violation{p, "agents_cap", fmt.Sprintf("%d lines, limit %d", n, limit)})
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

// Seed builds a first baseline: every package ratio and every file that
// breaks an absolute rule.
func Seed(r Report) Report {
	next := Report{Files: map[string]FileMetrics{}, Packages: map[string]PackageMetrics{}}
	for p, m := range r.Files {
		if len(absolutes(m)) > 0 {
			next.Files[p] = m
		}
	}
	for p, m := range r.Packages {
		next.Packages[p] = m
	}
	return next
}

// Lower returns base set to the current metrics. A file leaves the baseline
// when it is gone or now meets the absolute rules. A file absent from base
// never enters it. Every current package is recorded. A rise is an error.
func Lower(r Report, base Report) (Report, error) {
	next := Report{Files: map[string]FileMetrics{}, Packages: map[string]PackageMetrics{}}
	for p, b := range base.Files {
		m, ok := r.Files[p]
		if !ok {
			continue
		}
		if exceeds(m, b) {
			return Report{}, fmt.Errorf("%s: %+v exceeds baseline %+v", p, m, b)
		}
		if len(absolutes(m)) > 0 {
			next.Files[p] = m
		}
	}
	for p, m := range r.Packages {
		b, ok := base.Packages[p]
		if d := ratioFailure(m, b, ok); d != "" {
			return Report{}, fmt.Errorf("%s: %s", p, d)
		}
		next.Packages[p] = m
	}
	return next, nil
}
