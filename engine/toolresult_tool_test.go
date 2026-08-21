package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// retainedSession builds a session with retention enabled and one handle
// already retained, holding text. It returns the session and the handle.
// It writes through the real writeRetainedToolResult path (sidecar file +
// durable record + in-memory metadata) rather than poking state directly, so
// every read test exercises the same bytes a live retention would leave.
func retainedSession(t *testing.T, text string) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewSession(Config{
		Providers:             provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:                 message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:            dir,
		ToolResultInlineBytes: 512,
	})
	handle, err := s.writeRetainedToolResult("bash", text)
	if err != nil {
		t.Fatal(err)
	}
	return s, handle
}

func readResult(t *testing.T, s *Session, args string) string {
	t.Helper()
	out, err := runReadToolResult(s, json.RawMessage(args))
	if err != nil {
		t.Fatalf("read_tool_result(%s): %v", args, err)
	}
	return out.Text()
}

// TestReadToolResultRangeRead: offset/limit select a 1-based line window.
func TestReadToolResultRangeRead(t *testing.T) {
	s, h := retainedSession(t, linesText(100))

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":10,"limit":3}`, h))

	for _, want := range []string{"line-10", "line-11", "line-12"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	for _, notWant := range []string{"line-9\n", "line-13"} {
		if strings.Contains(got, notWant) {
			t.Errorf("output contains out-of-window %q:\n%s", notWant, got)
		}
	}
	// A read that stopped early must SAY so and name the next offset —
	// otherwise a model treats a bounded window as the whole result.
	if !strings.Contains(got, "offset=13") {
		t.Errorf("output missing continuation notice naming offset=13:\n%s", got)
	}
}

// TestReadToolResultDefaultsToHead: with no offset/limit the read starts at
// line 1 and takes the default window.
func TestReadToolResultDefaultsToHead(t *testing.T) {
	s, h := retainedSession(t, linesText(1000))

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q}`, h))

	if !strings.Contains(got, "line-1\n") {
		t.Errorf("default read did not start at line 1:\n%s", got[:min(400, len(got))])
	}
	if !strings.Contains(got, fmt.Sprintf("line-%d", readToolResultDefaultLimit)) {
		t.Errorf("default read did not reach the default limit (%d)", readToolResultDefaultLimit)
	}
	if strings.Contains(got, fmt.Sprintf("line-%d\n", readToolResultDefaultLimit+1)) {
		t.Errorf("default read exceeded the default limit (%d)", readToolResultDefaultLimit)
	}
}

// TestReadToolResultLiteralSearch: search returns matching lines with their
// 1-based line numbers.
func TestReadToolResultLiteralSearch(t *testing.T) {
	text := "alpha\nbeta\nFAIL: something broke\ngamma\nFAIL: and again\ndelta\n"
	s, h := retainedSession(t, text)

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"FAIL:"}`, h))

	if !strings.Contains(got, "3: FAIL: something broke") {
		t.Errorf("missing numbered match at line 3:\n%s", got)
	}
	if !strings.Contains(got, "5: FAIL: and again") {
		t.Errorf("missing numbered match at line 5:\n%s", got)
	}
	if strings.Contains(got, "alpha") || strings.Contains(got, "gamma") {
		t.Errorf("search returned non-matching lines:\n%s", got)
	}

	// No match is a clean, informative result, not an error.
	none := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"NOSUCHTHING"}`, h))
	if !strings.Contains(none, "no lines match") {
		t.Errorf("no-match output = %q", none)
	}
}

// TestReadToolResultSearchIsLiteralNotRegex: a regex metacharacter is
// matched literally. Literal search is a contract, not an implementation
// detail — a model-authored regex against an unbounded file is a ReDoS
// surface with no upside here.
func TestReadToolResultSearchIsLiteralNotRegex(t *testing.T) {
	text := "plain line\nhas a.b literal\nhas axb regexy\n"
	s, h := retainedSession(t, text)

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"a.b"}`, h))

	if !strings.Contains(got, "has a.b literal") {
		t.Errorf("literal match missing:\n%s", got)
	}
	if strings.Contains(got, "axb") {
		t.Errorf("search behaved as a regex (matched axb via a.b):\n%s", got)
	}

	// Likewise a pattern that would be a valid regex matching everything.
	star := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":".*"}`, h))
	if !strings.Contains(star, "no lines match") {
		t.Errorf(".* was treated as a regex:\n%s", star)
	}
}

