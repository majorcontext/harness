package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestRealE2EWaitsPollConditionsWithGenerousBounds guards real_e2e.mjs
// against the one failure that made its predecessor red in CI again and
// again: a wait that measures the clock instead of the state.
//
// The removed session monitor's copy of this script died that way twice.
// A fixed `await sleep(N)` before an assertion encodes "a real HTTP round
// trip plus a real render finishes in N ms", which is false on a loaded
// runner and vacuous on a fast one — the assertion either fails for no
// product reason or passes without observing anything. Short wait bounds
// did the same: harness#184 raised every bound in the monitor's script
// after three CI runs died 12-14s into 4-8s windows, and verified the
// change with a one-off grep that nothing since re-ran.
//
// Go, not node, so `go test -race ./...` runs it with no toolchain
// prerequisite: TestRealEndToEnd itself skips without node on PATH, and
// this rule must hold in that environment too.
func TestRealE2EWaitsPollConditionsWithGenerousBounds(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine tools/hub/e2e directory")
	}
	script := filepath.Join(filepath.Dir(thisFile), "real_e2e.mjs")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading %s: %v", script, err)
	}
	src := string(b)

	// The polling primitive is the ONLY place a delay belongs: it sleeps
	// between two evaluations of the caller's condition, never before an
	// assertion.
	lo, hi := waitForBody(t, src)
	for _, d := range awaitedSleeps(src) {
		if d.offset < lo || d.offset > hi {
			t.Errorf("line %d awaits a fixed delay outside waitFor: %s\na delay before an assertion is a guessed deadline — poll the condition through waitFor instead", d.line, d.text)
		}
	}

	// Every bound, the default and each override, stays far above real
	// latency: a bound decides how loudly a genuine hang fails, never how
	// fast a passing run finishes.
	const minBound = 15000
	bounds := regexp.MustCompile(`timeoutMs\s*[:=]\s*(\d+)`).FindAllStringSubmatch(src, -1)
	if len(bounds) == 0 {
		t.Fatal("no timeoutMs bound found in real_e2e.mjs; this guard must be updated with the wait primitive it protects")
	}
	for _, m := range bounds {
		ms, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parsing %q: %v", m[0], err)
		}
		if ms < minBound {
			t.Errorf("%s is below the %dms floor; a loaded runner blows a short window with nothing wrong", m[0], minBound)
		}
	}
}

// waitForBody returns the byte range of waitFor's function body, matching
// braces from its declaration.
func waitForBody(t *testing.T, src string) (int, int) {
	t.Helper()
	decl := strings.Index(src, "async function waitFor(")
	if decl < 0 {
		t.Fatal("real_e2e.mjs must keep waitFor as its single wait primitive")
	}
	// Skip the parameter list first: waitFor's options argument is itself a
	// destructured object literal, so the first brace after the declaration
	// belongs to the signature, not the body.
	parens := 0
	afterParams := -1
	for i := decl; i < len(src); i++ {
		switch src[i] {
		case '(':
			parens++
		case ')':
			parens--
			if parens == 0 {
				afterParams = i
			}
		}
		if afterParams >= 0 {
			break
		}
	}
	if afterParams < 0 {
		t.Fatal("could not find the end of waitFor's parameter list")
	}
	open := strings.Index(src[afterParams:], "{")
	if open < 0 {
		t.Fatal("could not find waitFor's body")
	}
	open += afterParams
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return open, i
			}
		}
	}
	t.Fatal("waitFor's body is unbalanced")
	return 0, 0
}

type sleepSite struct {
	offset int
	line   int
	text   string
}

// awaitedSleeps finds every awaited delay in executable source. Comment
// lines are skipped: waitFor's own doc comment quotes the banned shape to
// explain why it is banned.
func awaitedSleeps(src string) []sleepSite {
	var out []sleepSite
	offset := 0
	for n, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "*") {
			if i := strings.Index(line, "await sleep("); i >= 0 {
				out = append(out, sleepSite{offset: offset + i, line: n + 1, text: trimmed})
			}
		}
		offset += len(line) + 1
	}
	return out
}
