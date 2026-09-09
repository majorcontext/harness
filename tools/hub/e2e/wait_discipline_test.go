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

// minBoundMS is the floor every wait bound in real_e2e.mjs must clear. A
// bound decides how loudly a genuine hang fails, never how fast a passing
// run finishes, so a short one buys nothing and costs a false failure on a
// loaded runner.
const minBoundMS = 15000

// TestRealE2EWaitsPollConditionsWithGenerousBounds guards real_e2e.mjs
// against the failure that made its predecessor red in CI again and again:
// a wait that measures the clock instead of the state.
//
// The removed session monitor's copy of this script died that way twice. A
// fixed delay before an assertion encodes "a real HTTP round trip plus a
// real render finishes in N ms", which is false on a loaded runner and
// vacuous on a fast one — the assertion either fails for no product reason
// or passes without observing anything. Short bounds did the same:
// harness#184 raised every bound in the monitor's script after three CI
// runs died 12-14s into 4-8s windows, and proved it with a one-off grep
// that nothing since re-ran.
//
// Go, not node, so `go test -race ./...` runs it with no toolchain
// prerequisite: TestRealEndToEnd itself skips without node on PATH, and
// this rule must hold in that environment too.
//
// Scope: this reads one line at a time, so it names the shapes a delay
// actually takes in this file — an awaited sleep, an awaited setTimeout, a
// hand-rolled Promise delay — rather than claiming to catch every possible
// spelling across lines.
func TestRealE2EWaitsPollConditionsWithGenerousBounds(t *testing.T) {
	src := readRealE2E(t)
	lo, hi := waitForBody(t, src)

	for _, d := range delaySites(src) {
		if d.offset < lo || d.offset > hi {
			t.Errorf("line %d holds a fixed delay outside waitFor: %s\na delay before an assertion is a guessed deadline — poll the condition through waitFor instead", d.line, d.text)
		}
	}

	bounds := regexp.MustCompile(`timeoutMs\s*[:=]\s*([^,}\n]+)`).FindAllStringSubmatch(src, -1)
	if len(bounds) == 0 {
		t.Fatal("no timeoutMs bound found in real_e2e.mjs; this guard must be updated with the wait primitive it protects")
	}
	for _, m := range bounds {
		ms, ok := literalMS(m[1])
		if !ok {
			t.Errorf("bound %q is not a numeric literal; write the milliseconds inline so this floor can read them", strings.TrimSpace(m[0]))
			continue
		}
		if ms < minBoundMS {
			t.Errorf("%s is below the %dms floor; a loaded runner blows a short window with nothing wrong", strings.TrimSpace(m[0]), minBoundMS)
		}
	}
}

// literalMS reads a bound written inline, as a plain integer ("20000",
// "15_000") or a product of them ("30 * 1000"). Anything else — an
// identifier, a call, arithmetic this does not model — returns false, so a
// bound this floor cannot read is reported rather than skipped silently.
func literalMS(expr string) (int, bool) {
	total := 1
	for _, part := range strings.Split(strings.TrimSpace(expr), "*") {
		digits := strings.ReplaceAll(strings.TrimSpace(part), "_", "")
		n, err := strconv.Atoi(digits)
		if err != nil {
			return 0, false
		}
		total *= n
	}
	return total, true
}

func readRealE2E(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine tools/hub/e2e directory")
	}
	script := filepath.Join(filepath.Dir(thisFile), "real_e2e.mjs")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading %s: %v", script, err)
	}
	return string(b)
}

type delaySite struct {
	offset int
	line   int
	text   string
}

var (
	awaitedSleep = regexp.MustCompile(`await\s+sleep\s*\(`)
	sleepHelper  = regexp.MustCompile(`^(const|let|var)\s+sleep\s*=`)
)

// delaySites finds every delay in executable source: an awaited sleep, an
// awaited setTimeout, and any other Promise wrapped around setTimeout — the
// hand-rolled sleep. The script's own sleep helper is the one allowed
// definition, and jsdom's requestAnimationFrame polyfill (a bare setTimeout,
// never awaited) is not a delay. Comment lines are skipped: waitFor's doc
// comment quotes the banned shape to explain why it is banned.
func delaySites(src string) []delaySite {
	var out []delaySite
	offset := 0
	for n, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "//"), strings.HasPrefix(trimmed, "*"), sleepHelper.MatchString(trimmed):
		default:
			timed := strings.Contains(line, "setTimeout(")
			awaited := strings.Contains(line, "await")
			hand := timed && strings.Contains(line, "new Promise(")
			if loc := awaitedSleep.FindStringIndex(line); loc != nil {
				out = append(out, delaySite{offset: offset + loc[0], line: n + 1, text: trimmed})
			} else if hand || (timed && awaited) {
				out = append(out, delaySite{offset: offset + strings.Index(line, "setTimeout("), line: n + 1, text: trimmed})
			}
		}
		offset += len(line) + 1
	}
	return out
}

// waitForBody returns the byte range of waitFor's body.
func waitForBody(t *testing.T, src string) (int, int) {
	t.Helper()
	decl := strings.Index(src, "async function waitFor(")
	if decl < 0 {
		t.Fatal("real_e2e.mjs must keep waitFor as its single wait primitive")
	}
	// Skip the parameter list: waitFor's options argument is itself a
	// destructured object literal, so the first brace after the declaration
	// belongs to the signature, not the body.
	parens, afterParams := 0, -1
	for i := decl; i < len(src) && afterParams < 0; i++ {
		switch src[i] {
		case '(':
			parens++
		case ')':
			if parens--; parens == 0 {
				afterParams = i
			}
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
			if depth--; depth == 0 {
				return open, i
			}
		}
	}
	t.Fatal("waitFor's body is unbalanced")
	return 0, 0
}
