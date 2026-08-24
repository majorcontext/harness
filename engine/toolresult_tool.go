// The `read_tool_result` session tool: bounded reads back into context of a
// tool result that retention moved out of context (see toolresult.go and
// docs/plans/2026-08-19-tool-result-handles.md §6).
//
// Two read modes, one shape each:
//
//   - range: offset/limit, a 1-based line window.
//   - search: a LITERAL substring, returning matching lines with their
//     1-based line numbers. Deliberately not a regex — a model-authored
//     regex against an unbounded file is a ReDoS surface with no upside,
//     and literal search is what reading back a captured log actually
//     needs.
//
// Every read is bounded TWICE — by a line budget and by a byte budget,
// whichever binds first, with an explicit notice when either does. That is
// the entire point: an unbounded read back into context would defeat
// retention, turning a mechanism that keeps bytes out of the context window
// into a two-step way of putting them back.
//
// Registered in newSession only when retention itself is enabled (a
// positive Config.ToolResultInlineBytes AND a SessionDir), exactly like the
// `process`/`goal`/`mcp` tools gate on their own preconditions: a session
// that can never mint a handle must not advertise a tool whose only
// argument is one.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// readToolResultToolName is the built-in tool's fixed name.
const readToolResultToolName = "read_tool_result"

const (
	// readToolResultDefaultLimit / readToolResultMaxLimit bound the line
	// window. The hard cap is what makes an absurd model-supplied limit
	// (limit: 1000000) safe rather than a way to reinstate the whole blob.
	readToolResultDefaultLimit = 200
	readToolResultMaxLimit     = 2000

	// readToolResultDefaultMaxBytes / readToolResultMaxMaxBytes bound the
	// output in bytes. The default matches the retention inline default
	// (16 KiB) so one read costs about what the preview already cost; the
	// hard cap is 64 KiB, still well under bash's own 96 KiB output cap.
	readToolResultDefaultMaxBytes = 16384
	readToolResultMaxMaxBytes     = 65536

	// readToolResultScanBuf bounds one line. A retained result can contain
	// a single enormous line (minified JSON, a base64 dump); the default
	// bufio.Scanner buffer would fail the whole read on it. Long lines are
	// truncated per line instead, which keeps a pathological input
	// readable rather than unreadable.
	readToolResultScanBuf = 1024 * 1024

	// readToolResultKnownHandleList bounds how many known handles an
	// unknown-handle error lists (see knownToolResultHandles).
	readToolResultKnownHandleList = 20

	// readToolResultMinMaxBytes is the ABSOLUTE floor on a caller-supplied
	// max_bytes — never below this regardless of request shape. Below it,
	// the fixed preamble every read writes (the "handle (tool=..., N
	// bytes, N lines) ..." line, plus a truncation/continuation notice)
	// can itself exceed the budget, leaving zero or negative room for any
	// line — the byte-budget loop in readToolResultRange then reports "no
	// lines at offset N" even though the result has plenty, because nothing
	// EVER fit, not because nothing was there (review finding F11). Rather
	// than silently produce that misleading message, a request below the
	// floor is rejected outright with an error naming the floor.
	//
	// This constant alone is NOT sufficient (review finding, round 5): the
	// actual preamble grows with the handle, the TOOL NAME (unbounded —
	// an MCP tool name can be long), and the byte/line/offset/limit
	// counts, so a sufficiently long tool name could exceed this floor's
	// own body budget and reproduce the exact false-empty class it exists
	// to prevent, silently, at a max_bytes value the gate had just
	// accepted. See readToolResultFloor, which computes the REAL floor for
	// one specific request from its ACTUAL preamble rather than guessing —
	// this constant is only its lower bound for the common case (a short
	// handle and tool name).
	readToolResultMinMaxBytes = 256

	// readToolResultNoticeReserve is subtracted from maxBytes before the
	// per-line/per-match budget loop runs, in both range and search modes
	// (review finding N10). The trailing notice ("[truncated at N bytes;
	// continue with offset=N]", "[truncated at N bytes after N match(es);
	// ...]") is built and appended AFTER that loop decides how much body
	// content fit — if the loop budgeted the body against the FULL
	// maxBytes, appending the notice on top could push the total past
	// maxBytes by however long the notice is (~50-80 bytes measured). This
	// constant is a generous upper bound on any notice this file emits, so
	// reserving it up front guarantees body+notice together never exceed
	// the caller's maxBytes, whether or not a notice ends up being needed.
	readToolResultNoticeReserve = 128

	// readToolResultMinBodyRoom is the minimum body space a request must
	// leave AFTER its own preamble and the notice reserve — see
	// readToolResultFloor. A generous constant, not a tight one: its job
	// is to guarantee genuine room for actual content, not merely a
	// non-negative number.
	readToolResultMinBodyRoom = 64
)

