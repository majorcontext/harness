// Package injectedauth owns the login state of the provider entries with auth "injected": a server in front of the
// endpoint holds the credential and adds it, and Harness sends none. A Logins value, made by the composition root of
// a connection, sends the status probe, keeps its answer, and turns a 401 of a model call into the error of the turn.
package injectedauth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
)

const (
	// cacheTTL is how long the answer of a probe stands.
	cacheTTL = 30 * time.Second
	// recentOK is how long after a probe that answered 200 a 401 of a model call is a fault of the server.
	recentOK = 2 * time.Minute
	// probeTimeout bounds one probe. A probe that takes longer is unknown.
	probeTimeout = 3 * time.Second
	maxBody      = 1 << 16
	// maxCatalog bounds the body that a probe keeps for the catalog fetch.
	maxCatalog = 1 << 20
)

// State is the answer of the status probe.
type State int

// The states of a probe.
const (
	// Unknown is an answer that says nothing: a failure, a timeout, or a status other than 200, 401, 404, and 405.
	Unknown State = iota
	SignedIn
	SignedOut
	// NoProbe is a 404 or 405: the server has no status probe, so only the 401 of a model call says that the user is signed out.
	NoProbe
)

// Probe is the status probe of an entry: the model list request and the headers its wire needs besides the extra headers of the entry.
type Probe struct {
	URL     string
	Headers map[string]string
	// Catalog says that the probe is also the request of the model catalog of the entry. The body of a probe that answers 200
	// then stays for cacheTTL, and the next request for the URL takes it once.
	Catalog bool
}

// Options is what the composition root gives a Logins.
type Options struct {
	// Transport returns the transport of the model calls of a provider. The probe uses it, so a probe reaches the server as a
	// model call does. nil, or a nil result: the default transport.
	Transport func(provider string) http.RoundTripper
	// Probe returns the status probe of a provider entry. It is the one place that knows the wire of the entry.
	Probe func(name string, p config.Provider) (Probe, bool)
	// Now returns the time. nil: time.Now.
	Now func() time.Time
}

// Logins owns the login state of the injected entries of one connection. The backend and the ACP logins of the connection
// share it.
type Logins struct {
	opts Options

	mu      sync.Mutex
	entries map[string]*Entry
}

// New returns a Logins over opts.
func New(opts Options) *Logins {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Logins{opts: opts, entries: map[string]*Entry{}}
}

// Now returns the time of the owner.
func (l *Logins) Now() time.Time { return l.opts.Now() }

// Of returns the Entry of the providers entry name, and nil for an entry that does not use injected auth.
func (l *Logins) Of(name string, p config.Provider) *Entry {
	if l == nil || !p.UsesInjectedAuth() || l.opts.Probe == nil {
		return nil
	}
	probe, ok := l.opts.Probe(name, p)
	if !ok {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := name + "\x00" + probe.URL
	e := l.entries[key]
	if e == nil {
		e = &Entry{logins: l, provider: name, loginURL: p.LoginURL, probe: probe}
		l.entries[key] = e
	}
	return e
}

// Entry is the login state of one provider entry.
type Entry struct {
	logins   *Logins
	provider string
	loginURL string
	probe    Probe

	mu            sync.Mutex
	state         State
	at            time.Time
	okAt          time.Time
	modelRejected bool
	generation    uint64
	body          []byte
	bodyAt        time.Time
}

// LoginURL is the sign-in page of the entry, or empty.
func (e *Entry) LoginURL() string { return e.loginURL }

// Message is the text of a login-required error. It holds the sign-in URL and no header value.
func (e *Entry) Message() string {
	msg := "not logged in to " + e.provider
	if e.loginURL != "" {
		msg += "; sign in at " + e.loginURL
	}
	return msg
}

// Generation changes each time the entry becomes signed in.
func (e *Entry) Generation() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.generation
}

// Reset forgets the cached answer. A new sign-in starts with it.
func (e *Entry) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state, e.at, e.okAt, e.modelRejected, e.body = Unknown, time.Time{}, time.Time{}, false, nil
}

// Cached returns the answer of the last probe while it stands. An entry whose model calls the server rejected after a
// sign-in check is signed in until the next Reset.
func (e *Entry) Cached() (State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.modelRejected:
		return SignedIn, true
	case e.state != Unknown && e.logins.Now().Sub(e.at) < cacheTTL:
		return e.state, true
	}
	return Unknown, false
}

