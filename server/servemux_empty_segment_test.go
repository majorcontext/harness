package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/provider"
)

// TestEmptySessionIDSegmentAnswers404WithoutRedirect pins the fix for a
// routing gap in every "/session/{id}/..." handler: an EMPTY {id} segment
// ("/session//abort") never reaches the mux's {id} patterns at all.
//
// net/http's ServeMux calls cleanPath internally, which collapses the
// doubled slash and issues a 301 redirect to the cleaned path (e.g.
// "/session/abort"). Go's http.Client, following that redirect, converts a
// POST or DELETE to GET (RFC 7231 semantics net/http implements for 301).
// The request that actually lands is a GET to a path with no {id} segment
// at all, matching whatever route (if any) shares that shape — never the
// {id} handler, and never triggering the documented sessionIDOrNotFound 404
// contract (see server/handlers.go).
//
// This test asserts the MECHANISM, not just the final status: it checks
// resp.Request.URL.Path to prove no redirect-and-relanding occurred. A
// status-only assertion would keep passing even if the guard were removed,
// for any route table where the redirect target happens to answer 404 on
// its own.
func TestEmptySessionIDSegmentAnswers404WithoutRedirect(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)

	// Every {id}-keyed sub-resource this server registers (see the
	// mux.HandleFunc calls in server/server.go's route table).
	cases := []struct{ method, path string }{
		{"GET", "/session//message"},
		{"GET", "/session//request"},
		{"GET", "/session//wait"},
		{"POST", "/session//prompt_async"},
		{"POST", "/session//enqueue"},
		{"GET", "/session//queue"},
		{"DELETE", "/session//queue"},
		{"POST", "/session//compact"},
		{"POST", "/session//goal"},
		{"DELETE", "/session//goal"},
		{"POST", "/session//model"},
		{"POST", "/session//thinking"},
		{"POST", "/session//abort"},
	}
	for _, c := range cases {
		resp, data := h.do(c.method, c.path, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404: %s", c.method, c.path, resp.StatusCode, data)
		}
		// The mechanism assertion: the final request URL must still carry
		// the empty segment. If it doesn't, the mux's cleanPath redirect
		// fired and the request landed somewhere else entirely.
		if got := resp.Request.URL.Path; !strings.HasPrefix(got, "/session//") {
			t.Errorf("%s %s: final request path = %q, want prefix \"/session//\" (a redirect occurred; the empty-id guard must answer before the mux runs)",
				c.method, c.path, got)
		}
	}
}

// TestEmptySessionIDGuardLeavesRealRoutesAlone is the counterweight: the
// pre-mux guard must match ONLY an empty {id} segment. A guard drawn even
// slightly too wide — matching "/session" or "/session/" — would break the
// session collection endpoint itself, a worse regression than the one being
// fixed here.
func TestEmptySessionIDGuardLeavesRealRoutesAlone(t *testing.T) {
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{asstTurn("done")}}
	h := newHarness(t, prov)

	if resp, data := h.do("GET", "/session", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /session status = %d, want 200: %s", resp.StatusCode, data)
	}

	id := h.createSessionBody(map[string]any{})
	if resp, data := h.do("GET", "/session/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /session/%s status = %d, want 200: %s", id, resp.StatusCode, data)
	}
	if resp, data := h.do("GET", "/session/"+id+"/message", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /session/%s/message status = %d, want 200: %s", id, resp.StatusCode, data)
	}
}

// TestIsEmptySessionIDPath is the unit-level boundary check on the guard's
// predicate directly, independent of any HTTP plumbing.
func TestIsEmptySessionIDPath(t *testing.T) {
	wantTrue := []string{"/session//", "/session//abort", "/session//a/b"}
	for _, p := range wantTrue {
		if !isEmptySessionIDPath(p) {
			t.Errorf("isEmptySessionIDPath(%q) = false, want true", p)
		}
	}
	wantFalse := []string{"/session", "/session/", "/session/x", "/session/x/abort", "/health", "/", "/sessions//x"}
	for _, p := range wantFalse {
		if isEmptySessionIDPath(p) {
			t.Errorf("isEmptySessionIDPath(%q) = true, want false", p)
		}
	}
}
