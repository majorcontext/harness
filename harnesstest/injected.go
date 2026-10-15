package harnesstest

import (
	"cmp"
	"net/http"
	"strings"
	"sync"
)

// InjectedRequest is one request that an injecting Server saw: the method, the path, and the credential headers, by
// lower-case name.
type InjectedRequest struct {
	Method, Path      string
	CredentialHeaders map[string]string
	// Version is the anthropic-version header.
	Version string
}

type injectedState struct {
	mu       sync.Mutex
	opts     InjectedOptions
	signedIn bool
	seen     []InjectedRequest
}

// Inject makes s a credential-injecting server in front of the backend, as InjectedOptions says. It answers GET /v1/models
// as the status probe of the Anthropic wire. Call it before the first request.
func (s *Server) Inject(opts InjectedOptions) {
	s.inj = &injectedState{opts: opts, signedIn: !opts.SignedOut}
}

// SignIn makes an injecting Server accept requests.
func (s *Server) SignIn() {
	s.inj.mu.Lock()
	defer s.inj.mu.Unlock()
	s.inj.signedIn = true
}

// InjectedRequests returns the requests that an injecting Server saw, in order.
func (s *Server) InjectedRequests() []InjectedRequest {
	s.inj.mu.Lock()
	defer s.inj.mu.Unlock()
	return append([]InjectedRequest(nil), s.inj.seen...)
}

// injectedServe records r and answers it when the server denies it or when it is the status probe. It reports whether it answered.
func (s *Server) injectedServe(w http.ResponseWriter, r *http.Request) bool {
	st := s.inj
	rec := InjectedRequest{Method: r.Method, Path: r.URL.Path, Version: r.Header.Get("anthropic-version")}
	for _, name := range []string{"authorization", "x-api-key", "chatgpt-account-id", "originator"} {
		if v := r.Header.Get(name); v != "" {
			if rec.CredentialHeaders == nil {
				rec.CredentialHeaders = map[string]string{}
			}
			rec.CredentialHeaders[name] = v
		}
	}
	probe := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models")
	st.mu.Lock()
	st.seen = append(st.seen, rec)
	denied := !st.signedIn && !(probe && st.opts.WeakProbe)
	st.mu.Unlock()
	switch {
	case probe && st.opts.ProbeStatus != 0:
		s.wire().writeError(w, st.opts.ProbeStatus, "harnesstest: model list unavailable")
		return true
	case denied:
		status := cmp.Or(st.opts.DenyStatus, http.StatusUnauthorized)
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="harness"`)
		}
		s.wire().writeError(w, status, "harnesstest: no credential for this caller")
		return true
	case probe:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
		return true
	}
	return false
}
