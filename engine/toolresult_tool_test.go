package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// TestReadToolResultPartialFirstLineOffsetIsRecoverable is a round-3 review
// finding's red test. When the very FIRST shown line alone exceeds
// max_bytes, only a truncated prefix is emitted (shown=1) — but the
// original notice reported "continue with offset=offset+1", silently
// skipping past the UNSHOWN REMAINDER of that same line: it can never be
// retrieved through range mode at any max_bytes, because every
// continuation jumps straight to the NEXT line.
//
// The fix keeps the continuation offset UNCHANGED in this specific case,
// so a caller that re-reads at the same offset with a larger max_bytes
// makes real progress into the same line (each read re-scans from byte
// zero, so a bigger budget naturally reaches further before truncating
// again) — genuinely recoverable, not just an honest dead end.
func TestReadToolResultPartialFirstLineOffsetIsRecoverable(t *testing.T) {
	longLine := strings.Repeat("Z", 300)
	text := longLine + "\nshort-line-2\nshort-line-3\n"
	s, h := retainedSession(t, text)

	small := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"max_bytes":%d}`, h, readToolResultMinMaxBytes))
	if strings.Count(small, "Z") >= len(longLine) {
		t.Fatalf("test setup: expected byte truncation on the long first line (want a partial Z run):\n%s", small)
	}
	if strings.Contains(small, "continue with offset=2") {
		t.Errorf("notice advances past the truncated line's own unshown remainder to line 2, abandoning it permanently:\n%s", small)
	}

	// Re-reading at the SAME offset with a bigger budget must show MORE of
	// line 1 than the first attempt did — proof the remainder is actually
	// reachable, not just honestly described as lost.
	bigger := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"max_bytes":%d}`, h, readToolResultDefaultMaxBytes))
	if strings.Count(bigger, "Z") <= strings.Count(small, "Z") {
		t.Errorf("re-reading offset=1 with a bigger max_bytes did not recover more of the long line: small has %d Z's, bigger has %d",
			strings.Count(small, "Z"), strings.Count(bigger, "Z"))
	}
	// With ample budget, the full line plus the next lines are reachable.
	full := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"max_bytes":%d}`, h, readToolResultMaxMaxBytes))
	if !strings.Contains(full, longLine) {
		t.Errorf("with the max max_bytes, the full long line should be recoverable at offset=1:\n%s", full[:min(400, len(full))])
	}
	if !strings.Contains(full, "short-line-2") {
		t.Errorf("with the max max_bytes, subsequent lines should also be reachable:\n%s", full[:min(400, len(full))])
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

// TestReadToolResultSearchCountCapNoticeDoesNotSuggestMaxBytes is a
// round-3 review finding's red test. Hitting the MATCH-COUNT cap
// (readToolResultMaxLimit, 2000) set the same byteTrunc flag a byte-budget
// stop uses, so the notice always said "...narrow the search or increase
// max_bytes to see more" — but raising max_bytes cannot surface a single
// additional match once the count cap fired; the search simply stopped
// counting. That half of the advice actively misdirects the model's
// recovery attempt. This constructs 2500 short matching lines with a
// generous max_bytes (well above what 2000 short entries need), so the
// COUNT cap — not the byte budget — is what actually stops the search.
func TestReadToolResultSearchCountCapNoticeDoesNotSuggestMaxBytes(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&b, "hit line %d\n", i)
	}
	s, h := retainedSession(t, b.String())

	got := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"hit","max_bytes":%d}`, h, readToolResultMaxMaxBytes))
	if n := strings.Count(got, "hit line"); n != readToolResultMaxLimit {
		t.Fatalf("test setup: matched %d lines, want exactly the count cap %d (so the byte budget provably didn't bind first)", n, readToolResultMaxLimit)
	}
	if strings.Contains(got, "increase max_bytes") {
		t.Errorf("count-capped search notice suggests increasing max_bytes, which cannot surface more matches:\n%s", got[len(got)-min(200, len(got)):])
	}
	if !strings.Contains(got, fmt.Sprintf("%d match", readToolResultMaxLimit)) {
		t.Errorf("notice does not name the count cap that actually stopped the search:\n%s", got[len(got)-min(200, len(got)):])
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

// TestReadToolResultFloorAccountsForLongToolName is a round-5 review
// finding's red test. The flat 256-byte floor did not account for the
// ACTUAL preamble a request would produce — for a long MCP-shaped tool
// name plus large byte/line counts, the preamble alone can exceed
// bodyMax (floor - the 128-byte notice reserve), reproducing exactly the
// F11 false-empty class the floor exists to prevent, at a max_bytes value
// (256) the tool had just accepted as valid.
func TestReadToolResultFloorAccountsForLongToolName(t *testing.T) {
	dir := t.TempDir()
	longTool := "mcp__some_moderately_long_server_name_here__a_fairly_long_tool_name_here_too_yes_indeed"
	s := NewSession(Config{
		Providers:             provider.Registry{"test": &scriptedProvider{name: "test"}},
		Model:                 message.ModelRef{Provider: "test", Model: "m1"},
		SessionDir:            dir,
		ToolResultInlineBytes: 512,
	})
	// Large byte/line counts, matching the reviewer's "large byte/line/
	// offset/limit numbers" half of the scenario.
	handle, err := s.writeRetainedToolResult(longTool, strings.Repeat("line-x\n", 123456))
	if err != nil {
		t.Fatal(err)
	}

	// max_bytes=256 (the flat floor) is accepted by the earlier gate — the
	// point of this test is what happens AFTER that gate, not the gate
	// itself.
	out, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q,"max_bytes":%d}`, handle, readToolResultMinMaxBytes)))
	if err != nil {
		// Also acceptable: the floor computation rejects this request
		// outright (a real error naming the floor), rather than silently
		// returning a misleading empty result. Either is fine as long as
		// it's not the false-empty case checked below.
		return
	}
	got := out.Text()
	if strings.Contains(got, "no lines at offset") {
		t.Errorf("a long tool name pushed the preamble past bodyMax, reproducing the F11 false-empty class at the accepted floor:\n%s", got)
	}
}

// TestReadToolResultSurvivesOversizedLine is review finding F1's red test:
// a single line at or beyond readToolResultScanBuf defeats bufio.Scanner
// entirely (Scan returns false, sc.Err() is bufio.ErrTooLong). Before the
// fix, range mode reported "no lines at offset 1 (has 1 lines)" and search
// mode false-negatived on a needle that is plainly present — no max_bytes
// value recovered anything, on a result whose own preview header told the
// model it was recoverable via read_tool_result. The fix falls back to a
// raw io.ReaderAt-based line source with no per-line limit.
//
// The needle sits ~1.2 MiB into the 2 MiB line — deliberately far past both
// readToolResultScanBuf (1 MiB) AND the default max_bytes (16384): a
// rescue that only ever showed a prefix from byte 0 of the line (the
// original round-2 shape review finding N1 caught) could never reach it,
// only a window ANCHORED AT THE MATCH can.
//
// The assertion is on the BODY only — everything after the first "\n" —
// never the whole output (review finding N1's second half): the header
// line itself echoes the needle back via `lines matching %q:`, so
// asserting against the whole string is tautological and passes even if
// matching is completely broken. TestReadToolResultSearchNeverMatchMutant
// exercises that escape directly by forcing strings.Contains to report no
// match, proving THIS test would catch it.
func TestReadToolResultSurvivesOversizedLine(t *testing.T) {
	// 2 MiB, one line, well beyond readToolResultScanBuf (1 MiB).
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
	body := searchBodyAfterHeader(t, searchOut)
	if strings.Contains(body, "0 match(es)") {
		t.Errorf("search over an oversized line false-negatived (0 matches) on a needle known to be present:\n%s", searchOut[:min(300, len(searchOut))])
	}
	if !strings.Contains(body, needle) {
		t.Errorf("search body (after the header line) does not contain the needle:\n%s", body[:min(300, len(body))])
	}
}

// searchBodyAfterHeader strips readToolResultSearch's first line (the
// header, which echoes the search needle back via `lines matching %q:` and
// so must never be used as evidence a match was actually found — see
// TestReadToolResultSurvivesOversizedLine's doc comment, review finding
// N1). The zero-match case is a single line with no header at all (it
// never got as far as writing one) — treated as its own body, verbatim.
func searchBodyAfterHeader(t *testing.T, out string) string {
	t.Helper()
	i := strings.IndexByte(out, '\n')
	if i < 0 {
		return out
	}
	return out[i+1:]
}

// TestReadToolResultSearchNeverMatchMutant is review finding N1's mutation-
// verification: it forces strings.Contains to report NO match ever (a
// literal implementation of "search is completely broken"), and confirms
// TestReadToolResultSurvivesOversizedLine's body-only assertion catches it
// — proving that assertion is not the tautology the original
// whole-output-contains-the-needle check was (the header alone would have
// kept that version green even here).
func TestReadToolResultSearchNeverMatchMutant(t *testing.T) {
	orig := stringsContainsForSearch
	stringsContainsForSearch = func(s, substr string) bool { return false }
	defer func() { stringsContainsForSearch = orig }()

	s, h := retainedSession(t, linesText(10)+"UNIQUE-MUTANT-NEEDLE\n")
	out := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"UNIQUE-MUTANT-NEEDLE"}`, h))
	// With matching forced off, the ENTIRE output collapses to the
	// single-line "no lines match" message — there is no separate header
	// line at all (readToolResultSearch never gets far enough to write
	// one; see runReadToolResult/readToolResultSearch). Note that message
	// necessarily echoes the needle back too (confirming what was
	// searched), so this deliberately does NOT assert "the needle is
	// absent" — that would be exactly the same tautology this test exists
	// to rule out on the OTHER test. It asserts the shape: no
	// match-with-content, no header line — is produced.
	if !strings.Contains(out, "no lines match") {
		t.Fatalf("mutant did not produce the expected no-match output: %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("mutant output has extra structure (a header/body split) it should not have: %q", out)
	}
}

