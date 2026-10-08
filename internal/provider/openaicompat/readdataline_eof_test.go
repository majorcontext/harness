package openaicompat

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadDataLine_EOFOnPartialDataLine covers the connection-dropped-
// mid-chunk case: the server sends a "data: " prefixed line but the
// connection closes (EOF) before a trailing newline arrives. readDataLine
// must not hang or panic — it should hand back whatever partial payload it
// has (or a clean io.EOF), and the caller must surface a typed error rather
// than wedge.
func TestReadDataLine_EOFOnPartialDataLine(t *testing.T) {
	const partial = `data: {"id":"chatcmpl_1","choices":[{"delta":{"content":"partial`
	r := strings.NewReader(partial)
	body := io.NopCloser(r)
	s := &stream{body: body, r: bufio.NewReader(r)}

	ev, err := s.Next()

	if err == nil {
		t.Fatalf("expected an error for a truncated trailing data line, got event %+v with nil error", ev)
	}
	// The line has a full "data:" prefix, so readDataLine hands the
	// (incomplete) JSON payload up rather than swallowing it as EOF; the
	// caller's JSON decode then fails cleanly. Either that decode error or a
	// bare io.EOF is an acceptable "safe" outcome — anything else (panic,
	// hang) is not, and the goroutine-less synchronous call above already
	// proves there's no hang.
	if !errors.Is(err, io.EOF) {
		var wantSubstring = "bad chunk"
		if !strings.Contains(err.Error(), wantSubstring) {
			t.Fatalf("unexpected error shape: %v", err)
		}
	}
}

// TestReadDataLine_EOFMidDataPrefix covers the case where the connection
// drops before even the "data:" field-name prefix has fully arrived — the
// line has no recognizable field name at all, so readDataLine must resolve
// this to a plain io.EOF instead of misinterpreting a fragment as a payload.
func TestReadDataLine_EOFMidDataPrefix(t *testing.T) {
	const partial = `dat`
	r := strings.NewReader(partial)
	body := io.NopCloser(r)
	s := &stream{body: body, r: bufio.NewReader(r)}

	_, err := s.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF for a line cut off mid field-name, got %v", err)
	}
}

// TestReadDataLine_EOFAfterBlankLine covers a clean EOF that arrives exactly
// on an event boundary (no partial line pending at all): Next must return
// io.EOF, never hang or panic.
func TestReadDataLine_EOFAfterBlankLine(t *testing.T) {
	r := strings.NewReader("")
	body := io.NopCloser(r)
	s := &stream{body: body, r: bufio.NewReader(r)}

	_, err := s.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty stream, got %v", err)
	}
}
