package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// scanEvents opens GET /event?from=0 and passes each frame to visit until it
// returns true. It returns nil on that, or the stream error that ended the read
// first, such as ctx expiring.
func (p *serveProc) scanEvents(ctx context.Context, visit func(raw []byte) bool) error {
	p.t.Helper()
	return p.scanEventsFrom(ctx, 0, false, "", func(_ string, raw []byte) bool { return visit(raw) })
}

// scanEventsFrom is scanEvents with a resume cursor. header sends it as
// Last-Event-ID instead of the from query. session asks the server to filter.
// visit also gets the id field of the frame.
func (p *serveProc) scanEventsFrom(ctx context.Context, from int64, header bool, session string, visit func(id string, raw []byte) bool) error {
	p.t.Helper()
	q := url.Values{}
	if !header {
		q.Set("from", strconv.FormatInt(from, 10))
	}
	if session != "" {
		q.Set("session", session)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.addr+"/event?"+q.Encode(), nil)
	if err != nil {
		p.t.Fatalf("event request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if header {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(from, 10))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatalf("GET /event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			return err
		}
		if visit(sc.id, raw) {
			return nil
		}
	}
}

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
