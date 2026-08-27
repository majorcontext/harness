package server

import (
	"net/http"
	"net/http/pprof"
)

// registerPProf adds the standard profiling routes to mux, each wrapped by
// auth. Options.PProf gates the call.
//
// Importing net/http/pprof also registers these handlers on
// http.DefaultServeMux as an import side effect. That mux is never served
// by this binary — cmd/harness/main.go passes the *Server itself to
// http.Server.Handler — so the only reachable profiling surface is the one
// registered here, authed, and only when a caller opts in.
//
// The four named profiles are registered one by one rather than left to
// pprof.Index's own path parsing, so the route table names every path this
// server answers. Index still serves the HTML listing at the collection
// path, and any other named profile through it.
func registerPProf(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /debug/pprof/", auth(pprof.Index))
	mux.HandleFunc("GET /debug/pprof/cmdline", auth(pprof.Cmdline))
	mux.HandleFunc("GET /debug/pprof/profile", auth(pprof.Profile))
	mux.HandleFunc("GET /debug/pprof/symbol", auth(pprof.Symbol))
	mux.HandleFunc("GET /debug/pprof/trace", auth(pprof.Trace))
}
