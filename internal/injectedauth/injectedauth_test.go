package injectedauth_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/injectedauth"
)

const catalogURL = "https://models.example/models"

type cutBody struct{ read bool }

func (b *cutBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, errors.New("cut")
	}
	b.read = true
	return copy(p, `{"models":[`), nil
}

func (*cutBody) Close() error { return nil }

type fakeServer struct {
	status int
	cut    bool
	calls  int
}

func (f *fakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	f.calls++
	body := io.NopCloser(strings.NewReader(`{"models":[]}`))
	if f.cut {
		body = &cutBody{}
	}
	return &http.Response{StatusCode: f.status, Body: body, Header: http.Header{}, Request: req}, nil
}

func catalogEntry(srv *fakeServer) *injectedauth.Entry {
	l := injectedauth.New(injectedauth.Options{
		Transport: func(string) http.RoundTripper { return srv },
		Probe: func(string, config.Provider) (injectedauth.Probe, bool) {
			return injectedauth.Probe{URL: catalogURL, Catalog: true}, true
		},
	})
	return l.Of("p", config.Provider{Auth: config.AuthInjected})
}

func fetch(t *testing.T, e *injectedauth.Entry, srv *fakeServer) int {
	t.Helper()
	before := srv.calls
	resp, err := e.Client(&http.Client{Transport: srv}).Get(catalogURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return srv.calls - before
}

func TestCatalogFetchTakesTheBodyOfAFreshProbeOnce(t *testing.T) {
	srv := &fakeServer{status: 200}
	e := catalogEntry(srv)
	e.Probe(context.Background(), nil)
	if n := fetch(t, e, srv); n != 0 {
		t.Errorf("fetch after a probe sent %d requests, want 0", n)
	}
	if n := fetch(t, e, srv); n != 1 {
		t.Errorf("second fetch sent %d requests, want 1", n)
	}
}

func TestCatalogFetchGoesLiveAfterAModelCallIsRejected(t *testing.T) {
	srv := &fakeServer{status: 200}
	e := catalogEntry(srv)
	e.Probe(context.Background(), nil)
	srv.status = 401
	if resp, err := e.Client(&http.Client{Transport: srv}).Get("https://models.example/responses"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a 401 of a model call returned no error")
	}
	srv.status = 200
	if n := fetch(t, e, srv); n != 1 {
		t.Errorf("fetch after a rejection sent %d requests, want 1", n)
	}
}

func TestCatalogFetchGoesLiveAfterAReset(t *testing.T) {
	srv := &fakeServer{status: 200}
	e := catalogEntry(srv)
	e.Probe(context.Background(), nil)
	e.Reset()
	if n := fetch(t, e, srv); n != 1 {
		t.Errorf("fetch after a reset sent %d requests, want 1", n)
	}
}

func TestCatalogFetchGoesLiveAfterATruncatedProbeBody(t *testing.T) {
	srv := &fakeServer{status: 200, cut: true}
	e := catalogEntry(srv)
	e.Probe(context.Background(), nil)
	srv.cut = false
	if n := fetch(t, e, srv); n != 1 {
		t.Errorf("fetch after a cut probe sent %d requests, want 1", n)
	}
}
