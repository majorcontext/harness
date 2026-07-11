package server

import "testing"

func TestBearerSubprotocolToken(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"", "", false},
		{"bearer.abc123", "abc123", true},
		{"bearer.abc123, other", "abc123", true},
		{"other, bearer.abc123", "abc123", true},
		{" bearer.abc123 ", "abc123", true},
		{"notbearer.abc123", "", false},
		{"bearer.", "", true}, // empty token still "found"; caller compares it
	}
	for _, c := range cases {
		tok, ok := bearerSubprotocolToken(c.header)
		if tok != c.wantToken || ok != c.wantOK {
			t.Errorf("bearerSubprotocolToken(%q) = %q, %v; want %q, %v", c.header, tok, ok, c.wantToken, c.wantOK)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		cors   string
		origin string
		want   bool
	}{
		{"", "http://x", false},
		{"*", "http://anything", true},
		{"http://localhost:7777", "http://localhost:7777", true},
		{"http://localhost:7777", "http://evil.example", false},
		{"http://localhost:7777", "HTTP://LOCALHOST:7777", false}, // exact match, no case folding
	}
	for _, c := range cases {
		s := &Server{opts: Options{CORSOrigin: c.cors}}
		if got := s.originAllowed(c.origin); got != c.want {
			t.Errorf("originAllowed(cors=%q, origin=%q) = %v; want %v", c.cors, c.origin, got, c.want)
		}
	}
}

// TestTermUnsupportedPlatformReturns501 is the "windows path" coverage the
// unix e2e test (term_unix_test.go) can't provide: it exercises handleTerm's
// ptySupported branch for real. On this platform (unix, since the e2e test
// file requires it) ptySupported is true, so there is nothing to assert here
// — the branch this test targets doesn't exist. On a !unix GOOS (built from
// term_other.go's ptySupported = false), it runs for real and asserts the
// exact contract: a plain 501 response, no WebSocket upgrade, before ever
// touching a PTY.
func TestTermUnsupportedPlatformReturns501(t *testing.T) {
	if ptySupported {
		t.Skip("this platform supports PTYs; see term_unix_test.go for the real e2e coverage")
	}
	h := newHarnessOpts(t, t.TempDir(), &scriptedProvider{name: "test"}, 0)
	resp, body := h.do("GET", "/term", nil)
	if resp.StatusCode != 501 {
		t.Fatalf("status = %d, want 501; body: %s", resp.StatusCode, body)
	}
}
