package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateSessionWithParentRoundTripsThroughRestart proves parent_session
// is accepted on POST /session, echoed on the created Session, and survives
// a process restart (persisted the same way Config.WorkDir is — see
// engine/store.go and TestParentSessionHeaderRoundTrip).
func TestCreateSessionWithParentRoundTripsThroughRestart(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}

	srv1 := newServer(t, dir, prov, 0)
	ts1 := httptest.NewServer(srv1)
	h1 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv1, ts: ts1}

	resp, data := h1.do("POST", "/session", map[string]string{
		"model":          "test/m1",
		"parent_session": "ses_1111111111111111",
	})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	var created struct {
		ID            string `json:"id"`
		ParentSession string `json:"parent_session"`
	}
	mustUnmarshal(t, data, &created)
	if created.ParentSession != "ses_1111111111111111" {
		t.Fatalf("created ParentSession = %q, want %q", created.ParentSession, "ses_1111111111111111")
	}

	resp, data = h1.do("GET", "/session/"+created.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get status %d: %s", resp.StatusCode, data)
	}
	var got struct {
		ParentSession string `json:"parent_session"`
	}
	mustUnmarshal(t, data, &got)
	if got.ParentSession != "ses_1111111111111111" {
		t.Fatalf("GET /session/{id} ParentSession = %q, want %q", got.ParentSession, "ses_1111111111111111")
	}

	// Also present on the list entry.
	resp, data = h1.do("GET", "/session", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list status %d: %s", resp.StatusCode, data)
	}
	var list []struct {
		ID            string `json:"id"`
		ParentSession string `json:"parent_session"`
	}
	mustUnmarshal(t, data, &list)
	found := false
	for _, s := range list {
		if s.ID == created.ID {
			found = true
			if s.ParentSession != "ses_1111111111111111" {
				t.Fatalf("list ParentSession = %q, want %q", s.ParentSession, "ses_1111111111111111")
			}
		}
	}
	if !found {
		t.Fatalf("session %s not found in list", created.ID)
	}

	ts1.Close()
	if err := srv1.Close(); err != nil {
		t.Fatalf("closing first server: %v", err)
	}

	srv2 := newServer(t, dir, prov, 0)
	ts2 := httptest.NewServer(srv2)
	t.Cleanup(ts2.Close)
	h2 := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv2, ts: ts2}

	resp, data = h2.do("GET", "/session/"+created.ID, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get after restart status %d: %s", resp.StatusCode, data)
	}
	mustUnmarshal(t, data, &got)
	if got.ParentSession != "ses_1111111111111111" {
		t.Fatalf("ParentSession after restart = %q, want %q", got.ParentSession, "ses_1111111111111111")
	}
}

// TestCreateSessionWithoutParentOmitsField proves parent_session is absent
// (not an empty string) on a session created without it — the common case.
func TestCreateSessionWithoutParentOmitsField(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("POST", "/session", map[string]string{"model": "test/m1"})
	if resp.StatusCode != 201 {
		t.Fatalf("create status %d: %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), "parent_session") {
		t.Fatalf("expected parent_session to be omitted, got %s", data)
	}
}

// TestCreateSessionParentSessionValidation exercises the 400 rejection
// rules: empty string and oversized (>128 bytes) are both rejected.
func TestCreateSessionParentSessionValidation(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})

	resp, data := h.do("POST", "/session", map[string]string{
		"model":          "test/m1",
		"parent_session": "",
	})
	// An absent key and an explicit empty string are indistinguishable over
	// the wire in Go's JSON decode into a string field without a pointer, so
	// this case is exercised via a raw body below instead.
	_ = resp
	_ = data

	resp, data = h.do("POST", "/session", map[string]string{
		"model":          "test/m1",
		"parent_session": strings.Repeat("x", 129),
	})
	if resp.StatusCode != 400 {
		t.Fatalf("oversized parent_session status = %d, want 400: %s", resp.StatusCode, data)
	}

	resp, data = h.do("POST", "/session", map[string]string{
		"model":          "test/m1",
		"parent_session": strings.Repeat("x", 128),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("128-byte parent_session status = %d, want 201: %s", resp.StatusCode, data)
	}
}

// TestCreateSessionParentSessionExplicitEmptyRejected uses a raw JSON body
// (rather than the map[string]string helper, which cannot distinguish an
// absent key from an empty-string value once marshaled) to prove an
// explicit empty string is rejected, matching the "if present it must be
// non-empty" rule.
func TestCreateSessionParentSessionExplicitEmptyRejected(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	req, data := h.doRaw("POST", "/session", `{"model":"test/m1","parent_session":""}`)
	if req.StatusCode != 400 {
		t.Fatalf("explicit empty parent_session status = %d, want 400: %s", req.StatusCode, data)
	}
}
