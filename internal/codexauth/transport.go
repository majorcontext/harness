package codexauth

import (
	"io"
	"net/http"
	"net/url"
	"slices"
)

// Transport adds the login to the requests that go to the endpoints it is
// bound to, and to no other request. A request that the backend answers with 401
// is sent again once, with credentials that the Source refreshed.
type Transport struct {
	src       *Source
	base      http.RoundTripper
	endpoints []endpoint
}

// endpoint is the scheme, host, and path of a bound URL.
type endpoint struct{ scheme, host, path string }

// NewTransport binds the login to the URLs of model endpoints: the scheme, host, and path of each. A query is not part of the binding.
// base is the transport underneath; nil is http.DefaultTransport. A URL that is not absolute binds nothing.
func NewTransport(src *Source, base http.RoundTripper, endpoints ...string) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	t := &Transport{src: src, base: base}
	for _, raw := range endpoints {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			t.endpoints = append(t.endpoints, endpoint{canonicalScheme(u.Scheme), u.Host, u.Path})
		}
	}
	return t
}

// canonicalScheme maps the websocket schemes to the HTTP schemes of the handshake.
func canonicalScheme(s string) string {
	switch s {
	case "ws":
		return "http"
	case "wss":
		return "https"
	}
	return s
}

func (t *Transport) bound(u *url.URL) bool {
	return slices.Contains(t.endpoints, endpoint{canonicalScheme(u.Scheme), u.Host, u.Path})
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.bound(req.URL) {
		return t.base.RoundTrip(req)
	}
	creds, err := t.src.Credentials(req.Context(), "")
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := t.base.RoundTrip(withLogin(req, creds))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	again, ok := replay(req)
	if !ok {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	fresh, err := t.src.Credentials(req.Context(), creds.AccessToken)
	if err != nil {
		return nil, err
	}
	resp, err = t.base.RoundTrip(withLogin(again, fresh))
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, &LoginRequiredError{Reason: "the Codex backend rejected the refreshed login"}
	}
	return resp, err
}

// replay returns req with a new body, and false when the body cannot be sent again.
func replay(req *http.Request) (*http.Request, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	again := req.Clone(req.Context())
	again.Body = body
	return again, true
}

// withLogin returns a copy of req that carries the login.
func withLogin(req *http.Request, c Credentials) *http.Request {
	out := req.Clone(req.Context())
	out.Header.Set("Authorization", "Bearer "+c.AccessToken)
	account := AccountFromToken(c.AccessToken)
	if account == "" {
		account = c.AccountID
	}
	out.Header.Set("ChatGPT-Account-ID", account)
	out.Header.Set("originator", Originator)
	return out
}
