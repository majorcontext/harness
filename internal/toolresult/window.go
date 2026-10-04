package toolresult

import (
	"bufio"
	"fmt"
	"strings"
)

// window returns limit lines from offset within maxBytes. A read that
// stops early says so and names the next offset, so a model never takes a
// window for the whole result.
func window(sc *bufio.Scanner, m Meta, offset, limit, maxBytes int) string {
	offset = max(offset, 1)
	limit = clamp(limit, defaultLimit, maxLimit)
	bodyMax := maxBytes - noticeReserve
	var b strings.Builder
	b.WriteString(rangePreamble(m, offset, limit))
	line, shown := 0, 0
	cut, partial := false, false
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
			// A first line over the budget shows its prefix, or one huge
			// line could never be read at any max_bytes.
			if room := bodyMax - b.Len(); shown == 0 && room > 0 {
				b.WriteString(truncate(t, room) + "\n")
				shown++
				partial = true
			}
			cut = true
			break
		}
		b.WriteString(t + "\n")
		shown++
	}
	switch next := offset + shown; {
	case shown == 0:
		return fmt.Sprintf("%s: no lines at offset %d (the retained result has %d lines)", m.Handle, offset, m.Lines)
	case partial:
		fmt.Fprintf(&b, "[line %d exceeds max_bytes (%d); increase max_bytes and re-read at the same offset=%d to see more of it]\n", offset, maxBytes, offset)
	case cut:
		fmt.Fprintf(&b, "[truncated at %d bytes; continue with offset=%d]\n", maxBytes, next)
	case next <= m.Lines:
		fmt.Fprintf(&b, "[%d more line(s); continue with offset=%d]\n", m.Lines-next+1, next)
	}
	return b.String()
}

// search returns the lines that hold needle, a literal substring: a
// model-written regexp over a large result is a ReDoS risk.
func search(sc *bufio.Scanner, m Meta, needle string, maxBytes int) string {
	bodyMax := maxBytes - noticeReserve
	var b strings.Builder
	b.WriteString(searchPreamble(m, needle))
	line, matches := 0, 0
	cut, capped := false, false
	for sc.Scan() {
		line++
		t := sc.Text()
		if !strings.Contains(t, needle) {
			continue
		}
		entry := fmt.Sprintf("%d: %s\n", line, t)
		if b.Len()+len(entry) > bodyMax {
			if matches == 0 {
				matches += around(&b, t, needle, line, bodyMax)
			}
			cut = true
			break
		}
		b.WriteString(entry)
		matches++
		if matches >= maxLimit {
			capped = true
			break
		}
	}
	switch {
	case matches == 0 && !cut:
		return fmt.Sprintf("%s: no lines match %q (searched %d lines)", m.Handle, needle, line)
	case capped:
		fmt.Fprintf(&b, "[stopped at %d match(es) (the match-count limit); narrow the search to see more — increasing max_bytes will not surface additional matches]\n", matches)
	case cut:
		fmt.Fprintf(&b, "[truncated at %d bytes after %d match(es); narrow the search or increase max_bytes to see more]\n", maxBytes, matches)
	}
	return b.String()
}

// around writes the part of a matching line t that fits the budget,
// starting a little before the match, which can be far into a huge line.
// It returns 1 when it wrote the line.
func around(b *strings.Builder, t, needle string, line, bodyMax int) int {
	const ellipsis = "..."
	prefix := fmt.Sprintf("%d: ", line)
	room := bodyMax - b.Len() - len(prefix) - 1 - 2*len(ellipsis)
	if room <= 0 {
		return 0
	}
	start := max(strings.Index(t, needle)-matchContext, 0)
	for start > 0 && start < len(t) && t[start]&0xC0 == 0x80 {
		start++
	}
	rest := t[start:]
	w := truncate(rest, room)
	b.WriteString(prefix)
	if start > 0 {
		b.WriteString(ellipsis)
	}
	b.WriteString(w)
	if len(w) < len(rest) {
		b.WriteString(ellipsis)
	}
	b.WriteByte('\n')
	return 1
}
