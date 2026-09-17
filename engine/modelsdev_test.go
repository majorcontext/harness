package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness/message"
)

func resetModelsDevSnapshot(t *testing.T) {
	t.Helper()
	modelsDevSnapshot.Store(nil)
	t.Cleanup(func() { modelsDevSnapshot.Store(nil) })
}

func swapModelsDevClient(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	orig := modelsDevHTTPClient
	modelsDevHTTPClient = &http.Client{Transport: rt}
	t.Cleanup(func() { modelsDevHTTPClient = orig })
}

// staticTransport answers every request with a fixed body and ignores the
// request context.
type staticTransport struct {
	body string
}

func (s staticTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

// parkedFetchTransport reports the in-flight request's context and parks
// until released, holding the fetch open mid-flight.
type parkedFetchTransport struct {
	fetchCtx chan context.Context
	release  chan struct{}
}

func (p *parkedFetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p.fetchCtx <- req.Context()
	<-p.release
	return nil, http.ErrHandlerTimeout
}

func TestModelsDevRefreshPopulatesSnapshot(t *testing.T) {
	resetModelsDevSnapshot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"gemini-3-flash": 1000000}`))
	}))
	t.Cleanup(srv.Close)

	if err := refreshModelsDevWindows(context.Background(), srv.URL); err != nil {
		t.Fatalf("refreshModelsDevWindows: %v", err)
	}
	ref := message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3-flash"}
	tokens, ok := modelsDevWindowLookup(ref)
	if !ok || tokens != 1_000_000 {
		t.Fatalf("modelsDevWindowLookup(%s) = %d, %v; want 1000000, true", ref, tokens, ok)
	}
}

func TestModelsDevRefreshKeepsLastGoodOnFailure(t *testing.T) {
	resetModelsDevSnapshot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"gemini-3-flash": 1000000}`))
	}))
	t.Cleanup(srv.Close)

	if err := refreshModelsDevWindows(context.Background(), srv.URL); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	srv.Close()

	if err := refreshModelsDevWindows(context.Background(), srv.URL); err == nil {
		t.Fatal("refresh against a closed server succeeded, want an error")
	}
	if tokens, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3-flash"}); !ok || tokens != 1_000_000 {
		t.Fatalf("last-good snapshot lost after a failed refresh: %d, %v", tokens, ok)
	}
}

func TestModelsDevWindowLookupEmptySnapshotMiss(t *testing.T) {
	resetModelsDevSnapshot(t)
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3-flash"}); ok {
		t.Fatal("empty snapshot resolved a window, want a miss")
	}
}

type countingRoundTripper struct {
	hits atomic.Int32
}

func (c *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	c.hits.Add(1)
	return nil, http.ErrHandlerTimeout
}

// TestModelsDevWindowLookupDoesNoIO proves the session path reads only the
// in-memory snapshot: with the HTTP client replaced by one that fails any
// call, a hit still resolves and a miss still returns without touching it.
func TestModelsDevWindowLookupDoesNoIO(t *testing.T) {
	resetModelsDevSnapshot(t)
	rt := &countingRoundTripper{}
	swapModelsDevClient(t, rt)

	modelsDevSnapshot.Store(&modelsDevWindows{windows: map[string]int{"gemini-3-flash": 1_000_000}})
	if tokens, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3-flash"}); !ok || tokens != 1_000_000 {
		t.Fatalf("snapshot hit = %d, %v; want 1000000, true", tokens, ok)
	}
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/absent"}); ok {
		t.Fatal("absent model resolved, want a miss")
	}
	if got := rt.hits.Load(); got != 0 {
		t.Fatalf("HTTP client called %d times, want 0 (the session path must not do I/O)", got)
	}
}

func TestFetchModelsDevWindowsRejectsOversizedBody(t *testing.T) {
	resetModelsDevSnapshot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, modelsDevMaxBodyBytes+1))
	}))
	t.Cleanup(srv.Close)

	if err := refreshModelsDevWindows(context.Background(), srv.URL); err == nil {
		t.Fatal("oversized body was accepted")
	}
}

func TestConfigModelsDevEnabled(t *testing.T) {
	if (Config{ContextWindowFromModelsDev: true}).modelsDevEnabled() {
		t.Fatal("true bool with empty url must be off")
	}
	if (Config{ContextWindowModelsDevURL: "https://control.example/windows"}).modelsDevEnabled() {
		t.Fatal("url with false bool must be off")
	}
	if !(Config{ContextWindowFromModelsDev: true, ContextWindowModelsDevURL: "https://control.example/windows"}).modelsDevEnabled() {
		t.Fatal("true bool with url must be on")
	}
}

// A null body must fail the refresh, not store an empty snapshot over the
// last-good one.
func TestModelsDevRefreshRejectsNullSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("null"))
	}))
	t.Cleanup(srv.Close)
	if err := refreshModelsDevWindows(context.Background(), srv.URL); err == nil {
		t.Fatal("null snapshot was accepted")
	}
}

