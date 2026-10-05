package e2e

import (
	"bytes"
	"io"
	"strings"
)

// sseScanner extracts the data payload of each SSE frame from a reader.
type sseScanner struct {
	r   *bufReader
	buf bytes.Buffer
	id  string // the id field of the frame next returned
}

func newSSEScanner(r io.Reader) *sseScanner { return &sseScanner{r: newBufReader(r)} }

func (s *sseScanner) next() ([]byte, error) {
	s.buf.Reset()
	s.id = ""
	got := false
	for {
		line, err := s.r.readLine()
		if err != nil {
			if got {
				return s.buf.Bytes(), nil
			}
			return nil, err
		}
		switch {
		case line == "":
			if got {
				return s.buf.Bytes(), nil
			}
		case strings.HasPrefix(line, "id:"):
			s.id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			s.buf.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			got = true
		}
	}
}

// bufReader is a minimal line reader that respects the underlying reader's
// (context-driven) read errors so the bounded read in eventReplay terminates.
type bufReader struct {
	r   io.Reader
	buf []byte
}

func newBufReader(r io.Reader) *bufReader { return &bufReader{r: r} }

func (b *bufReader) readLine() (string, error) {
	for {
		if i := bytes.IndexByte(b.buf, '\n'); i >= 0 {
			line := string(bytes.TrimRight(b.buf[:i], "\r"))
			b.buf = b.buf[i+1:]
			return line, nil
		}
		tmp := make([]byte, 4096)
		n, err := b.r.Read(tmp)
		if n > 0 {
			b.buf = append(b.buf, tmp[:n]...)
			continue
		}
		if err != nil {
			return "", err
		}
	}
}
