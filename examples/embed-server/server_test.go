package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func jwt(t *testing.T, exp time.Time, account string) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." +
		enc(map[string]any{"exp": exp.Unix(), authClaim: map[string]string{"chatgpt_account_id": account}}) + ".sig"
}

type rig struct {
	t        *testing.T
	srv      *server
	front    *httptest.Server
	upstream *httptest.Server
	gotReq   atomic.Pointer[http.Request]
	hits     atomic.Int32
	issuer   *httptest.Server
	refresh  atomic.Pointer[string] // access token that the issuer returns; nil: reject
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t}
	r.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.hits.Add(1)
		r.gotReq.Store(q)
		_, _ = io.WriteString(w, "ok")
	}))
	r.issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		tok := r.refresh.Load()
		if q.URL.Path != "/oauth/token" || tok == nil {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": *tok, "refresh_token": "next-refresh"})
	}))
	srv, err := newServer(config{Upstream: r.upstream.URL + "/backend-api/codex", Issuer: r.issuer.URL})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = srv
	r.front = httptest.NewServer(srv.handler())
	t.Cleanup(func() { r.front.Close(); r.upstream.Close(); r.issuer.Close() })
	return r
}

// signIn starts a session for user and returns its cookie.
func (r *rig) signIn(user string) *http.Cookie {
	w := httptest.NewRecorder()
	r.srv.sessions.login(w, httptest.NewRequest("POST", "/login", nil), user)
	return w.Result().Cookies()[0]
}

func (r *rig) call(c *http.Cookie, headers map[string]string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.front.URL+"/model-proxy/relay/codex/responses?x=1", strings.NewReader(`{}`))
	if c != nil {
		req.AddCookie(c)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Origin", r.front.URL)
	req.Header.Set("Referer", r.front.URL+"/")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestModelProxyAnswers401WithoutSessionOrToken(t *testing.T) {
	r := newRig(t)
	if got := r.call(nil, nil).StatusCode; got != 401 {
		t.Errorf("no session: status %d, want 401", got)
	}
	resp := r.call(r.signIn("ann"), nil)
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Errorf("no token: status %d, header %q; want 401 with WWW-Authenticate", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if r.hits.Load() != 0 {
		t.Error("a request without a token reached the upstream")
	}
}

func TestModelProxyInjectsTheCredentialOfTheSessionUserOnly(t *testing.T) {
	r := newRig(t)
	r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Hour), "acct-ann"), Refresh: "r", Account: "acct-ann"})
	r.srv.store.Set("bob", tokens{Access: jwt(t, time.Now().Add(time.Hour), "acct-bob"), Refresh: "r", Account: "acct-bob"})
	ann := r.signIn("ann")

	resp := r.call(ann, map[string]string{
		"Authorization": "Bearer page-supplied", "X-Api-Key": "k", "ChatGPT-Account-ID": "acct-bob",
		"X-App-User": "bob", "Sec-Ch-Ua": "x",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	up := r.gotReq.Load()
	if up == nil {
		t.Fatal("the upstream saw no request")
	}
	if up.URL.Path != "/backend-api/codex/responses" || up.URL.RawQuery != "x=1" {
		t.Errorf("upstream URL %s?%s", up.URL.Path, up.URL.RawQuery)
	}
	if !strings.HasPrefix(up.Header.Get("Authorization"), "Bearer ") || strings.Contains(up.Header.Get("Authorization"), "page-supplied") {
		t.Error("the upstream did not get the stored token")
	}
	if got := up.Header.Get("ChatGPT-Account-ID"); got != "acct-ann" {
		t.Errorf("account %q, want acct-ann", got)
	}
	for _, h := range []string{"Cookie", "Origin", "Referer", "X-Api-Key", "X-App-User", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Ch-Ua"} {
		if v := up.Header.Values(h); len(v) != 0 {
			t.Errorf("the upstream got %s", h)
		}
	}
}

func TestModelProxyRefusesCrossSiteAndOtherPaths(t *testing.T) {
	r := newRig(t)
	r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Hour), "a"), Refresh: "r", Account: "a"})
	c := r.signIn("ann")
	if got := r.call(c, map[string]string{"Sec-Fetch-Site": "cross-site"}).StatusCode; got != 403 {
		t.Errorf("cross-site: status %d, want 403", got)
	}
	req, _ := http.NewRequest("GET", r.front.URL+"/model-proxy/relay/other/x", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != 404 || r.hits.Load() != 0 {
		t.Errorf("other relay: status %d, hits %d", resp.StatusCode, r.hits.Load())
	}
}

func TestExpiringTokenIsRefreshedBeforeTheRequest(t *testing.T) {
	r := newRig(t)
	fresh := jwt(t, time.Now().Add(time.Hour), "a")
	r.refresh.Store(&fresh)
	r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Minute), "a"), Refresh: "old", Account: "a"})
	if got := r.call(r.signIn("ann"), nil).StatusCode; got != 200 {
		t.Fatalf("status %d", got)
	}
	if got := r.gotReq.Load().Header.Get("Authorization"); got != "Bearer "+fresh {
		t.Error("the upstream did not get the refreshed token")
	}
}

func TestRejectedRefreshTokenEndsTheLogin(t *testing.T) {
	r := newRig(t)
	r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Minute), "a"), Refresh: "old", Account: "a"})
	c := r.signIn("ann")
	if got := r.call(c, nil).StatusCode; got != 401 {
		t.Errorf("status %d, want 401", got)
	}
	if r.srv.store.Has("ann") || r.hits.Load() != 0 {
		t.Error("the login stayed, or the request reached the upstream")
	}
}

func TestBackendRejectionForcesOneRefresh(t *testing.T) {
	r := newRig(t)
	now := time.Now()
	r.srv.store.now = func() time.Time { return now }
	fresh := jwt(t, now.Add(2*time.Hour), "a")
	r.refresh.Store(&fresh)
	r.srv.store.Set("ann", tokens{Access: jwt(t, now.Add(time.Hour), "a"), Refresh: "old", Account: "a"})
	now = now.Add(2 * staleCooldown)
	r.srv.store.MarkStale("ann")
	if _, ok := r.srv.store.Fresh(t.Context(), "ann"); !ok {
		t.Fatal("no login")
	}
	tok, _ := r.srv.store.Fresh(t.Context(), "ann")
	if tok.Access != fresh || tok.Refresh != "next-refresh" {
		t.Error("the stale login was not refreshed and rotated")
	}
}