// A null member must fail the whole fetch: it would otherwise decode as a
// zero window and masquerade as a below-floor value instead of a miss.
func TestModelsDevFetchRejectsNullMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"gemini-3.8-flash":null}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := fetchModelsDevWindows(context.Background(), srv.URL); err == nil {
		t.Fatal("null member was accepted")
	}
}

func startModelsDevRefreshForTest(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	startModelsDevRefresh(ctx, url)
}

// The converged contract: the request path NEVER waits — a lookup against a
// not-yet-populated snapshot is an ordinary miss, and the background
// refresher populates it off the request path.
func TestModelsDevLookupNeverWaitsForFirstFetch(t *testing.T) {
	resetModelsDevSnapshot(t)
	fetch := &parkedFetchTransport{
		fetchCtx: make(chan context.Context, 1),
		release:  make(chan struct{}),
	}
	swapModelsDevClient(t, fetch)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startModelsDevRefreshForTest(t, ctx, "http://models.dev.invalid/windows")

	<-fetch.fetchCtx // the initial fetch is parked mid-flight

	// Called on the test goroutine: a lookup that waited on the in-flight
	// fetch would block here, before release.
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"}); ok {
		t.Fatal("lookup returned a hit against a snapshot the held-up fetch never populated")
	}
	close(fetch.release)
}

// A fetch that outlived its source must not publish: the transport returns
// a complete response even though the context is already cancelled.
func TestModelsDevRefreshDoesNotStoreAfterCancel(t *testing.T) {
	resetModelsDevSnapshot(t)
	swapModelsDevClient(t, staticTransport{body: `{"gemini-3.8-flash":1048576}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := refreshModelsDevWindows(ctx, "http://models.dev.invalid/windows"); err == nil {
		t.Fatal("refresh with a cancelled context succeeded")
	}
	if modelsDevSnapshot.Load() != nil {
		t.Fatal("refresh with a cancelled context published a snapshot")
	}
}

// Setting an empty URL disables the source: the previous refresher is
// cancelled and the snapshot cleared.
func TestSetModelsDevRefreshSourceEmptyDisables(t *testing.T) {
	resetModelsDevSnapshot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"gemini-3.8-flash":1048576}`))
	}))
	t.Cleanup(srv.Close)
	if err := refreshModelsDevWindows(context.Background(), srv.URL); err != nil {
		t.Fatalf("populate snapshot: %v", err)
	}
	SetModelsDevRefreshSource(context.Background(), "")
	if modelsDevSnapshot.Load() != nil {
		t.Fatal("empty URL did not clear the snapshot")
	}
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"}); ok {
		t.Fatal("lookup resolved a window after the source was disabled")
	}
}

// A source swap must not serve the previous source's snapshot: until the
// new URL's first fetch succeeds, lookups miss instead of resolving the
// old source's entries.
func TestSetModelsDevRefreshSourceSwapClearsSnapshot(t *testing.T) {
	resetModelsDevSnapshot(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"gemini-3.8-flash":1048576}`))
	}))
	t.Cleanup(srv.Close)
	if err := refreshModelsDevWindows(context.Background(), srv.URL); err != nil {
		t.Fatalf("populate snapshot: %v", err)
	}

	fetch := &parkedFetchTransport{
		fetchCtx: make(chan context.Context, 1),
		release:  make(chan struct{}),
	}
	swapModelsDevClient(t, fetch)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	SetModelsDevRefreshSource(ctx, "http://other.invalid/windows")

	<-fetch.fetchCtx // the new source's initial fetch is parked mid-flight
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"}); ok {
		t.Fatal("source swap still served the previous source's snapshot")
	}
	close(fetch.release)
}

func TestSetModelsDevRefreshSourceEmptyCancelsRefresher(t *testing.T) {
	resetModelsDevSnapshot(t)
	fetch := &parkedFetchTransport{
		fetchCtx: make(chan context.Context, 1),
		release:  make(chan struct{}),
	}
	swapModelsDevClient(t, fetch)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	SetModelsDevRefreshSource(ctx, "http://models.dev.invalid/windows")

	fetchCtx := <-fetch.fetchCtx // the initial fetch is parked mid-flight
	SetModelsDevRefreshSource(context.Background(), "")
	select {
	case <-fetchCtx.Done():
	default:
		t.Fatal("empty URL did not cancel the previous refresher")
	}
	close(fetch.release)
}

func TestModelsDevErrTextStripsURL(t *testing.T) {
	const src = "https://control.example/windows?token=abc"
	err := &url.Error{Op: "Get", URL: src, Err: errors.New("boom")}
	text := modelsDevErrText(err, src)
	if strings.Contains(text, "token=abc") {
		t.Fatalf("error text leaked the URL query: %s", text)
	}
	if !strings.Contains(text, "boom") {
		t.Fatalf("error text lost the cause: %s", text)
	}
}
