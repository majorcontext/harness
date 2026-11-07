package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

type accountProxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f accountProxyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type accountProxyBrokenBody struct {
	data string
	err  error
}

func (b *accountProxyBrokenBody) Read(p []byte) (int, error) {
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, b.err
}
func (*accountProxyBrokenBody) Close() error { return nil }

func TestAccountProxyDiagnostics(t *testing.T) {
	skipShort(t)
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, data         string
		err, readErr       error
		cancel, singleByte bool
		status             int
		want               []string
	}{
		{name: "complete stream", data: terminal},
		{name: "split terminal", data: strings.ReplaceAll(terminal, "\n", "\r\n"), singleByte: true},
		{name: "partial terminal", data: "event: message_stop\n", want: []string{"phase=stream", "eof=true", "context=active"}},
		{name: "truncated stream", data: "event: message_start\n\n", want: []string{"request=1", "phase=stream", "eof=true", "context=active", "after="}},
		{name: "broken stream", readErr: io.ErrUnexpectedEOF, cancel: true, want: []string{"phase=stream", "unexpected_eof=true", "context=canceled"}},
		{name: "data and error", data: terminal, readErr: io.ErrUnexpectedEOF, want: []string{"unexpected_eof=true"}},
		{name: "upgrade untouched", status: http.StatusSwitchingProtocols, data: "opaque", readErr: io.ErrUnexpectedEOF},
		{name: "transport failure", err: errors.New("secret-token https://secret.invalid/private"), want: []string{"phase=transport", "error_type=*errors.errorString", "context=active"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := new(accountProxyCapture)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://secret:password@secret.invalid/private", nil)
			if err != nil {
				t.Fatal(err)
			}
			body := io.ReadCloser(&accountProxyBrokenBody{data: tc.data, err: tc.readErr})
			if tc.readErr == nil {
				reader := io.Reader(strings.NewReader(tc.data))
				if tc.singleByte {
					reader = iotest.OneByteReader(reader)
				}
				body = io.NopCloser(reader)
			}
			status := tc.status
			if status == 0 {
				status = http.StatusOK
			}
			rt := accountProxyTransport{base: accountProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
			}), capture: capture}
			resp, err := rt.RoundTrip(req)
			if !errors.Is(err, tc.err) {
				t.Fatalf("RoundTrip error = %v, want %v", err, tc.err)
			}
			if resp != nil {
				got, readErr := io.ReadAll(resp.Body)
				if string(got) != tc.data || !errors.Is(readErr, tc.readErr) {
					t.Fatalf("read = %q, %v; want %q, %v", got, readErr, tc.data, tc.readErr)
				}
				_, _ = resp.Body.Read(make([]byte, 1))
				_ = resp.Body.Close()
			}
			checkAccountProxyDiagnostics(t, capture, tc.want)
		})
	}
}

func checkAccountProxyDiagnostics(t *testing.T, capture *accountProxyCapture, want []string) {
	t.Helper()
	if got := capture.diagnosticLines(false); len(got) != 0 {
		t.Fatalf("successful test logged %v", got)
	}
	lines := capture.diagnosticLines(true)
	if len(want) == 0 {
		if len(lines) != 0 {
			t.Fatalf("complete stream logged %v", lines)
		}
		return
	}
	if len(lines) != 1 {
		t.Fatalf("diagnostics = %v, want only the first error", lines)
	}
	for _, part := range want {
		if !strings.Contains(lines[0], part) {
			t.Errorf("diagnostic %q lacks %q", lines[0], part)
		}
	}
	for _, secret := range []string{"secret", "password", "private"} {
		if strings.Contains(lines[0], secret) {
			t.Errorf("diagnostic leaks %q: %s", secret, lines[0])
		}
	}
}