// record keeps the answer of a probe. body is what a probe that answered 200 read in full for the catalog fetch, else nil.
func (e *Entry) record(status int, body []byte) State {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.logins.Now()
	e.body, e.bodyAt = nil, now
	if status == http.StatusOK {
		e.body = body
	}
	switch status {
	case http.StatusOK:
		if e.state != SignedIn {
			e.generation++
		}
		e.state, e.at, e.okAt = SignedIn, now, now
	case http.StatusUnauthorized:
		e.state, e.at, e.okAt = SignedOut, now, time.Time{}
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		e.state, e.at, e.okAt = NoProbe, now, time.Time{}
	default:
		return Unknown
	}
	return e.state
}

// Probe sends the status probe through the transport of the entry with the extra headers of the entry and no credential,
// and records the answer. It follows no redirect. A failure and a wait of more than three seconds are Unknown.
func (e *Entry) Probe(ctx context.Context, extra map[string]string) State {
	rt := http.DefaultTransport
	if f := e.logins.opts.Transport; f != nil {
		if t := f(e.provider); t != nil {
			rt = t
		}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.probe.URL, nil)
	if err != nil {
		return Unknown
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	for k, v := range e.probe.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return Unknown
	}
	var body []byte
	if e.probe.Catalog && resp.StatusCode == http.StatusOK {
		var err error
		if body, err = io.ReadAll(io.LimitReader(resp.Body, maxCatalog+1)); err != nil || len(body) > maxCatalog {
			body = nil
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	}
	_ = resp.Body.Close()
	return e.record(resp.StatusCode, body)
}

// takeBody returns the body that a probe kept while it stands, and forgets it.
func (e *Entry) takeBody() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	body := e.body
	e.body = nil
	if e.logins.Now().Sub(e.bodyAt) >= cacheTTL {
		return nil
	}
	return body
}

// Client returns a client over hc, or over the default transport when hc is nil, that treats a 401 of a model call as the
// end of the login. The client follows no redirect, so a 3xx answer ends the request.
func (e *Entry) Client(hc *http.Client) *http.Client {
	out := &http.Client{}
	base := http.DefaultTransport
	if hc != nil {
		out = &http.Client{Jar: hc.Jar, Timeout: hc.Timeout}
		if hc.Transport != nil {
			base = hc.Transport
		}
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	out.Transport = &transport{entry: e, base: base}
	return out
}

type transport struct {
	entry *Entry
	base  http.RoundTripper
}

// RoundTrip implements http.RoundTripper. A request for the model list is a probe: its answer is recorded and returned. A
// catalog entry answers it from the body of a fresh probe.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && t.entry.probe.Catalog && req.URL.String() == t.entry.probe.URL {
		if body := t.entry.takeBody(); body != nil {
			return &http.Response{
				Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
				Header:        http.Header{"Content-Type": {"application/json"}},
				Body:          io.NopCloser(bytes.NewReader(body)),
				ContentLength: int64(len(body)),
				Request:       req,
			}, nil
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if req.Method == http.MethodGet && req.URL.String() == t.entry.probe.URL {
		t.entry.record(resp.StatusCode, nil)
		return resp, nil
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	return nil, t.entry.rejection()
}

// rejection is the error of a model call that the server answered with 401.
func (e *Entry) rejection() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.logins.Now()
	e.body = nil
	if !e.okAt.IsZero() && now.Sub(e.okAt) < recentOK {
		e.modelRejected = true
		msg := "the server accepted the sign-in check but rejected the model call; "
		if e.loginURL != "" {
			msg += "sign in again at " + e.loginURL + ", or "
		}
		return &ServerError{msg + "check how the server authorizes model calls"}
	}
	e.state, e.at, e.okAt = SignedOut, now, time.Time{}
	return &LoginRequiredError{Text: e.Message()}
}

// LoginRequiredError says that the server has no credential for the user. A retry cannot resolve it.
type LoginRequiredError struct{ Text string }

func (e *LoginRequiredError) Error() string { return e.Text }

// Permanent marks the error for provider.Do: it is not sent again.
func (e *LoginRequiredError) Permanent() bool { return true }

// LoginRequired marks the error for the turn: only a new login resolves it.
func (e *LoginRequiredError) LoginRequired() bool { return true }

// ServerError says that the server answered the sign-in check and then rejected the model call. A login does not resolve it.
type ServerError struct{ Text string }

func (e *ServerError) Error() string { return strings.TrimSpace(e.Text) }

// Permanent marks the error for provider.Do: it is not sent again.
func (e *ServerError) Permanent() bool { return true }
