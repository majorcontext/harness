// Package injectedauth owns the state of the login of a provider entry with auth "injected": a server in front of the
// endpoint holds the credential and adds it, and Harness sends none. It sends the status probe, keeps its answer, and
// turns a 401 of a model call into the error of the turn.
package injectedauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/provider/anthropic"
	"github.com/majorcontext/harness/internal/provider/openai"
)

const (
	// cacheTTL is how long the answer of a probe stands.
	cacheTTL = 30 * time.Second
	// recentOK is how long after a probe that answered 200 a 401 of a model call is a fault of the server.
	recentOK = 2 * time.Minute
	// probeTimeout bounds one probe. A probe that takes longer is unknown.
	probeTimeout = 3 * time.Second
	maxBody      = 1 << 16
)

// State is the answer of the status probe.
type State int

// The states of a probe.
const (
	Unknown State = iota
	SignedIn
	SignedOut
)

// Entry is the login state of one provider entry. One Entry serves every runtime and every login of the process
// that names the same entry and probe URL.
type Entry struct {
	provider, probeURL string
	// probeHeaders are the headers that the wire of the entry needs on the probe, besides the extra headers of the entry.
	probeHeaders map[string]string

	mu            sync.Mutex
	loginURL      string
	state         State
	at            time.Time
	okAt          time.Time
	modelRejected bool
	generation    uint64
}

var (
	registryMu sync.Mutex
	registry   = map[string]*Entry{}
)

// Of returns the Entry of the providers entry name, and nil for an entry that does not use injected auth.
func Of(name string, p config.Provider) *Entry {
	wire := config.WireOf(name, p)
	if !p.UsesInjectedAuth() || wire == config.WireNone {
		return nil
	}
	probe, headers := anthropic.ModelsURL(p.BaseURL), map[string]string{"Anthropic-Version": anthropic.Version}
	if wire == config.WireResponses {
		probe, headers = openai.ListURL(p.BaseURL, p.ResponsesPath, p.ModelsFrom == config.ModelsFromChatGPT), nil
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	key := name + "\x00" + probe
	e := registry[key]
	if e == nil {
		e = &Entry{provider: name, probeURL: probe, probeHeaders: headers}
		registry[key] = e
	}
	e.mu.Lock()
	e.loginURL = p.LoginURL
	e.mu.Unlock()
	return e
}

// LoginURL is the sign-in page of the entry, or empty.
func (e *Entry) LoginURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loginURL
}

// Message is the text of a login-required error. It holds the sign-in URL and no header value.
func (e *Entry) Message() string {
	msg := "not logged in to " + e.provider
	if u := e.LoginURL(); u != "" {
		msg += "; sign in at " + u
	}
	return msg
}

// Generation changes each time the entry becomes signed in.
func (e *Entry) Generation() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fmt.Sprint(e.generation)
}

// Reset forgets the cached answer. A new sign-in starts with it.
func (e *Entry) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state, e.at, e.okAt, e.modelRejected = Unknown, time.Time{}, time.Time{}, false
}

// Cached returns the answer of the last probe while it stands. An entry whose model calls the server rejected after a
// sign-in check is signed in until the next Reset.
func (e *Entry) Cached() (State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.modelRejected:
		return SignedIn, true
	case e.state != Unknown && time.Since(e.at) < cacheTTL:
		return e.state, true
	}
	return Unknown, false
}

func (e *Entry) record(status int) State {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch status {
	case http.StatusOK:
		if e.state != SignedIn {
			e.generation++
		}
		now := time.Now()
		e.state, e.at, e.okAt = SignedIn, now, now
	case http.StatusUnauthorized:
		e.state, e.at, e.okAt = SignedOut, time.Now(), time.Time{}
	default:
		return Unknown
	}
	return e.state
}

// Probe sends the status probe through rt with the extra headers of the entry and no credential, and records the answer.
// It follows no redirect. An answer other than 200 and 401, a failure, and a wait of more than three seconds are Unknown.
func (e *Entry) Probe(ctx context.Context, rt http.RoundTripper, extra map[string]string) State {
	if rt == nil {
		rt = http.DefaultTransport
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.probeURL, nil)
	if err != nil {
		return Unknown
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	for k, v := range e.probeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	hc := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return Unknown
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	return e.record(resp.StatusCode)
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

// RoundTrip implements http.RoundTripper. A request for the model list is a probe: its answer is recorded and returned.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if req.Method == http.MethodGet && req.URL.String() == t.entry.probeURL {
		t.entry.record(resp.StatusCode)
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
	if !e.okAt.IsZero() && time.Since(e.okAt) < recentOK {
		e.modelRejected = true
		e.mu.Unlock()
		msg := "the server accepted the sign-in check but rejected the model call; "
		if u := e.LoginURL(); u != "" {
			msg += "sign in again at " + u + ", or "
		}
		return &ServerError{msg + "check how the server authorizes model calls"}
	}
	e.state, e.at = SignedOut, time.Now()
	e.mu.Unlock()
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