// TestReadToolResultBoundedByMaxBytes: max_bytes binds before the line
// budget does, and the output says so. Double bounding is the entire point —
// an unbounded read back into context would defeat retention.
func TestReadToolResultBoundedByMaxBytes(t *testing.T) {
	s, h := retainedSession(t, linesText(2000))

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"limit":2000,"max_bytes":300}`, h))

	if len(got) > 500 { // header + <=300 body + notice
		t.Errorf("output len = %d, want bounded near max_bytes=300:\n%s", len(got), got)
	}
	if !strings.Contains(got, "truncated at 300 bytes") {
		t.Errorf("output missing byte-truncation notice:\n%s", got)
	}
	if !strings.Contains(got, "continue with offset=") {
		t.Errorf("byte-truncated read must name the next offset:\n%s", got)
	}

	// The hard ceiling clamps an absurd request rather than honoring it.
	huge := readResult(t, s, fmt.Sprintf(`{"handle":%q,"limit":2000,"max_bytes":99999999}`, h))
	if len(huge) > readToolResultMaxMaxBytes+1024 {
		t.Errorf("output len = %d, want <= hard cap %d", len(huge), readToolResultMaxMaxBytes)
	}
}

// TestReadToolResultLimitHardCapped: an absurd limit is clamped to
// readToolResultMaxLimit rather than reinstating the whole blob.
func TestReadToolResultLimitHardCapped(t *testing.T) {
	if got := clampInt(1000000, readToolResultDefaultLimit, readToolResultMaxLimit); got != readToolResultMaxLimit {
		t.Errorf("clampInt(1000000) = %d, want %d", got, readToolResultMaxLimit)
	}
	if got := clampInt(0, readToolResultDefaultLimit, readToolResultMaxLimit); got != readToolResultDefaultLimit {
		t.Errorf("clampInt(0) = %d, want default %d", got, readToolResultDefaultLimit)
	}
	if got := clampInt(-5, readToolResultDefaultLimit, readToolResultMaxLimit); got != readToolResultDefaultLimit {
		t.Errorf("clampInt(-5) = %d, want default %d", got, readToolResultDefaultLimit)
	}

	// End to end: a huge limit against many short lines is still bounded by
	// the byte ceiling, and never returns more than the line cap.
	s, h := retainedSession(t, linesText(9000))
	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"limit":1000000,"max_bytes":%d}`, h, readToolResultMaxMaxBytes))
	if n := strings.Count(got, "line-"); n > readToolResultMaxLimit {
		t.Errorf("returned %d lines, want <= hard cap %d", n, readToolResultMaxLimit)
	}
}

// TestReadToolResultMaxMaxBytesIsPinned pins readToolResultMaxMaxBytes's
// concrete value (review finding F6(b)), the same way
// TestReadToolResultLimitHardCapped pins readToolResultMaxLimit: a bare
// relative assertion ("clamped to SOME max") would keep passing if the
// constant silently changed, which is exactly the drift this guards against
// for the documented "max 65536" contract in the tool's own description.
func TestReadToolResultMaxMaxBytesIsPinned(t *testing.T) {
	if readToolResultMaxMaxBytes != 65536 {
		t.Fatalf("readToolResultMaxMaxBytes = %d, want 65536 (the documented hard cap)", readToolResultMaxMaxBytes)
	}
	if got := clampInt(99999999, readToolResultDefaultMaxBytes, readToolResultMaxMaxBytes); got != 65536 {
		t.Errorf("clampInt(99999999) = %d, want 65536", got)
	}
}

// TestReadToolResultRejectsMaxBytesBelowFloor: review finding F11. A
// max_bytes smaller than the fixed preamble every read writes left NO room
// for any line, and the byte-budget loop reported the same "no lines at
// offset N" message a genuinely empty result gets — indistinguishable from
// the real thing. max_bytes=1 must be rejected outright with a clear error
// naming the floor, not silently misreported as an empty result.
func TestReadToolResultRejectsMaxBytesBelowFloor(t *testing.T) {
	s, h := retainedSession(t, linesText(50))

	_, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q,"max_bytes":1}`, h)))
	if err == nil {
		t.Fatal("max_bytes=1 accepted, want a clear rejection")
	}
	if !strings.Contains(err.Error(), "max_bytes") || !strings.Contains(err.Error(), "1") {
		t.Errorf("error does not name max_bytes/the floor: %v", err)
	}
	// It must NOT be the misleading "no lines" message a genuinely empty
	// read produces — this result plainly has lines.
	if strings.Contains(err.Error(), "no lines") {
		t.Errorf("rejection reused the misleading empty-result wording: %v", err)
	}

	// Just above the floor must still work normally.
	if _, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q,"max_bytes":%d}`, h, readToolResultMinMaxBytes))); err != nil {
		t.Errorf("max_bytes at the floor (%d) rejected: %v", readToolResultMinMaxBytes, err)
	}
}

