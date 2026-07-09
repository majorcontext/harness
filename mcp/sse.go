package mcp

import (
	"bufio"
	"bytes"
	"io"
)

// sseEvent is one parsed Server-Sent Events event
// (https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation).
// Only the fields the Streamable HTTP transport cares about are kept.
type sseEvent struct {
	id   string
	data []byte
}

// readSSE reads Server-Sent Events from r, calling onEvent once per
// complete event (a run of "data:" lines terminated by a blank line).
// Lines with unrecognized field names (e.g. "event", "retry") are ignored,
// as the transport only needs the data payload and, for future
// resumability, the event id. onEvent returning true stops reading (e.g.
// once the response we were waiting for has arrived) without treating the
// remainder of the stream as an error.
func readSSE(r io.Reader, onEvent func(sseEvent) (stop bool, err error)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var data [][]byte
	var id string
	flush := func() (bool, error) {
		if len(data) == 0 {
			id = ""
			return false, nil
		}
		ev := sseEvent{id: id, data: bytes.Join(data, []byte("\n"))}
		data = nil
		id = ""
		return onEvent(ev)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			stop, err := flush()
			if err != nil || stop {
				return err
			}
			continue
		}
		field, value, _ := cutSSEField(line)
		switch field {
		case "data":
			data = append(data, []byte(value))
		case "id":
			id = value
		default:
			// event, retry, comments (":"-prefixed) — not needed.
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A stream may end without a trailing blank line; flush whatever's
	// pending.
	_, err := flush()
	return err
}

// cutSSEField splits "field: value" or "field:value" per the SSE spec
// (a single leading space after the colon, if present, is stripped).
func cutSSEField(line string) (field, value string, ok bool) {
	i := indexByte(line, ':')
	if i < 0 {
		return line, "", false
	}
	field = line[:i]
	value = line[i+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
