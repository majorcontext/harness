package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPProf_NotRegisteredOnDefaultServeMux is the server-package guard's
// twin, at the binary's own import graph. The server test proves only that
// nothing on THAT package's graph imports net/http/pprof; this binary links
// far more (the engine, providers, plugins, MCP, the hub, every tool), and
// any one of those pulling in net/http/pprof would publish /debug/pprof/*
// on http.DefaultServeMux for the whole process, outside Options.PProf.
//
// serveCmd sets http.Server.Handler explicitly, so the default mux is not
// served today and this is defense in depth against that changing. It costs
// one map lookup per path.
func TestPProf_NotRegisteredOnDefaultServeMux(t *testing.T) {
	for _, path := range []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/profile",
		"/debug/pprof/cmdline",
		"/debug/pprof/trace",
		"/debug/pprof/symbol",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, pattern := http.DefaultServeMux.Handler(req); pattern != "" {
			t.Errorf("%s is registered on http.DefaultServeMux as %q; some package this binary links imports net/http/pprof", path, pattern)
		}
	}
}
