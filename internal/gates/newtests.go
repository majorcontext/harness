package gates

import (
	"bytes"
	"go/ast"
	"go/token"
	"strings"
	"unicode"
	"unicode/utf8"
)

// testScopeLines counts the code lines of f that can hold a test: every Test,
// Benchmark, Fuzz, and Example function from signature to closing brace, and
// every package-level var declaration, which is where test tables live.
// Helpers, fakes, methods, and type declarations are outside the count.
func testScopeLines(fset *token.FileSet, f *ast.File, lines [][]byte, commentLine map[int]bool) int {
	n := 0
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body == nil || !isTestFunc(d) {
				continue
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
		default:
			continue
		}
		first, last := fset.PositionFor(d.Pos(), false).Line, fset.PositionFor(d.End(), false).Line
		for l := first; l <= last && l <= len(lines); l++ {
			if !commentLine[l] && len(bytes.TrimSpace(lines[l-1])) > 0 {
				n++
			}
		}
	}
	return n
}

func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil {
		return false
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		rest, ok := strings.CutPrefix(fn.Name.Name, prefix)
		if !ok {
			continue
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return rest == "" || !unicode.IsLower(r)
	}
	return false
}
