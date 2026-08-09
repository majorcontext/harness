//go:build e2e

package workload

import (
	"bufio"
	"bytes"
	"io"
)

// sseEvent is one parsed server-sent event: the event name (if any) and its
// concatenated data lines, joined by newlines per the SSE spec.
type sseEvent struct {
	Name string
	ID   string
	Data []byte
}

// sseScanner reads a text/event-stream body one event at a time. It is a
// deliberately minimal reader — no reconnection, no retry field handling —
// enough to drive both the boxes-service event stream and the harness
// serve /event stream in this suite.
type sseScanner struct {
	r *bufio.Reader
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReader(r)}
}

// next reads until a blank line terminates one event, or returns io.EOF
// once the stream closes with no further event pending.
func (s *sseScanner) next() (sseEvent, error) {
	var ev sseEvent
	var data [][]byte
	sawAny := false
	for {
		line, err := s.r.ReadString('\n')
		trimmed := bytes.TrimRight([]byte(line), "\r\n")
		if len(trimmed) > 0 {
			sawAny = true
			switch {
			case bytes.HasPrefix(trimmed, []byte("event:")):
				ev.Name = string(bytes.TrimSpace(trimmed[len("event:"):]))
			case bytes.HasPrefix(trimmed, []byte("id:")):
				ev.ID = string(bytes.TrimSpace(trimmed[len("id:"):]))
			case bytes.HasPrefix(trimmed, []byte("data:")):
				data = append(data, bytes.TrimPrefix(trimmed[len("data:"):], []byte(" ")))
			}
		} else if sawAny {
			ev.Data = bytes.Join(data, []byte("\n"))
			return ev, nil
		}
		if err != nil {
			if sawAny {
				ev.Data = bytes.Join(data, []byte("\n"))
				return ev, nil
			}
			return sseEvent{}, err
		}
	}
}