// readToolResultArgs is the tool's input shape.
type readToolResultArgs struct {
	Handle   string `json:"handle"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	Search   string `json:"search"`
	MaxBytes int    `json:"max_bytes"`
}

// readToolResultTool builds the `read_tool_result` session tool.
func readToolResultTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: readToolResultToolName,
			Description: "Read back a large tool result that was retained out of context. " +
				"When a tool produces more output than fits inline, its result is replaced by a " +
				"short preview carrying a handle (trh_N); the full output is kept on disk and read " +
				"back through this tool. " +
				"Range mode: offset (1-based first line, default 1) and limit (lines, default 200, " +
				"max 2000). Search mode: search is a LITERAL substring (not a regex) — when set, " +
				"offset/limit are ignored and matching lines are returned with their line numbers. " +
				"Output is always bounded by max_bytes (default 16384, max 65536) and says so when " +
				"truncated, so read in windows rather than trying to pull the whole result back.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"handle": {"type": "string", "description": "The trh_N handle from a retained tool result's preview header"},
					"offset": {"type": "integer", "description": "1-based first line to return (range mode; default 1)"},
					"limit": {"type": "integer", "description": "Maximum lines to return (range mode; default 200, max 2000)"},
					"search": {"type": "string", "description": "Literal substring to find; returns matching lines with line numbers. Not a regex."},
					"max_bytes": {"type": "integer", "description": "Maximum output bytes (default 16384, max 65536)"}
				},
				"required": ["handle"]
			}`),
		},
		Run: func(_ context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runReadToolResult(s, args)
		},
	}
}

// runReadToolResult dispatches one read_tool_result call against s.
func runReadToolResult(s *Session, raw json.RawMessage) (message.Parts, error) {
	var in readToolResultArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s: invalid arguments: %w", readToolResultToolName, err)
	}
	if in.Handle == "" {
		return nil, fmt.Errorf("%s: a %q argument is required", readToolResultToolName, "handle")
	}
	// Validate the token's SHAPE before any filesystem path is built from
	// it: same defense-in-depth posture LoadSession takes with
	// ValidSessionID, so a path-traversal-shaped handle is rejected without
	// touching disk.
	if _, ok := parseToolResultHandle(in.Handle); !ok {
		return nil, fmt.Errorf("%s: malformed handle %q (want %sN, e.g. %s1)",
			readToolResultToolName, in.Handle, toolResultHandlePrefix, toolResultHandlePrefix)
	}

	meta, ok := s.lookupToolResult(in.Handle)
	if !ok {
		known := s.knownToolResultHandles(readToolResultKnownHandleList)
		if len(known) == 0 {
			return nil, fmt.Errorf("%s: unknown handle %q (this session has retained no tool results)",
				readToolResultToolName, in.Handle)
		}
		return nil, fmt.Errorf("%s: unknown handle %q (this session's handles: %s)",
			readToolResultToolName, in.Handle, strings.Join(known, ", "))
	}

	// A budget below the floor can be entirely consumed by the fixed
	// preamble every read writes, before a single line of actual content —
	// see readToolResultMinMaxBytes's doc comment (review finding F11) and
	// readToolResultFloor's (round 5: the flat constant alone doesn't
	// account for a long tool name or large byte/line counts inflating
	// THIS request's actual preamble past it).
	if floor := readToolResultFloor(meta, in); in.MaxBytes > 0 && in.MaxBytes < floor {
		return nil, fmt.Errorf("%s: max_bytes %d is below the minimum %d for this result (handle=%s tool=%q)",
			readToolResultToolName, in.MaxBytes, floor, meta.Handle, meta.Tool)
	}

	f, err := s.openRetainedToolResult(in.Handle)
	if err != nil {
		if errors.Is(err, errToolResultFileMissing) {
			// Fixed wording, never the raw *os.PathError: that error
			// carries the absolute sidecar path, and this string becomes
			// model-visible tool output (the same leak rule
			// classifyMCPConnectError follows).
			return nil, fmt.Errorf("%s: handle %q is known but its retained data is no longer on disk (%d bytes from tool %q); it cannot be read back",
				readToolResultToolName, in.Handle, meta.Bytes, meta.Tool)
		}
		return nil, fmt.Errorf("%s: cannot read handle %q", readToolResultToolName, in.Handle)
	}
	defer f.Close()

	maxBytes := clampInt(in.MaxBytes, readToolResultDefaultMaxBytes, readToolResultMaxMaxBytes)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), readToolResultScanBuf)

	scan := func(src toolResultLineSource) (message.Parts, error) {
		if in.Search != "" {
			return readToolResultSearch(src, meta, in.Search, maxBytes)
		}
		return readToolResultRange(src, meta, in.Offset, in.Limit, maxBytes)
	}

	parts, err := scan(sc)
	if err == nil && errors.Is(sc.Err(), bufio.ErrTooLong) {
		// A single line at or beyond readToolResultScanBuf defeats
		// bufio.Scanner outright (review finding F1): Scan returns false,
		// sc.Err() is bufio.ErrTooLong, and — because this was never
		// checked before — the caller saw a plain "no lines"/"no match"
		// result indistinguishable from a genuinely empty read, on a
		// result the preview header told the model was recoverable. Retry
		// once against readerAtLineSource, a raw io.ReaderAt-based line
		// source with no per-line size limit (review finding N7: the
		// FIRST cut of this fallback read the entire file into memory
		// upfront via make([]byte, meta.Bytes) even for a tiny range
		// request — readerAtLineSource streams in fixed-size chunks
		// instead, so a small request against a huge oversized-line file
		// only ever reads as far as the caller actually scans).
		parts, err = scan(newReaderAtLineSource(f, int64(meta.Bytes)))
	}
	return parts, err
}