// countingReaderAt wraps an io.ReaderAt and records the total bytes
// requested via ReadAt, so a test can assert how much of a file a reader
// actually touched.
type countingReaderAt struct {
	r     io.ReaderAt
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.bytes += int64(n)
	return n, err
}

// TestReaderAtLineSourceStreamsRatherThanBuffersWholeFile is review finding
// N7's red test: the FIRST cut of the F1 fallback (toolResultFallbackLines)
// allocated make([]byte, meta.Bytes) and read the file via ONE ReadAt call
// — the ENTIRE file into memory — even to satisfy a request for just the
// first line. This asserts the streaming replacement
// (readerAtLineSource): reading only the first (short) line of an 8 MiB
// file touches only a small, bounded number of bytes, nowhere near the
// file's full size.
func TestReaderAtLineSourceStreamsRatherThanBuffersWholeFile(t *testing.T) {
	size := 8 * 1024 * 1024
	content := "first line\n" + strings.Repeat("x", size-len("first line\n"))

	f := &countingReaderAt{r: strings.NewReader(content)}
	src := newReaderAtLineSource(f, int64(len(content)))

	if !src.Scan() {
		t.Fatalf("Scan() = false, want true: %v", src.Err())
	}
	if got := src.Text(); got != "first line" {
		t.Fatalf("Text() = %q, want %q", got, "first line")
	}

	// Reading exactly one short line must not have pulled anywhere near
	// the whole 8 MiB file through ReadAt.
	if f.bytes > 4*readerAtLineSourceChunk {
		t.Errorf("reading one line touched %d bytes of an %d-byte file — want bounded to a few chunks (%d bytes each), not the whole file",
			f.bytes, size, readerAtLineSourceChunk)
	}
}

