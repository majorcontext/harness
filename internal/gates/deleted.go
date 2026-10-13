package gates

import (
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// deletedRules match a reference to a package path that does not exist or
// lives under internal/: a path under engine, server, or mcpserver at the
// module root, a symbol of the engine package, provider/claudecode, the
// module path of a package that moved under internal/ (provider included), and
// harness-migrate. A rule runs only on a line that holds its literal, which
// its pattern contains.
var deletedRules = []struct {
	literal string
	re      *regexp.Regexp
}{
	{"engine/", regexp.MustCompile(`(?:^|[^\w/.-])engine/\w`)},
	{"server/", regexp.MustCompile(`(?:^|[^\w/.-])(?:mcp)?server/\w`)},
	{"engine.", regexp.MustCompile(`(?:^|[^\w/.-])engine\.[A-Za-z_]`)},
	{"provider/claudecode", regexp.MustCompile(`\bprovider/claudecode\b`)},
	{"majorcontext/harness/", regexp.MustCompile(`majorcontext/harness/(?:engine|server|mcpserver|message|modelmeta|mcp|skill|command|process|imageclamp|migrate|provider)\b`)},
	{"harness-migrate", regexp.MustCompile(`\bharness-migrate\b`)},
}

func namesDeletedPath(line string) bool {
	for _, r := range deletedRules {
		if strings.Contains(line, r.literal) && r.re.MatchString(line) {
			return true
		}
	}
	return false
}

func mayNameDeletedPath(text string) bool {
	for _, r := range deletedRules {
		if strings.Contains(text, r.literal) {
			return true
		}
	}
	return false
}

// CheckDeletedReferences fails each Go file, in code or in a comment, that
// names a deleted package path (spec: Internal packages), except the
// files of allow. The gate package is out of scope: it holds the pattern.
func CheckDeletedReferences(fsys fs.FS, allow map[string]string) ([]Violation, error) {
	var out []Violation
	hit := map[string]bool{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (skipDirs[d.Name()] || nestedRoot(fsys, p) || p == "internal/gates") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		text := string(data)
		if !mayNameDeletedPath(text) {
			return nil
		}
		for i, line := range strings.Split(text, "\n") {
			if namesDeletedPath(line) {
				hit[p] = true
				if _, listed := allow[p]; !listed {
					out = append(out, Violation{Path: p, Rule: "deleted_reference", Detail: fmt.Sprintf("line %d names a deleted package path: %s", i+1, strings.TrimSpace(line))})
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range slices.Sorted(maps.Keys(allow)) {
		if !hit[p] {
			out = append(out, Violation{Path: p, Rule: "deleted_reference", Detail: "the allow list names a file that is no violation; delete the entry"})
		}
	}
	return out, nil
}