// readToolResultFloor computes the REAL minimum max_bytes for ONE specific
// request — not a guess (review finding, round 5). It builds the EXACT
// preamble this request's mode (range or search) will produce, from the
// real meta and real request fields, and floors it at
// readToolResultMinMaxBytes so the common case (a short handle and tool
// name) never rises above the familiar 256.
//
// This mirrors readToolResultRange's and readToolResultSearch's own
// preamble-building fmt.Sprintf calls exactly — offset/limit are clamped
// the identical way — so the computed floor is precise, not an
// approximation that could itself be wrong in either direction.
func readToolResultFloor(meta toolResultMeta, in readToolResultArgs) int {
	var preamble string
	if in.Search != "" {
		preamble = fmt.Sprintf("%s (tool=%s, %d bytes, %d lines) lines matching %q:\n",
			meta.Handle, meta.Tool, meta.Bytes, meta.Lines, in.Search)
	} else {
		offset := in.Offset
		if offset <= 0 {
			offset = 1
		}
		limit := clampInt(in.Limit, readToolResultDefaultLimit, readToolResultMaxLimit)
		preamble = fmt.Sprintf("%s (tool=%s, %d bytes, %d lines) lines %d-%d:\n",
			meta.Handle, meta.Tool, meta.Bytes, meta.Lines, offset, offset+limit-1)
	}
	floor := len(preamble) + readToolResultNoticeReserve + readToolResultMinBodyRoom
	if floor < readToolResultMinMaxBytes {
		floor = readToolResultMinMaxBytes
	}
	return floor
}

// toolResultLineSource is the line-by-line interface readToolResultRange and
// readToolResultSearch scan through. *bufio.Scanner satisfies it directly
// (its Scan/Text/Err methods match exactly); sliceLineSource is the F1
// fallback source, built from a raw io.ReaderAt read with no per-line size
// limit.
type toolResultLineSource interface {
	Scan() bool
	Text() string
	Err() error
}

// readerAtLineSourceChunk is how many bytes readerAtLineSource reads from
// the underlying file per ReadAt call, growing its internal buffer only
// that far ahead of what the caller has actually consumed.
const readerAtLineSourceChunk = 64 * 1024

