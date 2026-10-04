// Package toolresult keeps a large tool result out of the history: the
// history holds a preview with a handle, and the read_tool_result tool
// reads the rest back in bounded windows.
package toolresult

import (
	"fmt"
	"strconv"
	"strings"
)

// Prefix starts each handle. A handle is short so that a model can type it back.
const Prefix = "trh_"

// headBytes bounds Meta.Head.
const headBytes = 80

// indexMax bounds the handles that Index lists one by one.
const indexMax = 32

const (
	// Inline is the size above which a result is retained, and the size of its preview.
	Inline = 16384
	// Budget bounds the retained bytes of a session.
	Budget = 4 << 20
)

// Meta describes one retained result. Bytes, Lines, and Head describe the
// masked text in the blob named Key.
type Meta struct {
	Handle string
	Tool   string
	Key    string
	Bytes  int
	Lines  int
	Head   string
}

// Handle returns handle number n.
func Handle(n int) string { return Prefix + strconv.Itoa(n) }

// Number returns the number of handle h. It accepts only a handle that
// Handle makes: digits with no sign and no leading zero.
func Number(h string) (int, bool) {
	rest, ok := strings.CutPrefix(h, Prefix)
	if !ok || rest == "" || rest[0] < '1' || rest[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

// NewMeta describes masked, retained under handle in the blob key.
func NewMeta(handle, tool, key, masked string) Meta {
	return Meta{Handle: handle, Tool: tool, Key: key, Bytes: len(masked), Lines: countLines(masked), Head: truncate(masked, headBytes)}
}

// Preview is the history text of a retained result: a header that names
// the handle, then the first Inline bytes of masked.
func Preview(m Meta, masked string) string {
	p := truncate(masked, Inline)
	return fmt.Sprintf("[tool result retained: handle=%s tool=%s bytes=%d lines=%d preview_bytes=%d — read the rest with read_tool_result(handle=%q)]\n%s",
		m.Handle, m.Tool, m.Bytes, m.Lines, len(p), m.Handle, p)
}

// Refused is the history text of a result that the retained total refuses.
// It names no handle, because nothing was written, and it does not say
// that a later, smaller result is refused too.
func Refused(tool, masked string) string {
	p := truncate(masked, Inline)
	return fmt.Sprintf("[tool result truncated: tool=%s bytes=%d preview_bytes=%d — retaining this result would exceed the per-session retention budget; its remainder is discarded irrecoverably, though a smaller result later this session may still be retained]\n%s",
		tool, len(masked), len(p), p)
}

// Index lists the newest retained results for a compaction summary, so a
// handle stays reachable after the preview that named it is folded. metas
// are in handle order. readable reports whether a blob still exists.
func Index(metas []Meta, readable func(Meta) bool) string {
	if len(metas) == 0 {
		return ""
	}
	older := max(len(metas)-indexMax, 0)
	var b strings.Builder
	b.WriteString("[retained tool results (this index is machine-generated, not part of the summary above):\n")
	for _, m := range metas[older:] {
		status := "still readable via read_tool_result"
		if !readable(m) {
			status = "sidecar file missing, no longer readable"
		}
		fmt.Fprintf(&b, "  %s tool=%s bytes=%d lines=%d head=%q (%s)\n", m.Handle, m.Tool, m.Bytes, m.Lines, m.Head, status)
	}
	if older > 0 {
		fmt.Fprintf(&b, "  ...and %d older retained result(s) (not listed; still on disk if their sidecar files survive, addressable by handle number if known)\n", older)
	}
	b.WriteString("]")
	return b.String()
}

// countLines counts one line per '\n', plus a last line with no '\n'.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// truncate returns the longest prefix of s of at most n bytes that does
// not split a rune.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
