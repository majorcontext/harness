package process

import (
	"bytes"
	"io"
	"os"
)

// tailScanCap bounds how many trailing bytes tailFile ever reads from a
// log file, so a long-running dev server's megabytes-large log cannot
// balloon memory just to answer "last 50 lines" — mirrors the spirit of
// engine/bash.go's cappedWriter (bounded capture, not unbounded then
// truncate).
const tailScanCap = 1 << 20 // 1 MiB

// tailFile returns the last n lines of the file at path. A missing file
// returns an *os.PathError satisfying os.IsNotExist, so callers can treat
// "never started" as "no logs yet" rather than an error.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	start := int64(0)
	if size > tailScanCap {
		start = size - tailScanCap
	}
	if _, err := f.Seek(start, 0); err != nil {
		return "", err
	}
	return lastLines(f, size-start, n)
}

// lastLines reads want bytes from r (tolerating arbitrary read chunking —
// a single Read may legally short-read, which previously left NUL bytes
// in the emitted tail) and returns the last n newline-separated lines of
// what was actually read. A reader that ends early (file truncated
// between Stat and read) yields the bytes it produced, not an error.
func lastLines(r io.Reader, want int64, n int) (string, error) {
	buf := make([]byte, want)
	read, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	buf = buf[:read]

	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n"))), nil
}