// readerAtLineSource is F1's raw-byte fallback line source, streamed via
// io.ReaderAt in fixed-size chunks rather than loaded into memory all at
// once (review finding N7: the first cut of this fallback allocated
// make([]byte, meta.Bytes) — the WHOLE file — even to satisfy a 256-byte
// range request). It has no per-line size limit at all, unlike
// bufio.Scanner: that limit is exactly what defeated the scanner in the
// first place (bufio.ErrTooLong). io.ReaderAt specifically, not Read/Seek:
// an absolute-offset read is independent of wherever the *bufio.Scanner
// that failed already left the same *os.File's read cursor.
//
// Once the caller's range/search loop breaks (limit reached, byte budget
// hit, match found), Scan is simply never called again — bytes past that
// point are never read from disk at all.
type readerAtLineSource struct {
	r    io.ReaderAt
	size int64 // total bytes CLAIMED to exist (meta.Bytes) — may exceed the real file; see Scan's zero-byte-EOF handling
	off  int64 // next unread byte offset in the underlying file
	buf  []byte
	line string
	err  error
}

func newReaderAtLineSource(r io.ReaderAt, size int64) *readerAtLineSource {
	return &readerAtLineSource{r: r, size: size}
}

// Scan advances to the next line, growing the internal buffer by one chunk
// at a time only when the buffer held so far has no line to give.
func (l *readerAtLineSource) Scan() bool {
	if l.err != nil {
		return false
	}
	for {
		if i := bytes.IndexByte(l.buf, '\n'); i >= 0 {
			l.line = string(l.buf[:i])
			l.buf = l.buf[i+1:]
			return true
		}
		if l.off >= l.size {
			// Nothing left to read from disk. Whatever remains in buf (if
			// anything) is a final, unterminated line.
			if len(l.buf) == 0 {
				return false
			}
			l.line = string(l.buf)
			l.buf = nil
			l.off = l.size + 1 // ensure the next call returns false, not this same tail again
			return true
		}
		want := readerAtLineSourceChunk
		if remaining := l.size - l.off; remaining < int64(want) {
			want = int(remaining)
		}
		chunk := make([]byte, want)
		n, err := l.r.ReadAt(chunk, l.off)
		l.off += int64(n)
		l.buf = append(l.buf, chunk[:n]...)
		if errors.Is(err, io.EOF) && n == 0 {
			// The real file is SHORTER than l.size (meta.Bytes) claims — a
			// volume rollback, or an operator's partial wipe of
			// toolresults/, the same mismatch class errToolResultFileMissing
			// already anticipates elsewhere. Without this, l.off never
			// reaches l.size (io.EOF was excluded from the error check
			// below on purpose, to tolerate an ordinary short final read),
			// so the loop above never terminates: IndexByte finds nothing,
			// l.off >= l.size stays false forever, and ReadAt keeps
			// returning (0, io.EOF) at the same offset — a 100% CPU
			// busy-loop that wedges the run slot indefinitely. Treat a
			// zero-byte EOF read as the TRUE end of data: force the
			// size-reached branch above to fire on the next iteration by
			// pinning l.off to l.size, so whatever real bytes made it into
			// l.buf still surface as a final line (or Scan cleanly returns
			// false if there were none).
			l.off = l.size
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			l.err = err
			return false
		}
	}
}
func (l *readerAtLineSource) Text() string { return l.line }
func (l *readerAtLineSource) Err() error   { return l.err }

