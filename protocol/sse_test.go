package protocol

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type failingReader struct{ r io.Reader }

func (f failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestDecodeSSE(t *testing.T) {
	ev := `{"seq":2,"t":"2026-01-01T00:00:00Z","k":"turn.started","d":{}}`
	for _, tc := range []struct {
		name   string
		in     io.Reader
		seqs   []uint64
		errMsg string
		code   string
	}{
		{"two events", strings.NewReader("id: 1\ndata: " + ev + "\n\n: ping\nid: 2\ndata: " + ev + "\n\n"), []uint64{2, 2}, "", ""},
		{"multiline data", strings.NewReader("data: {\"seq\":2,\"t\":\"2026-01-01T00:00:00Z\",\ndata: \"k\":\"x\",\"d\":{}}\n\n"), []uint64{2}, "", ""},
		{"error frame ends", strings.NewReader("event: error\ndata: {\"error\":{\"code\":\"session_not_found\",\"message\":\"gone\"}}\n\ndata: " + ev + "\n\n"), nil, "gone", "session_not_found"},
		{"empty object", strings.NewReader("data: {}\n\n"), nil, `no "seq" member`, ""},
		{"null", strings.NewReader("data: null\n\n"), nil, `no "seq" member`, ""},
		{"missing member", strings.NewReader(`data: {"seq":1,"t":"2026-01-01T00:00:00Z","k":"x"}` + "\n\n"), nil, `no "d" member`, ""},
		{"empty error object", strings.NewReader("event: error\ndata: {}\n\n"), nil, "no error code", ""},
		{"truncated frame dropped", strings.NewReader("data: " + ev + "\n\ndata: " + ev), []uint64{2}, "", ""},
		{"read error", failingReader{strings.NewReader("data: " + ev + "\n\n")}, []uint64{2}, "connection reset", ""},
	} {
		var seqs []uint64
		var gotErr error
		for e, err := range DecodeSSE(tc.in) {
			if err != nil {
				gotErr = err
				break
			}
			seqs = append(seqs, e.Seq)
		}
		if len(seqs) != len(tc.seqs) {
			t.Errorf("%s: seqs = %v, want %v", tc.name, seqs, tc.seqs)
		}
		if (tc.errMsg == "") != (gotErr == nil) || gotErr != nil && !strings.Contains(gotErr.Error(), tc.errMsg) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, gotErr, tc.errMsg)
		}
		var pe *Error
		if tc.code != "" && (!errors.As(gotErr, &pe) || string(pe.Code) != tc.code) {
			t.Errorf("%s: err = %v, want *Error with code %q", tc.name, gotErr, tc.code)
		}
	}
}
