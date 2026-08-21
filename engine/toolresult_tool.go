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

	// readToolResultMinMaxBytes is the floor on a caller-supplied max_bytes.
	// Below it, the fixed preamble every read writes (the "handle (tool=...,
	// N bytes, N lines) ..." line, plus a truncation/continuation notice)
	// can itself exceed the budget, leaving zero or negative room for any
	// line — the byte-budget loop in readToolResultRange then reports "no
	// lines at offset N" even though the result has plenty, because nothing
	// EVER fit, not because nothing was there (review finding F11). Rather
	// than silently produce that misleading message, a request below the
	// floor is rejected outright with an error naming the floor.
	readToolResultMinMaxBytes = 256
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
	// see readToolResultMinMaxBytes's doc comment (review finding F11).
	if in.MaxBytes > 0 && in.MaxBytes < readToolResultMinMaxBytes {
		return nil, fmt.Errorf("%s: max_bytes %d is below the minimum %d",
			readToolResultToolName, in.MaxBytes, readToolResultMinMaxBytes)
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
		// once against a raw io.ReaderAt read of the whole file with no
		// per-line size limit, rather than reporting this retained result
		// permanently unreadable.
		lines, ferr := toolResultFallbackLines(f, meta.Bytes)
		if ferr == nil {
			parts, err = scan(&sliceLineSource{lines: lines})
		}
	}
	return parts, err
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

// sliceLineSource adapts a pre-split []string to toolResultLineSource.
type sliceLineSource struct {
	lines []string
	i     int
}

func (l *sliceLineSource) Scan() bool {
	if l.i >= len(l.lines) {
		return false
	}
	l.i++
	return true
}
func (l *sliceLineSource) Text() string { return l.lines[l.i-1] }
func (l *sliceLineSource) Err() error   { return nil }

// toolResultFallbackLines reads the retained file's first size bytes via a
// raw io.ReaderAt read — independent of any *bufio.Scanner's already-
// advanced internal read position on the same *os.File, which is exactly
// why ReaderAt (an absolute-offset read) is used here instead of Read/Seek
// — and splits on '\n' with no per-line length limit. size is meta.Bytes:
// the ORIGINAL byte length recorded at retention time (see
// writeRetainedToolResult), which is what the file was written as.
func toolResultFallbackLines(r io.ReaderAt, size int) ([]string, error) {
	if size <= 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := r.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	text := string(buf[:n])
	if text == "" {
		return nil, nil
	}
	text = strings.TrimSuffix(text, "\n")
	return strings.Split(text, "\n"), nil
}

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

// readToolResultRange implements the offset/limit line window.
func readToolResultRange(sc toolResultLineSource, meta toolResultMeta, offset, limit, maxBytes int) (message.Parts, error) {
	if offset <= 0 {
		offset = 1
	}
	limit = clampInt(limit, readToolResultDefaultLimit, readToolResultMaxLimit)

	var b strings.Builder
	fmt.Fprintf(&b, "%s (tool=%s, %d bytes, %d lines) lines %d-%d:\n",
		meta.Handle, meta.Tool, meta.Bytes, meta.Lines, offset, offset+limit-1)

	line := 0
	shown := 0
	byteTrunc := false
	for sc.Scan() {
		line++
		if line < offset {
			continue
		}
		if shown >= limit {
			break
		}
		t := sc.Text()
		if b.Len()+len(t)+1 > maxBytes {
			// A line that does not fit the remaining budget normally ends
			// the window. But if NOTHING has been shown yet, the very
			// first line alone is over budget (a minified-JSON or base64
			// dump retained as one enormous line), and stopping here would
			// report "no lines at offset 1" — making that result
			// permanently unreadable through this tool at any max_bytes.
			// Emit the prefix that does fit instead, UTF-8-safely, and let
			// the truncation notice below say so.
			if shown == 0 {
				if room := maxBytes - b.Len(); room > 0 {
					b.WriteString(truncateUTF8(t, room))
					b.WriteByte('\n')
					shown++
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
	appendReadNotice(&b, meta, offset+shown, byteTrunc, maxBytes)
	return jsonlessResult(b.String())
}

// readToolResultSearch implements literal-substring search. offset/limit are
// deliberately ignored in this mode (documented in the tool description):
// a line window over a filtered set means something different from a line
// window over the file, and conflating the two is how a model ends up
// silently reading the wrong region.
func readToolResultSearch(sc toolResultLineSource, meta toolResultMeta, needle string, maxBytes int) (message.Parts, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (tool=%s, %d bytes, %d lines) lines matching %q:\n",
		meta.Handle, meta.Tool, meta.Bytes, meta.Lines, needle)

	line := 0
	matches := 0
	byteTrunc := false
	for sc.Scan() {
		line++
		t := sc.Text()
		// strings.Contains, never regexp: literal by contract (see the
		// package doc comment).
		if !strings.Contains(t, needle) {
			continue
		}
		entry := fmt.Sprintf("%d: %s\n", line, t)
		if b.Len()+len(entry) > maxBytes {
			byteTrunc = true
			break
		}
		b.WriteString(entry)
		matches++
		if matches >= readToolResultMaxLimit {
			byteTrunc = true
			break
		}
	}

	if matches == 0 && !byteTrunc {
		return jsonlessResult(fmt.Sprintf("%s: no lines match %q (searched %d lines)", meta.Handle, needle, line))
	}
	if byteTrunc {
		fmt.Fprintf(&b, "[truncated at %d bytes after %d match(es); narrow the search to see more]\n", maxBytes, matches)
	}
	return jsonlessResult(b.String())
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