// TestReadToolResultSurvivesOversizedLine is review finding F1's red test:
// a single line at or beyond readToolResultScanBuf defeats bufio.Scanner
// entirely (Scan returns false, sc.Err() is bufio.ErrTooLong). Before the
// fix, range mode reported "no lines at offset 1 (has 1 lines)" and search
// mode false-negatived on a needle that is plainly present — no max_bytes
// value recovered anything, on a result whose own preview header told the
// model it was recoverable via read_tool_result. The fix falls back to a
// raw io.ReaderAt read with no per-line limit.
func TestReadToolResultSurvivesOversizedLine(t *testing.T) {
	// 2 MiB, one line, well beyond readToolResultScanBuf (1 MiB) — with a
	// findable needle placed past the 1 MiB mark, so a fallback that only
	// looked at the first MiB would still miss it.
	needle := "UNIQUE-NEEDLE-31337"
	pad := 2*1024*1024 - len(needle) - 1
	big := strings.Repeat("x", 1200*1024) + needle + strings.Repeat("y", pad-1200*1024) + "\n"
	if len(big) < 2*1024*1024 {
		t.Fatalf("test setup: big is %d bytes, want >= 2MiB", len(big))
	}

	s, h := retainedSession(t, big)

	rangeOut := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"limit":1}`, h))
	if strings.Contains(rangeOut, "no lines at offset") {
		t.Errorf("range read over an oversized line reported no lines:\n%s", rangeOut[:min(300, len(rangeOut))])
	}
	if len(rangeOut) == 0 {
		t.Error("range read over an oversized line returned nothing")
	}

	searchOut := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":%q}`, h, needle))
	if !strings.Contains(searchOut, needle) {
		t.Errorf("search over an oversized line did not find a needle known to be present:\n%s", searchOut[:min(300, len(searchOut))])
	}
}

// TestReadToolResultUnknownHandle: a clean tool error that names the
// handles this session actually has, turning a dead end into a recoverable
// mistake.
func TestReadToolResultUnknownHandle(t *testing.T) {
	s, h := retainedSession(t, linesText(100))

	_, err := runReadToolResult(s, json.RawMessage(`{"handle":"trh_99"}`))
	if err == nil {
		t.Fatal("expected an error for an unknown handle")
	}
	if !strings.Contains(err.Error(), "trh_99") {
		t.Errorf("error does not name the requested handle: %v", err)
	}
	if !strings.Contains(err.Error(), h) {
		t.Errorf("error does not list the known handle %s: %v", h, err)
	}

	// A malformed token is rejected on shape, before any path is built.
	for _, bad := range []string{"", "bogus", "trh_", "trh_-1", "trh_x", "../../etc/passwd"} {
		if _, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, bad))); err == nil {
			t.Errorf("handle %q accepted, want rejection", bad)
		}
	}

	// A session with no handles at all says so rather than listing nothing.
	empty := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 512})
	_, err = runReadToolResult(empty, json.RawMessage(`{"handle":"trh_1"}`))
	if err == nil || !strings.Contains(err.Error(), "retained no tool results") {
		t.Errorf("empty-session error = %v", err)
	}
}

// TestReadToolResultMissingFile: the handle is known but its sidecar file is
// gone. The error must say so in fixed wording and must NOT leak the
// absolute filesystem path (the same model-visible-output leak rule
// classifyMCPConnectError follows).
func TestReadToolResultMissingFile(t *testing.T) {
	s, h := retainedSession(t, linesText(100))
	path := s.toolResultPath(h)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	_, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, h)))
	if err == nil {
		t.Fatal("expected an error for a missing sidecar file")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no longer on disk") {
		t.Errorf("error wording = %v", err)
	}
	if !strings.Contains(msg, h) {
		t.Errorf("error does not name the handle: %v", err)
	}
	if strings.Contains(msg, filepath.Dir(path)) {
		t.Errorf("error leaks the sidecar filesystem path: %v", err)
	}

	// Removing the whole directory degrades identically, not into a panic.
	if err := os.RemoveAll(s.toolResultsDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, h))); err == nil {
		t.Error("expected an error with the sidecar directory gone")
	}
}

// TestReadToolResultToolRegisteredOnlyWhenEnabled: the tool's presence
// tracks the retention gate exactly. A session that can never mint a handle
// must not advertise a tool whose only required argument is one.
func TestReadToolResultToolRegisteredOnlyWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		cfg   Config
		wantT bool
	}{
		{"enabled", Config{SessionDir: dir, ToolResultInlineBytes: 1024}, true},
		{"zero limit", Config{SessionDir: dir, ToolResultInlineBytes: 0}, false},
		{"negative limit", Config{SessionDir: dir, ToolResultInlineBytes: -1}, false},
		{"no session dir", Config{ToolResultInlineBytes: 1024}, false},
		{"bare config", Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSession(tc.cfg)
			_, ok := s.tools[readToolResultToolName]
			if ok != tc.wantT {
				t.Errorf("read_tool_result registered = %v, want %v", ok, tc.wantT)
			}
		})
	}
}