// clampInt resolves an optional positive-int argument: <= 0 takes def, and
// anything above max is clamped down to max (never rejected — a model that
// asks for too much should get the most it can have, not an error).
func clampInt(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// readToolResultRange implements the offset/limit line window. maxBytes is
// the caller's FULL budget; the trailing notice's reserve (N10) is carved
// out of it internally so body+notice together never exceed maxBytes.
func readToolResultRange(sc toolResultLineSource, meta toolResultMeta, offset, limit, maxBytes int) (message.Parts, error) {
	if offset <= 0 {
		offset = 1
	}
	limit = clampInt(limit, readToolResultDefaultLimit, readToolResultMaxLimit)
	bodyMax := maxBytes - readToolResultNoticeReserve

	var b strings.Builder
	fmt.Fprintf(&b, "%s (tool=%s, %d bytes, %d lines) lines %d-%d:\n",
		meta.Handle, meta.Tool, meta.Bytes, meta.Lines, offset, offset+limit-1)

	line := 0
	shown := 0
	byteTrunc := false
	partialFirstLine := false
	for sc.Scan() {
		line++
		if line < offset {
			continue
		}
		if shown >= limit {
			break
		}
		t := sc.Text()
		if b.Len()+len(t)+1 > bodyMax {
			// A line that does not fit the remaining budget normally ends
			// the window. But if NOTHING has been shown yet, the very
			// first line alone is over budget (a minified-JSON or base64
			// dump retained as one enormous line), and stopping here would
			// report "no lines at offset 1" — making that result
			// permanently unreadable through this tool at any max_bytes.
			// Emit the prefix that does fit instead, UTF-8-safely, and let
			// the truncation notice below say so.
			if shown == 0 {
				if room := bodyMax - b.Len(); room > 0 {
					b.WriteString(truncateUTF8(t, room))
					b.WriteByte('\n')
					shown++
					// Round-3 review finding: this line's UNSHOWN remainder
					// must stay reachable. Reporting "continue with
					// offset+1" (the ordinary case, below) would silently
					// skip straight to the NEXT line, abandoning whatever
					// this line didn't fit — permanently, since every
					// future read at that name would start from line 2
					// too. partialFirstLine keeps the continuation offset
					// UNCHANGED instead: a caller that re-reads at the same
					// offset with a bigger max_bytes re-scans from byte
					// zero and genuinely reaches further into this same
					// line, rather than being told (accurately, but
					// uselessly) that the rest is just gone.
					partialFirstLine = true
				}
			}
			byteTrunc = true
			break
		}
		b.WriteString(t)
		b.WriteByte('\n')
		shown++
	}

	if shown == 0 {
		return jsonlessResult(fmt.Sprintf("%s: no lines at offset %d (the retained result has %d lines)",
			meta.Handle, offset, meta.Lines))
	}
	if partialFirstLine {
		fmt.Fprintf(&b, "[line %d exceeds max_bytes (%d); increase max_bytes and re-read at the same offset=%d to see more of it]\n",
			offset, maxBytes, offset)
		return jsonlessResult(b.String())
	}
	appendReadNotice(&b, meta, offset+shown, byteTrunc, maxBytes)
	return jsonlessResult(b.String())
}

// stringsContainsForSearch is strings.Contains, indirected only so a test
// can force "never matches" and confirm the test suite actually catches
// that (review finding N1's mutation-verification requirement). Never
// reassigned outside a test.
var stringsContainsForSearch = strings.Contains

// readToolResultSearch implements literal-substring search. offset/limit are
// deliberately ignored in this mode (documented in the tool description):
// a line window over a filtered set means something different from a line
// window over the file, and conflating the two is how a model ends up
// silently reading the wrong region. maxBytes is the caller's FULL budget;
// see readToolResultRange's doc comment for the notice-reserve accounting
// (N10), applied identically here.
func readToolResultSearch(sc toolResultLineSource, meta toolResultMeta, needle string, maxBytes int) (message.Parts, error) {
	bodyMax := maxBytes - readToolResultNoticeReserve

	var b strings.Builder
	fmt.Fprintf(&b, "%s (tool=%s, %d bytes, %d lines) lines matching %q:\n",
		meta.Handle, meta.Tool, meta.Bytes, meta.Lines, needle)

	line := 0
	matches := 0
	byteTrunc := false
	countCapped := false
	for sc.Scan() {
		line++
		t := sc.Text()
		// stringsContainsForSearch (== strings.Contains in production),
		// never regexp: literal by contract (see the package doc comment).
		// The indirection exists solely so
		// TestReadToolResultSearchNeverMatchMutant (review finding N1) can
		// force "never matches" from a test and confirm the test suite
		// actually notices — the mutation-verification AGENTS.md's
		// red-verify rule asks for, kept as a standing regression guard
		// rather than a one-off manual check.
		if !stringsContainsForSearch(t, needle) {
			continue
		}
		entry := fmt.Sprintf("%d: %s\n", line, t)
		if b.Len()+len(entry) > bodyMax {
			// Review finding N1: the ORIGINAL code dropped a too-large
			// entry WHOLE and never incremented matches, so a matching
			// line that alone exceeds the budget reported "0 match(es)" —
			// a false negative on exactly the retained-result-is-one-
			// enormous-line case F1 exists for, with unusable advice
			// ("narrow the search" on a file that IS one line). Fix: if
			// this is the FIRST thing found, emit a truncated WINDOW
			// AROUND THE MATCH — not just a prefix of the line, since the
			// match itself can sit megabytes into a multi-megabyte line,
			// well past what a from-byte-0 prefix would ever reach —
			// instead of silently reporting no match at all.
			if matches == 0 {
				prefix := fmt.Sprintf("%d: ", line)
				const ellipsis = "..."
				reserve := len(prefix) + 1 // +1 for the trailing newline
				if room := bodyMax - b.Len() - reserve - 2*len(ellipsis); room > 0 {
					idx := strings.Index(t, needle) // >= 0: strings.Contains already matched above
					window, truncBefore, truncAfter := extractMatchWindow(t, idx, room)
					b.WriteString(prefix)
					if truncBefore {
						b.WriteString(ellipsis)
					}
					b.WriteString(window)
					if truncAfter {
						b.WriteString(ellipsis)
					}
					b.WriteByte('\n')
					matches++
				}
			}
			byteTrunc = true
			break
		}
		b.WriteString(entry)
		matches++
		if matches >= readToolResultMaxLimit {
			// Review finding (round 3): this is a MATCH-COUNT stop, not a
			// byte-budget stop — reusing byteTrunc's notice ("...increase
			// max_bytes to see more") actively misdirects the model, since
			// raising max_bytes cannot surface a single additional match
			// once counting stopped. countCapped gets its own notice below.
			countCapped = true
			break
		}
	}

	if matches == 0 && !byteTrunc {
		return jsonlessResult(fmt.Sprintf("%s: no lines match %q (searched %d lines)", meta.Handle, needle, line))
	}
	switch {
	case countCapped:
		fmt.Fprintf(&b, "[stopped at %d match(es) (the match-count limit); narrow the search to see more — increasing max_bytes will not surface additional matches]\n", matches)
	case byteTrunc:
		fmt.Fprintf(&b, "[truncated at %d bytes after %d match(es); narrow the search or increase max_bytes to see more]\n", maxBytes, matches)
	}
	return jsonlessResult(b.String())
}

// extractMatchWindow returns a byte-bounded window of t, anchored a small
// fixed distance BEFORE idx (the needle's byte offset in t) rather than at
// byte 0 (review finding N1) — a match megabytes into a multi-megabyte
// single-line result must still be visible, which a from-the-start prefix
// window cannot guarantee. Both cut points are UTF-8-safe: truncBefore/
// truncAfter report whether that side was actually cut, so the caller can
// mark it with "...".
func extractMatchWindow(t string, idx, budget int) (window string, truncBefore, truncAfter bool) {
	const contextBefore = 64
	if idx < 0 {
		idx = 0
	}
	start := idx - contextBefore
	if start < 0 {
		start = 0
	}
	start = utf8SafeForward(t, start)
	truncBefore = start > 0
	rest := t[start:]
	window = truncateUTF8(rest, budget)
	truncAfter = len(window) < len(rest)
	return window, truncBefore, truncAfter
}

// utf8SafeForward advances n forward past any continuation bytes
// (0b10xxxxxx) at that cut point, so slicing t[n:] never begins mid-rune —
// the leading-edge counterpart of truncateUTF8's trailing-edge walk-back.
func utf8SafeForward(t string, n int) int {
	for n > 0 && n < len(t) && t[n]&0xC0 == 0x80 {
		n++
	}
	return n
}

// appendReadNotice writes the trailing continuation/truncation notice for a
// range read. A read that stopped early must SAY it stopped early and name
// the next offset — otherwise a model reasonably (and wrongly) treats a
// bounded window as the whole result, which is the single most damaging way
// this feature could mislead.
func appendReadNotice(b *strings.Builder, meta toolResultMeta, nextOffset int, byteTrunc bool, maxBytes int) {
	switch {
	case byteTrunc:
		fmt.Fprintf(b, "[truncated at %d bytes; continue with offset=%d]\n", maxBytes, nextOffset)
	case nextOffset <= meta.Lines:
		fmt.Fprintf(b, "[%d more line(s); continue with offset=%d]\n", meta.Lines-nextOffset+1, nextOffset)
	}
}

// jsonlessResult wraps plain text output. read_tool_result returns raw text
// rather than the jsonResult envelope the goal/mcp/process tools use: its
// payload IS file content, and JSON-encoding it would both inflate it
// (escaping every quote and newline in the bytes the byte budget just
// carefully bounded) and make it harder for a model to read.
func jsonlessResult(s string) (message.Parts, error) {
	return message.Parts{&message.Text{Text: s}}, nil
}