// TestReaderAtLineSourceReadsWholeContentWhenScannedToCompletion is the
// counterweight: when a caller DOES scan every line (e.g. a full range
// read with a limit that covers the file, or a search that never finds its
// needle), readerAtLineSource must still return the FULL, correct content
// — the streaming rewrite must not have traded correctness for boundedness.
func TestReaderAtLineSourceReadsWholeContentWhenScannedToCompletion(t *testing.T) {
	var want []string
	for i := 1; i <= 500; i++ {
		want = append(want, fmt.Sprintf("line-%d", i))
	}
	content := strings.Join(want, "\n") + "\n"

	src := newReaderAtLineSource(strings.NewReader(content), int64(len(content)))
	var got []string
	for src.Scan() {
		got = append(got, src.Text())
	}
	if err := src.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d lines, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReaderAtLineSourceTerminatesWhenFileShorterThanClaimedSize is a
// round-3 review finding's red test: readerAtLineSource busy-loops FOREVER
// (100% CPU, wedging the run slot indefinitely) when the underlying file is
// SHORTER than the size it was constructed with (meta.Bytes) — a volume
// rollback, or an operator's partial wipe, the exact class of mismatch the
// surrounding errToolResultFileMissing handling already anticipates
// elsewhere in this package.
//
// Mechanism: once the real data is exhausted, ReadAt returns (0, io.EOF).
// l.off never advances (n==0), so l.off >= l.size (the FALSE, inflated
// claim) never becomes true, and io.EOF is excluded from the error check —
// so l.err never gets set either. Scan spins IndexByte -> ReadAt -> (0,
// io.EOF) forever.
//
// Run with a watchdog: this test must never rely on Scan() returning on its
// own if the bug is present — a goroutine plus a bounded select is the only
// way to observe "it never returns" without hanging the test run itself.
func TestReaderAtLineSourceTerminatesWhenFileShorterThanClaimedSize(t *testing.T) {
	content := "short content, no trailing newline, no newline anywhere"
	// Claim far more bytes exist than actually do — simulating meta.Bytes
	// (from a durable record) outliving a truncated/rolled-back sidecar file.
	claimedSize := int64(len(content)) + 1_000_000

	src := newReaderAtLineSource(strings.NewReader(content), claimedSize)

	done := make(chan bool, 1)
	go func() {
		done <- src.Scan()
	}()
	if !awaitSignal(t, done, "Scan() did not return — busy-loop: the file is shorter than the claimed size, and the loop cannot detect true EOF") {
		t.Fatalf("Scan() = false, Err() = %v; want true (the short real content should still surface as a final line)", src.Err())
	}
	if got := src.Text(); got != content {
		t.Errorf("Text() = %q, want %q", got, content)
	}
	// A second Scan call, past the real EOF, must also terminate promptly
	// (not resume spinning).
	done2 := make(chan bool, 1)
	go func() { done2 <- src.Scan() }()
	if awaitSignal(t, done2, "second Scan() did not return — busy-loop on the tail call") {
		t.Errorf("second Scan() = true, want false (no more real content)")
	}
}

// TestReadToolResultOutputNeverExceedsMaxBytes is review finding N10's red
// test. The trailing continuation/truncation notice was appended AFTER the
// per-line/per-match budget loop decided how much body fit against the
// FULL max_bytes — so body+notice together could exceed max_bytes by
// however long the notice text was (~50-80 bytes measured). Checked across
// both modes and a range of max_bytes values, including right at the
// floor, where the notice reserve is proportionally largest.
func TestReadToolResultOutputNeverExceedsMaxBytes(t *testing.T) {
	big := linesText(20000) // long enough to force truncation at every max_bytes tried below
	s, h := retainedSession(t, big)

	// The floor is now computed PER REQUEST from the actual preamble
	// (review finding, round 5) — for this fixture's large byte/line
	// counts, that floor sits a little above the flat
	// readToolResultMinMaxBytes constant. Start the sweep at the real
	// computed floor (search's is the larger of the two modes here) rather
	// than the flat constant, so this test's smallest value is always
	// accepted rather than legitimately rejected.
	meta, ok := s.lookupToolResult(h)
	if !ok {
		t.Fatal("handle not registered")
	}
	nearFloor := readToolResultFloor(meta, readToolResultArgs{Search: "line-"})
	if rangeFloor := readToolResultFloor(meta, readToolResultArgs{Offset: 1, Limit: 100000}); rangeFloor > nearFloor {
		nearFloor = rangeFloor
	}

	for _, mb := range []int{nearFloor, 300, 500, 1000, 4096, 16384} {
		t.Run(fmt.Sprintf("range/max_bytes=%d", mb), func(t *testing.T) {
			out := readResult(t, s, fmt.Sprintf(`{"handle":%q,"offset":1,"limit":100000,"max_bytes":%d}`, h, mb))
			if len(out) > mb {
				t.Errorf("range output len = %d, want <= max_bytes=%d:\n%s", len(out), mb, out)
			}
		})
		t.Run(fmt.Sprintf("search/max_bytes=%d", mb), func(t *testing.T) {
			// "line-" matches every line, guaranteeing byte-budget truncation
			// (never a clean "matches exhausted" finish) at every size tried.
			out := readResult(t, s, fmt.Sprintf(`{"handle":%q,"search":"line-","max_bytes":%d}`, h, mb))
			if len(out) > mb {
				t.Errorf("search output len = %d, want <= max_bytes=%d:\n%s", len(out), mb, out)
			}
		})
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

// TestReadToolResultNonMissingErrorIsNotReportedAsGone is a round-3 review
// finding's red test. openRetainedToolResult mapped EVERY os.Open error —
// not just os.ErrNotExist — to errToolResultFileMissing, so a transient
// condition (permission denied, EMFILE descriptor exhaustion, a flaky I/O
// error) got the SAME terminal "no longer on disk ... it cannot be read
// back" wording as a genuinely deleted file, steering the model away from
// retrying a read that would succeed once the transient condition clears.
// Only a true not-exist should get that permanent wording; anything else
// should fall through to the generic "cannot read handle" error, which
// carries no false claim of permanence.
func TestReadToolResultNonMissingErrorIsNotReportedAsGone(t *testing.T) {
	s, h := retainedSession(t, linesText(10))

	orig := openFileForToolResult
	simulated := errors.New("permission denied (simulated for this test)")
	openFileForToolResult = func(name string) (*os.File, error) { return nil, simulated }
	defer func() { openFileForToolResult = orig }()

	_, err := runReadToolResult(s, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, h)))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "no longer on disk") {
		t.Errorf("a non-missing-file error was reported with the terminal 'gone' wording: %v", err)
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
	// A single line this size is exactly the partial-first-line case (round
	// 3): the notice names the recovery path (increase max_bytes, re-read
	// at the same offset) rather than the generic "truncated ... continue
	// with offset=N+1" wording, which would silently abandon this line's
	// unshown remainder — see TestReadToolResultPartialFirstLineOffsetIsRecoverable.
	if !strings.Contains(got, "exceeds max_bytes") || !strings.Contains(got, "same offset=1") {
		t.Errorf("expected the partial-first-line recovery notice for a huge single line:\n%s", got[:min(300, len(got))])
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