// TestReadToolResultOffsetPastEnd: an offset past the end is a clean,
// informative result rather than an error or an empty body.
func TestReadToolResultOffsetPastEnd(t *testing.T) {
	s, h := retainedSession(t, linesText(10))
	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":500}`, h))
	if !strings.Contains(got, "no lines at offset 500") {
		t.Errorf("output = %q", got)
	}
}

// TestReadToolResultFinalWindowHasNoContinuationNotice: a read that reaches
// the end of the retained data must not claim there is more.
func TestReadToolResultFinalWindowHasNoContinuationNotice(t *testing.T) {
	s, h := retainedSession(t, linesText(10))
	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"limit":50}`, h))
	if !strings.Contains(got, "line-10") {
		t.Fatalf("read did not reach the last line:\n%s", got)
	}
	if strings.Contains(got, "more line(s)") || strings.Contains(got, "truncated") {
		t.Errorf("complete read claimed more data remained:\n%s", got)
	}
}

// TestToolResultRetentionSurvivesVeryLongSingleLine: a retained result that
// is one enormous line (minified JSON, a base64 dump) must still be
// readable — the default bufio.Scanner buffer would fail the whole read on
// it, which is why readToolResultScanBuf exists.
func TestToolResultRetentionSurvivesVeryLongSingleLine(t *testing.T) {
	long := strings.Repeat("x", 300*1024) + "\n"
	s, h := retainedSession(t, long)
	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"max_bytes":1024}`, h))
	if !strings.Contains(got, "truncated") {
		t.Errorf("expected a byte-truncation notice for a huge single line:\n%s", got[:min(300, len(got))])
	}
}

// TestTruncateUTF8NeverSplitsRune: the preview is a byte budget, but a cut
// mid-rune would put invalid UTF-8 into the canonical message and from there
// into a provider request.
func TestTruncateUTF8NeverSplitsRune(t *testing.T) {
	// "é" is 2 bytes; cutting at an odd offset lands mid-rune.
	s := strings.Repeat("é", 10)
	for n := 0; n <= len(s); n++ {
		got := truncateUTF8(s, n)
		if len(got) > n {
			t.Fatalf("truncateUTF8(%d) returned %d bytes", n, len(got))
		}
		if !utf8Valid(got) {
			t.Fatalf("truncateUTF8(%d) = %q is not valid UTF-8", n, got)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' && !strings.Contains(s, "\uFFFD") {
			return false
		}
	}
	// A byte-level check: every rune must round-trip.
	return strings.ToValidUTF8(s, "\x00") == s
}

// TestCountLines pins the line-count convention the preview header reports
// and read_tool_result's continuation notice arithmetic depends on.
func TestCountLines(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
		{"\n\n", 2},
	}
	for _, tc := range cases {
		if got := countLines(tc.in); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestParseToolResultHandle pins handle-shape validation, which is what
// keeps a path-traversal-shaped argument from ever reaching a filesystem
// path and what makes LoadSession's malformed-record skip decidable.
func TestParseToolResultHandle(t *testing.T) {
	good := map[string]int64{"trh_1": 1, "trh_42": 42, "trh_1000000": 1000000}
	for in, want := range good {
		n, ok := parseToolResultHandle(in)
		if !ok || n != want {
			t.Errorf("parseToolResultHandle(%q) = %d,%v; want %d,true", in, n, ok, want)
		}
	}
	for _, in := range []string{
		"", "trh_", "trh_0", "trh_-1", "trh_x", "trh_1x", "bogus", "1", "../trh_1", "trh_1/../..",
		// F13: digits-only, no leading zero, no sign — strconv.ParseInt
		// alone accepts these as alternate spellings of trh_1, but
		// writeRetainedToolResult (strconv.FormatInt) never produces
		// either, so they must not parse as aliases of the canonical
		// handle.
		"trh_+1", "trh_01", "trh_007", "trh_+42",
	} {
		if _, ok := parseToolResultHandle(in); ok {
			t.Errorf("parseToolResultHandle(%q) accepted, want rejected", in)
		}
	}
}

// TestKnownToolResultHandlesBounded: the unknown-handle error's handle list
// is bounded and newest-last, so a long session's error message never
// becomes its own context problem.
func TestKnownToolResultHandlesBounded(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir(), ToolResultInlineBytes: 512})
	for i := 0; i < 30; i++ {
		if _, err := s.writeRetainedToolResult("bash", fmt.Sprintf("body-%d\n", i)); err != nil {
			t.Fatal(err)
		}
	}
	got := s.knownToolResultHandles(5)
	want := []string{"trh_26", "trh_27", "trh_28", "trh_29", "trh_30"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("knownToolResultHandles(5) = %v, want %v", got, want)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = context.Background
