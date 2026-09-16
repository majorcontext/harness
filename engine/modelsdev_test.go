package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

func resetModelsDevSnapshot(t *testing.T) {
	t.Helper()
	modelsDevSnapshot.Store(nil)
	t.Cleanup(func() { modelsDevSnapshot.Store(nil) })
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
	orig := modelsDevHTTPClient
	rt := &countingRoundTripper{}
	modelsDevHTTPClient = &http.Client{Transport: rt}
	t.Cleanup(func() { modelsDevHTTPClient = orig })

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
	release := make(chan struct{})
	fetchDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Write([]byte(`{"gemini-3.8-flash":1048576}`))
		close(fetchDone)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startModelsDevRefreshForTest(t, ctx, srv.URL)

	// A lookup racing the in-flight initial fetch returns a miss at once
	// (bounded by the test's own patience, not the handler's hold).
	done := make(chan bool, 1)
	go func() {
		_, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"})
		done <- !ok
	}()
	select {
	case missed := <-done:
		if !missed {
			t.Fatal("lookup returned a hit against a snapshot the held-up fetch never populated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lookup blocked on the in-flight fetch; the request path must never wait")
	}
	close(release)
	<-fetchDone
}

// Setting an empty URL disables the source: the previous refresher is
// cancelled and the snapshot cleared.
func TestSetModelsDevRefreshSourceEmptyDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"gemini-3.8-flash":1048576}`))
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	SetModelsDevRefreshSource(ctx, srv.URL)
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"}); ok {
		t.Log("snapshot populated (timing-dependent); the disable assertion below is the point")
	}
	SetModelsDevRefreshSource(ctx, "")
	if modelsDevSnapshot.Load() != nil {
		t.Error("snapshot not cleared on disable")
	}
}
