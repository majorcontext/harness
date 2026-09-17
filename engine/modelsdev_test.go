package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

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

// parkedSuccessTransport parks each request until released, then answers
// with a fixed body.
type parkedSuccessTransport struct {
	arrived chan struct{}
	release chan struct{}
	body    string
}

func (p *parkedSuccessTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.arrived <- struct{}{}
	<-p.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(p.body)),
		Header:     make(http.Header),
	}, nil
}

// parkedRequest is one in-flight GET held by parkedRequestTransport.
type parkedRequest struct {
	release chan struct{}
	body    string
}

// parkedRequestTransport parks each request until that request's release
// channel closes, then answers with the body current when the request
// arrived. The body is swappable so one transport can serve two sources.
type parkedRequestTransport struct {
	mu      sync.Mutex
	body    string
	arrived chan *parkedRequest
}

func (p *parkedRequestTransport) setBody(body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.body = body
}

func (p *parkedRequestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	p.mu.Lock()
	body := p.body
	p.mu.Unlock()
	r := &parkedRequest{release: make(chan struct{}), body: body}
	p.arrived <- r
	<-r.release
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     make(http.Header),
	}, nil
}

type resolveOutcome struct {
	tokens int
	source string
	err    error
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

// The converged contract: the bare lookup NEVER waits — a lookup against a
// not-yet-populated snapshot is an ordinary miss, and the background
// refresher populates it off the lookup path. (resolveContextWindow's
// bounded wait for the initial fetch is pinned separately, above.)
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

// The first unknown-model lookup must not refuse while the initial fetch is
// still in flight: it waits for that fetch once, then resolves from the
// snapshot the fetch published.
func TestResolveContextWindowWaitsForInitialFetch(t *testing.T) {
	resetModelsDevSnapshot(t)
	stubContextWindowLookup(t, testContextWindowTable())

	fetch := &parkedSuccessTransport{
		arrived: make(chan struct{}, 1),
		release: make(chan struct{}),
		body:    `{"unknown-to-table":1000000}`,
	}
	swapModelsDevClient(t, fetch)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	startModelsDevRefreshForTest(t, ctx, "http://models.dev.invalid/windows")

	<-fetch.arrived // the initial fetch is parked mid-flight

	res := make(chan resolveOutcome, 1)
	go func() {
		tokens, source, err := resolveContextWindow(0, modelUnknown, true, true)
		res <- resolveOutcome{tokens, source, err}
	}()

	select {
	case got := <-res:
		t.Fatalf("resolve returned before the initial fetch completed: %+v", got)
	default:
	}
	close(fetch.release)
	got := <-res
	if got.err != nil {
		t.Fatalf("resolve after the initial fetch reported a miss: %v", got.err)
	}
	if got.tokens != 1_000_000 || got.source != contextWindowSourceModelsDev {
		t.Fatalf("resolve after the initial fetch = %d, %q; want 1000000, %q", got.tokens, got.source, contextWindowSourceModelsDev)
	}
}

// The refresher is single-flight: the hourly ticker must not issue a second
// GET while the initial fetch is still in flight. Runs in a synctest bubble
// so passing the tick is deterministic fake time.
func TestModelsDevTickerWaitsForInitialFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetModelsDevSnapshot(t)
		fetch := &parkedSuccessTransport{
			arrived: make(chan struct{}, 4),
			release: make(chan struct{}),
			body:    `{"gemini-3.8-flash":1048576}`,
		}
		swapModelsDevClient(t, fetch)
		ctx, cancel := context.WithCancel(context.Background())
		startModelsDevRefreshForTest(t, ctx, "http://models.dev.invalid/windows")

		<-fetch.arrived // the initial fetch is parked mid-flight
		synctest.Wait()
		time.Sleep(2 * modelsDevRefreshTTL) // fake clock passes the first tick
		synctest.Wait()

		secondGET := false
		select {
		case <-fetch.arrived:
			secondGET = true
		default:
		}

		close(fetch.release)
		synctest.Wait() // the fetch completes and the loop starts
		cancel()
		synctest.Wait() // the loop exits
		if secondGET {
			t.Fatal("ticker issued a second GET while the initial fetch was in flight")
		}
	})
}

