package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/majorcontext/gatekeeper/proxy"
)

const (
	// userHeader carries the app user from the middleware to the resolver. The
	// resolver registers it as a strip header, so it never reaches the upstream.
	userHeader = "X-App-User"
	relayName  = "codex"
)

type config struct {
	// Upstream is the Codex backend that the relay forwards to.
	Upstream string
	// Issuer is the OAuth issuer. Empty: auth.openai.com.
	Issuer string
	// HTTP sends the requests to the issuer. nil: http.DefaultClient.
	HTTP *http.Client
	// Pages are served by the static handlers.
	WasmPath, WasmExecPath, EmbedDir string
	// SecureCookie sets Secure on the session cookie. Set it for any
	// deployment that is not on a loopback address.
	SecureCookie bool
}

type server struct {
	cfg      config
	sessions *sessions
	store    *tokenStore
	proxy    *proxy.Proxy
	flows    *flows
}

func newServer(cfg config) (*server, error) {
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	s := &server{cfg: cfg, sessions: newSessions(cfg.SecureCookie), store: newTokenStore(cfg.Issuer, cfg.HTTP), flows: &flows{m: map[string]*flow{}}}
	p := proxy.NewProxy()
	if err := p.AddRelay(relayName, cfg.Upstream); err != nil {
		return nil, err
	}
	// The relay target is https. gatekeeper refuses to inject a credential
	// over http unless the header allows it; only a test upstream is http.
	insecure := u.Scheme == "http"
	p.SetCredentialResolverWithStripHeaders(u.Hostname(),
		func(ctx context.Context, _, r *http.Request, _ string) ([]proxy.CredentialHeader, error) {
			user := r.Header.Get(userHeader)
			// The relay copies the headers after the resolver ran. Removing the
			// header here is what keeps it from the upstream; the strip-header
			// registration covers only a request that skips the resolver.
			r.Header.Del(userHeader)
			tok, err := s.store.Fresh(ctx, user)
			if err != nil {
				return nil, err
			}
			invalidate := func() { s.store.MarkStale(user) }
			return []proxy.CredentialHeader{
				{Name: "Authorization", Value: "Bearer " + tok.Access, Grant: "codex", Invalidate: invalidate, AllowInsecure: insecure},
				{Name: "ChatGPT-Account-ID", Value: tok.Account, Grant: "codex", Invalidate: invalidate, AllowInsecure: insecure},
			}, nil
		}, userHeader)
	s.proxy = p
	return s, nil
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(styleCSS)
	})
	mux.HandleFunc("GET /harness.wasm", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, s.cfg.WasmPath) })
	mux.HandleFunc("GET /wasm_exec.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, s.cfg.WasmExecPath) })
	mux.Handle("GET /embed/", http.StripPrefix("/embed/", http.FileServer(http.Dir(s.cfg.EmbedDir))))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /whoami", s.whoami)
	mux.HandleFunc("GET /settings/codex", s.settingsPage)
	mux.HandleFunc("POST /settings/codex/start", s.settingsStart)
	mux.HandleFunc("POST /settings/codex/disconnect", s.settingsDisconnect)
	mux.Handle("/model-proxy/", http.StripPrefix("/model-proxy", http.HandlerFunc(s.modelProxy)))
	return mux
}

// strippedHeaders never reach the upstream. The relay copies every other
// request header, so the middleware must remove what the page sends.
var strippedHeaders = []string{"Cookie", "Origin", "Referer", "Authorization", "Proxy-Authorization", "X-Api-Key", "ChatGPT-Account-ID", userHeader}

// modelProxy is the only door to the gatekeeper relay. The proxy has no
// listener of its own and no authentication: this function is its
// authentication. Never mount the proxy where a request skips this function.
func (s *server) modelProxy(w http.ResponseWriter, r *http.Request) {
	// A gatekeeper Proxy is a forward proxy. It takes the relay branch only
	// when r.URL.Host is empty; an absolute-form request (or CONNECT) would go
	// to the host it names, with the credential of the resolver. Refuse them,
	// and rebuild the URL from the path and the query alone.
	if r.URL.Host != "" || r.URL.Scheme != "" || r.Method == http.MethodConnect {
		http.Error(w, "bad request target", http.StatusBadRequest)
		return
	}
	r.URL = &url.URL{Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	r.RequestURI = ""
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		r.Host = addr.String()
	} else {
		r.Host = ""
	}
	_, sess := s.sessions.sessionOf(r)
	if sess == nil {
		unauthorized(w, "sign in to the app")
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	// Only the codex relay is reachable. path.Clean stops dot segments.
	if clean := path.Clean(r.URL.Path); clean != r.URL.Path || !strings.HasPrefix(clean, "/relay/"+relayName+"/") {
		http.NotFound(w, r)
		return
	}
	// Refresh happens here, so a login that the issuer rejected answers 401
	// (the module maps it to login_required). The relay cannot send a reliable
	// 401 for a missing credential: it forwards the request with none.
	switch _, err := s.store.Fresh(r.Context(), sess.user); {
	case errors.Is(err, errNoLogin):
		unauthorized(w, "not signed in to Codex")
		return
	case err != nil:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "the sign-in could not be refreshed now", http.StatusServiceUnavailable)
		return
	}
	for _, h := range strippedHeaders {
		r.Header.Del(h)
	}
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "sec-") {
			r.Header.Del(k)
		}
	}
	r.Header.Set(userHeader, sess.user)
	s.proxy.ServeHTTP(w, r)
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="harness"`)
	http.Error(w, msg, http.StatusUnauthorized)
}

// sameOrigin rejects a request that a browser marks as cross-site. A client
// that sends no Sec-Fetch-Site is not a browser; the SameSite cookie already
// keeps a cross-site page from sending one with the session.
func sameOrigin(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" || site == "none"
}

// flow is one device code sign-in, bound to the session that started it.
type flow struct {
	code   *deviceCode
	err    string
	done   bool
	cancel context.CancelFunc
}

type flows struct {
	mu sync.Mutex
	m  map[string]*flow // by session id
}

// view returns a copy of the flow of the session.
func (f *flows) view(id string) flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl := f.m[id]; fl != nil {
		return flow{code: fl.code, err: fl.err, done: fl.done}
	}
	return flow{}
}

// take is view, and it forgets a flow that failed, so the page shows the
// error once and the map holds no finished flow.
func (f *flows) take(id string) flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl := f.m[id]
	if fl == nil {
		return flow{}
	}
	if fl.done {
		delete(f.m, id)
	}
	return flow{code: fl.code, err: fl.err, done: fl.done}
}

// drop cancels and forgets the flow of the session.
func (f *flows) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl := f.m[id]; fl != nil && fl.cancel != nil {
		fl.cancel()
	}
	delete(f.m, id)
}

// csrfOK checks the token of the form against the session, and the origin.
func csrfOK(r *http.Request, sess *session) bool {
	got := r.PostFormValue("csrf")
	return sameOrigin(r) && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}
