package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// recordPath streams one trivial request through a client and reports the
// request path the server actually saw. The handler answers with a minimal
// completed-response SSE body so Stream returns without error; the test only
// cares about the URL the client chose.
func recordPath(t *testing.T, c *Client) string {
	t.Helper()
	var got string
	handler := func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sse("response.completed", `{"type":"response.completed","response":{"id":"resp_1"}}`)) //nolint:errcheck
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	c.BaseURL = srv.URL
	if c.APIKey == "" {
		c.APIKey = "test-key"
	}
	s, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: Family, Model: "gpt-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()
	collect(t, s)
	return got
}

// TestResponsesPathDefault pins the pre-existing wire path for every caller
// that never sets ResponsesPath: the empty value must keep POSTing to
// <base>/v1/responses, byte-identical to the hardcoded path it replaces.
// This is the "changed nothing for anyone" half of the ResponsesPath field.
func TestResponsesPathDefault(t *testing.T) {
	if got := recordPath(t, &Client{}); got != "/v1/responses" {
		t.Errorf("path = %q, want %q", got, "/v1/responses")
	}
}

// TestResponsesPathCustom is the reason the field exists: a Responses-API
// compatible endpoint need not live at /v1/responses. A deployment routing
// one model family at a vendor endpoint with its own path must be able to
// say so without the adapter appending its own suffix.
func TestResponsesPathCustom(t *testing.T) {
	c := &Client{ResponsesPath: "/alt/responses"}
	if got := recordPath(t, c); got != "/alt/responses" {
		t.Errorf("path = %q, want %q", got, "/alt/responses")
	}
}

// TestClientFamilyDefaultsToPackageFamily proves the Family seam is
// backward compatible: a Client that names no family still reports the
// package constant, so every existing caller's ModelRef.Provider and
// ProviderData tag are unchanged.
func TestClientFamilyDefaultsToPackageFamily(t *testing.T) {
	c := &Client{}
	if got := c.Name(); got != Family {
		t.Errorf("Name() = %q, want %q", got, Family)
	}
}

// TestClientFamilyOverrideTagsReasoning is the seam a SECOND native
// Responses provider needs. Two such providers can be configured at once,
// pointing at different endpoints, and a session may swap between them.
// Encrypted reasoning items are endpoint-scoped opaque state, so each
// client must tag (and later replay) them under ITS OWN family — the
// canonical cross-provider drop rule — rather than under a shared package
// constant that would replay one endpoint's items to the other.
func TestClientFamilyOverrideTagsReasoning(t *testing.T) {
	c := &Client{Family: "secondary", APIKey: "test-key"}
	if got := c.Name(); got != "secondary" {
		t.Errorf("Name() = %q, want %q", got, "secondary")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, streamFixture) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	c.BaseURL = srv.URL

	s, err := c.Stream(context.Background(), &provider.Request{
		Model:    message.ModelRef{Provider: "secondary", Model: "gpt-5"},
		Messages: []message.Message{{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer s.Close()

	var done *provider.Event
	for _, ev := range collect(t, s) {
		if ev.Type == provider.EventDone {
			done = &ev
		}
	}
	if done == nil || done.Message == nil {
		t.Fatal("no done event with an assembled message")
	}
	var reasoning *message.Reasoning
	for _, p := range done.Message.Parts {
		if r, ok := p.(*message.Reasoning); ok {
			reasoning = r
		}
	}
	if reasoning == nil {
		t.Fatal("assembled message has no Reasoning part")
	}
	if _, ok := reasoning.ProviderData.Get("secondary"); !ok {
		t.Errorf("ProviderData keys = %v, want the client's own family %q", keysOf(reasoning.ProviderData), "secondary")
	}
	if _, ok := reasoning.ProviderData.Get(Family); ok {
		t.Errorf("ProviderData is tagged %q; a keyed client must not tag under the package constant", Family)
	}
}

func keysOf(pd message.ProviderData) []string {
	out := make([]string, 0, len(pd))
	for k := range pd {
		out = append(out, k)
	}
	return out
}

// TestResponsesPathNoLeadingSlash is the malformed-URL case. ResponsesPath
// is caller-supplied configuration, so "backend/responses" is a typo an
// operator will eventually write. Concatenated naively onto a base with no
// trailing slash it yields "https://hostbackend/responses" — a request to a
// DIFFERENT HOST, not merely a wrong path, which is a far worse failure
// than the typo deserves. The adapter absorbs it.
func TestResponsesPathNoLeadingSlash(t *testing.T) {
	c := &Client{ResponsesPath: "alt/responses"}
	if got := recordPath(t, c); got != "/alt/responses" {
		t.Errorf("path = %q, want %q", got, "/alt/responses")
	}
}

// TestResponsesURLNormalization pins the joining rule itself, including the
// mirror-image typo (a trailing slash on the base) that would otherwise
// produce a doubled separator.
func TestResponsesURLNormalization(t *testing.T) {
	tests := []struct {
		name string
		base string
		path string
		want string
	}{
		{name: "default path", base: "https://api.example.test", want: "https://api.example.test/v1/responses"},
		{name: "leading slash", base: "https://api.example.test", path: "/backend/responses", want: "https://api.example.test/backend/responses"},
		{name: "no leading slash", base: "https://api.example.test", path: "backend/responses", want: "https://api.example.test/backend/responses"},
		{name: "trailing slash on base", base: "https://api.example.test/", path: "/backend/responses", want: "https://api.example.test/backend/responses"},
		{name: "both typos at once", base: "https://api.example.test/", path: "backend/responses", want: "https://api.example.test/backend/responses"},
		{name: "base carries a path segment", base: "https://api.example.test/alt-api/v2", path: "/responses", want: "https://api.example.test/alt-api/v2/responses"},
		{name: "empty base uses the default", base: "", path: "/responses", want: defaultBaseURL + "/responses"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responsesURL(tt.base, tt.path); got != tt.want {
				t.Errorf("responsesURL(%q, %q) = %q, want %q", tt.base, tt.path, got, tt.want)
			}
		})
	}
}
