package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

// maxSSELine bounds one line of an events stream. A frame holds one event on one line.
const maxSSELine = 64 << 20

// DecodeSSE decodes an events stream, the body of GET /sessions/{id}/events with Accept text/event-stream.
// It yields each frame as an Event, durable and ephemeral. An error frame yields a *Error and ends
// the sequence, and so does a frame that is not an Event, and a read error of r. The sequence ends
// without an error when r ends. A frame that r cuts before its blank line is dropped.
// DecodeSSE does not close r; closing it ends a pending read with an error. The sequence is single-use.
func DecodeSSE(r io.Reader) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
		var name string
		var data []string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if len(data) == 0 {
					name = ""
					continue
				}
				ev, err := decodeFrame(name, strings.Join(data, "\n"))
				name, data = "", nil
				if err != nil {
					yield(Event{}, err)
					return
				}
				if !yield(ev, nil) {
					return
				}
				continue
			}
			field, value, _ := strings.Cut(line, ":")
			if field == "" {
				continue
			}
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				name = value
			case "data":
				data = append(data, value)
			}
		}
		if err := sc.Err(); err != nil {
			yield(Event{}, err)
		}
	}
}

func decodeFrame(name, data string) (Event, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &members); err != nil {
		return Event{}, fmt.Errorf("decode %s frame: %w", frameName(name), err)
	}
	if name == "error" {
		var body ErrorBody
		if err := json.Unmarshal([]byte(data), &body); err != nil {
			return Event{}, fmt.Errorf("decode error frame: %w", err)
		}
		if body.Error.Code == "" {
			return Event{}, errors.New("decode error frame: no error code")
		}
		return Event{}, &body.Error
	}
	for _, k := range []string{"seq", "t", "k", "d"} {
		if v, ok := members[k]; !ok || string(v) == "null" {
			return Event{}, fmt.Errorf("decode event frame: no %q member", k)
		}
	}
	var ev Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return Event{}, fmt.Errorf("decode event frame: %w", err)
	}
	return ev, nil
}

func frameName(name string) string {
	if name == "error" {
		return "error"
	}
	return "event"
}