// A lookup that started waiting on one generation must not let that
// generation's early close — a source swap cancelling it — answer for the
// replacement source: the wait re-targets the new generation's fetch.
// Runs in a synctest bubble; synctest.Wait settles each goroutine's parked
// state before the next transition, so "the waiter loaded generation A's
// channel" holds before the swap without racing goroutine startup.
func TestResolveContextWindowWaitsForSwappedSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetModelsDevSnapshot(t)
		stubContextWindowLookup(t, testContextWindowTable())
		fetches := &parkedRequestTransport{arrived: make(chan *parkedRequest, 2)}
		fetches.setBody(`{"other-model":1000}`) // source A must not answer
		swapModelsDevClient(t, fetches)

		SetModelsDevRefreshSource(context.Background(), "http://a.invalid/windows")
		a := <-fetches.arrived // A's initial fetch parked

		res := make(chan resolveOutcome, 1)
		go func() {
			tokens, source, err := resolveContextWindow(0, modelUnknown, true, true)
			res <- resolveOutcome{tokens, source, err}
		}()
		synctest.Wait() // the waiter is now parked on A's channel

		ctxB, cancelB := context.WithCancel(context.Background())
		fetches.setBody(`{"unknown-to-table":1000000}`)
		SetModelsDevRefreshSource(ctxB, "http://b.invalid/windows")
		b := <-fetches.arrived // B's initial fetch parked
		close(a.release)       // A's cancelled fetch returns; A's channel closes
		synctest.Wait()

		bad := ""
		select {
		case got := <-res:
			bad = fmt.Sprintf("resolve answered from the retired source's channel: %+v", got)
		default:
		}

		close(b.release)
		synctest.Wait()
		if bad == "" {
			got := <-res
			if got.err != nil {
				bad = fmt.Sprintf("resolve after source B's fetch reported a miss: %v", got.err)
			} else if got.tokens != 1_000_000 || got.source != contextWindowSourceModelsDev {
				bad = fmt.Sprintf("resolve after source B's fetch = %d, %q; want 1000000, %q", got.tokens, got.source, contextWindowSourceModelsDev)
			}
		}
		cancelB()
		synctest.Wait() // B's refresher loop exits
		if bad != "" {
			t.Fatal(bad)
		}
	})
}

// The snapshot lookup must key on modelmeta's full canonicalization, not
// just the last path segment: a Bifrost bedrock-mantle ref carries the
// dotted family (and possibly region) prefix and a bedrock version suffix
// ahead of the bare ID the snapshot keys on.
func TestModelsDevWindowLookupCanonicalKey(t *testing.T) {
	resetModelsDevSnapshot(t)
	modelsDevSnapshot.Store(&modelsDevWindows{windows: map[string]int{
		"claude-opus-5":    500_000,
		"gemini-3.8-flash": 1_000_000,
	}})
	cases := []struct {
		ref  message.ModelRef
		want int
	}{
		{message.ModelRef{Provider: "anthropic", Model: "bedrock_mantle/anthropic.claude-opus-5-v1:0"}, 500_000},
		{message.ModelRef{Provider: "anthropic", Model: "bedrock_mantle/us.anthropic.claude-opus-5-v1:0"}, 500_000},
		{message.ModelRef{Provider: "amazon-bedrock", Model: "anthropic.claude-opus-5-v1:0"}, 500_000},
		{message.ModelRef{Provider: "anthropic", Model: "anthropic/claude-opus-5-v1"}, 500_000},
		{message.ModelRef{Provider: "bifrost", Model: "vertex/gemini-3.8-flash"}, 1_000_000},
	}
	for _, tc := range cases {
		if tokens, ok := modelsDevWindowLookup(tc.ref); !ok || tokens != tc.want {
			t.Errorf("modelsDevWindowLookup(%s) = %d, %v; want %d, true", tc.ref, tokens, ok, tc.want)
		}
	}
	if _, ok := modelsDevWindowLookup(message.ModelRef{Provider: "anthropic", Model: "bedrock_mantle/anthropic.claude-opus-4-8"}); ok {
		t.Error("unkeyed bedrock ref resolved, want a miss")
	}
}

// Model checks and switches run under the session lock, so they must take
// a not-yet-populated snapshot as an ordinary miss: the bounded readiness
// wait belongs to session construction only. Runs in a synctest bubble so
// the no-wait assertion is exact — a waiting check can return only by
// burning the whole fetch deadline.
func TestModelCheckDoesNotWaitForInitialFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetModelsDevSnapshot(t)
		stubContextWindowLookup(t, testContextWindowTable())
		fetch := &parkedSuccessTransport{
			arrived: make(chan struct{}, 1),
			release: make(chan struct{}),
			body:    `{"switched":1000000}`,
		}
		swapModelsDevClient(t, fetch)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startModelsDevRefreshForTest(t, ctx, "http://models.dev.invalid/windows")

		<-fetch.arrived // the initial fetch is parked mid-flight

		cfg := requireCfg(&scriptedProvider{name: "test"}, modelKnownBig)
		cfg.ContextWindowFromModelsDev = true
		cfg.ContextWindowModelsDevURL = "http://models.dev.invalid/windows"
		cfg.Instructions = &InstructionsConfig{Disabled: true}
		s := NewSession(cfg)
		if err := s.ContextWindowErr(); err != nil {
			t.Fatalf("known model reported a context-window error: %v", err)
		}
		switched := message.ModelRef{Provider: "test", Model: "switched"}

		start := time.Now()
		checkErr := s.CheckModel(switched)
		s.SetModel(switched)
		switchErr := s.ContextWindowErr()
		elapsed := time.Since(start)
		close(fetch.release)
		cancel()
		synctest.Wait() // the refresher completes and its loop exits
		if !errors.Is(checkErr, ErrUnknownContextWindow) {
			t.Fatalf("CheckModel(switched) = %v, want the unknown-model refusal", checkErr)
		}
		if !errors.Is(switchErr, ErrUnknownContextWindow) {
			t.Fatalf("after SetModel, ContextWindowErr = %v, want the unknown-model refusal", switchErr)
		}
		if elapsed != 0 {
			t.Fatalf("model check/switch waited %v for the models.dev fetch; both must take the miss without waiting", elapsed)
		}
	})
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
