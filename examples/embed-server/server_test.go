package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	tok, err := r.srv.store.Fresh(t.Context(), "ann")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != fresh || tok.Refresh != "next-refresh" {
		t.Error("the stale login was not refreshed and rotated")
	}
}

// rawStatus writes a request as bytes on a TCP connection, so the HTTP client
// cannot normalize an absolute-form target, and returns the status code.
func rawStatus(t *testing.T, addr, request string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

func TestModelProxyIsNotAForwardProxy(t *testing.T) {
	r := newRig(t)
	var victimHits atomic.Int32
	var leaked atomic.Value
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		victimHits.Add(1)
		leaked.Store(q.Header.Get("Authorization"))
	}))
	defer victim.Close()
	r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Hour), "a"), Refresh: "r", Account: "a"})
	cookie := r.signIn("ann")
	front := strings.TrimPrefix(r.front.URL, "http://")
	vhost := strings.TrimPrefix(victim.URL, "http://")

	for name, req := range map[string]string{
		"absolute-form": "GET " + victim.URL + "/model-proxy/relay/codex/x HTTP/1.1\r\nHost: " + front + "\r\nCookie: " + cookie.String() + "\r\nConnection: close\r\n\r\n",
		"connect":       "CONNECT " + vhost + " HTTP/1.1\r\nHost: " + vhost + "\r\nCookie: " + cookie.String() + "\r\nConnection: close\r\n\r\n",
	} {
		if got := rawStatus(t, front, req); got == 200 {
			t.Errorf("%s: status 200", name)
		}
	}
	if victimHits.Load() != 0 || r.hits.Load() != 0 {
		t.Errorf("a request reached another server (victim %d, upstream %d); token sent: %v", victimHits.Load(), r.hits.Load(), leaked.Load() != nil && leaked.Load() != "")
	}
}

func TestTransientRefreshFailureKeepsTheLoginAndAnswers503(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"issuer down":     func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) },
		"unparseable 200": func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>") },
		"rate limited":    func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "slow", http.StatusTooManyRequests) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			bad := httptest.NewServer(handler)
			defer bad.Close()
			r.srv.store.issuer = bad.URL
			r.srv.store.Set("ann", tokens{Access: jwt(t, time.Now().Add(-time.Minute), "a"), Refresh: "r", Account: "a"})
			if got := r.call(r.signIn("ann"), nil).StatusCode; got != http.StatusServiceUnavailable {
				t.Errorf("status %d, want 503", got)
			}
			if !r.srv.store.Has("ann") || r.hits.Load() != 0 {
				t.Error("the login was dropped, or the request reached the upstream")
			}
		})
	}
}

func TestDeviceFlowPollingSlowsDownAndWaitsOnPending(t *testing.T) {
	var polls atomic.Int32
	access := jwt(t, time.Now().Add(time.Hour), "acct")
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = io.WriteString(w, `{"device_auth_id":"id","user_code":"ABCD-1234","interval":"2"}`)
		case "/api/accounts/deviceauth/token":
			switch polls.Add(1) {
			case 1:
				http.Error(w, `{"error":"slow_down"}`, http.StatusBadRequest)
			case 2:
				http.Error(w, `{"error":"authorization_pending"}`, http.StatusBadRequest)
			case 3:
				http.Error(w, "", http.StatusForbidden)
			default:
				_, _ = io.WriteString(w, `{"authorization_code":"c","code_verifier":"v"}`)
			}
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "r"})
		}
	}))
	defer issuer.Close()
	store := newTokenStore(issuer.URL, http.DefaultClient)
	var waits []time.Duration
	store.sleep = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }
	code, err := store.startDevice(t.Context())
	if err != nil || code.UserCode != "ABCD-1234" {
		t.Fatalf("start: %v %v", code, err)
	}
	tok, err := store.wait(t.Context(), code)
	if err != nil || tok.Account != "acct" || tok.Refresh != "r" {
		t.Fatalf("wait: %v", err)
	}
	// The first answer is slow_down: it adds 5 s to the interval of 2 s, and the interval stays.
	if want := []time.Duration{7 * time.Second, 7 * time.Second, 7 * time.Second}; !slices.Equal(waits, want) {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

func TestFinishedFlowsAreNotKept(t *testing.T) {
	r := newRig(t)
	access := jwt(t, time.Now().Add(time.Hour), "acct")
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = io.WriteString(w, `{"device_auth_id":"id","user_code":"AAAA-0000","interval":1}`)
		case "/api/accounts/deviceauth/token":
			if q.Header.Get("X-Fail") != "" {
				http.Error(w, "", http.StatusTeapot)
				return
			}
			_, _ = io.WriteString(w, `{"authorization_code":"c","code_verifier":"v"}`)
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "r"})
		}
	}))
	defer issuer.Close()
	r.srv.store.issuer = issuer.URL
	run := func(id, user string) {
		code, err := r.srv.store.startDevice(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fl := &flow{code: code, cancel: func() {}}
		r.srv.flows.m[id] = fl
		r.srv.runFlow(t.Context(), id, user, fl, code)
	}
	run("s1", "ann")
	if !r.srv.store.Has("ann") || len(r.srv.flows.m) != 0 {
		t.Errorf("after a finished sign-in: login %v, flows %d", r.srv.store.Has("ann"), len(r.srv.flows.m))
	}

	// A failed flow stays until the page shows it once.
	r.srv.store.hc = &http.Client{Transport: failPoll{http.DefaultTransport}}
	run("s2", "bob")
	if fl := r.srv.flows.take("s2"); fl.err == "" {
		t.Error("the failed flow lost its error")
	}
	if len(r.srv.flows.m) != 0 {
		t.Error("the failed flow was kept after it was shown")
	}
}

type failPoll struct{ http.RoundTripper }

func (f failPoll) RoundTrip(q *http.Request) (*http.Response, error) {
	if strings.HasSuffix(q.URL.Path, "/deviceauth/token") {
		q.Header.Set("X-Fail", "1")
	}
	return f.RoundTripper.RoundTrip(q)
}

func (r *rig) postForm(path string, c *http.Cookie, site string, form url.Values) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.front.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if site != "" {
		req.Header.Set("Sec-Fetch-Site", site)
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestLoginRefusesCrossSiteRotatesTheSessionAndLogoutEndsIt(t *testing.T) {
	r := newRig(t)
	if got := r.postForm("/login", nil, "cross-site", url.Values{"user": {"mallory"}}).StatusCode; got != http.StatusBadRequest {
		t.Errorf("cross-site login: status %d, want 400", got)
	}
	old := r.signIn("ann")
	resp := r.postForm("/login", old, "same-origin", url.Values{"user": {"ann"}})
	var fresh []*http.Cookie
	for _, c := range resp.Cookies() {
		if c.Value != "" {
			fresh = append(fresh, c)
		}
	}
	if len(fresh) != 1 || fresh[0].Value == old.Value {
		t.Fatal("login did not issue a new session id")
	}
	if _, sess := r.srv.sessions.sessionOf(requestWith(old)); sess != nil {
		t.Error("the old session id still works after login")
	}
	if got := r.postForm("/logout", fresh[0], "cross-site", nil).StatusCode; got != http.StatusForbidden {
		t.Errorf("cross-site logout: status %d, want 403", got)
	}
	r.postForm("/logout", fresh[0], "same-origin", nil)
	if _, sess := r.srv.sessions.sessionOf(requestWith(fresh[0])); sess != nil {
		t.Error("the session survives logout")
	}
}

func requestWith(c *http.Cookie) *http.Request {
	q := httptest.NewRequest("GET", "/", nil)
	q.AddCookie(c)
	return q
}

func TestSessionCookieIsSecureWhenConfigured(t *testing.T) {
	s := newSessions(true)
	w := httptest.NewRecorder()
	s.login(w, httptest.NewRequest("POST", "/login", nil), "ann")
	if c := w.Result().Cookies()[0]; !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie %+v", c)
	}
}

func newClockedStore(issuer string) (*tokenStore, *time.Time) {
	now := time.Now()
	st := newTokenStore(issuer, http.DefaultClient)
	st.now = func() time.Time { return now }
	return st, &now
}

func TestTransientRefreshFailureBacksOffWhileTheTokenIsValid(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	st, now := newClockedStore(down.URL)
	soon := jwt(t, now.Add(2*time.Minute), "a")
	st.Set("ann", tokens{Access: soon, Refresh: "r", Account: "a"})
	for range 5 {
		tok, err := st.Fresh(t.Context(), "ann")
		if err != nil || tok.Access != soon {
			t.Fatalf("a valid token must keep working during the backoff: %v", err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("issuer hits %d during the backoff, want 1", hits.Load())
	}
	*now = now.Add(refreshBackoff + time.Second)
	if _, err := st.Fresh(t.Context(), "ann"); err != nil || hits.Load() != 2 {
		t.Errorf("after the backoff: err %v, hits %d, want one more attempt", err, hits.Load())
	}

	// The backoff never authorizes an expired token.
	*now = now.Add(time.Hour)
	for range 3 {
		if _, err := st.Fresh(t.Context(), "ann"); !errors.Is(err, errRefresh) {
			t.Fatalf("expired token: err %v, want errRefresh", err)
		}
	}
	if hits.Load() != 3 {
		t.Errorf("issuer hits %d, want 3 (one attempt, then the backoff)", hits.Load())
	}
	if !st.Has("ann") {
		t.Error("the login was dropped")
	}
}

func TestRefreshFinishesAndStoresTheNewTokenWhenTheCallerLeaves(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var hits atomic.Int32
	fresh := jwt(t, time.Now().Add(time.Hour), "a")
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fresh, "refresh_token": "rotated"})
	}))
	defer issuer.Close()
	st := newTokenStore(issuer.URL, http.DefaultClient)
	st.Set("ann", tokens{Access: jwt(t, time.Now().Add(time.Minute), "a"), Refresh: "old", Account: "a"})

	ctx, cancel := context.WithCancel(t.Context())
	left := make(chan error, 1)
	go func() { _, err := st.Fresh(ctx, "ann"); left <- err }()
	<-entered
	cancel()
	if err := <-left; err == nil {
		t.Error("a caller that left got a token")
	}
	close(release)
	tok, err := st.Fresh(t.Context(), "ann")
	if err != nil || tok.Access != fresh || tok.Refresh != "rotated" || hits.Load() != 1 {
		t.Errorf("the rotated login was lost: err %v, hits %d", err, hits.Load())
	}
}

func TestStartDuringTeardownLeavesNoFlow(t *testing.T) {
	r := newRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var polls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			close(entered)
			select {
			case <-release:
			case <-q.Context().Done():
				return
			}
			_, _ = io.WriteString(w, `{"device_auth_id":"id","user_code":"AAAA-0000","interval":1}`)
		default:
			polls.Add(1)
		}
	}))
	defer issuer.Close()
	r.srv.store.issuer = issuer.URL
	cookie := r.signIn("ann")
	_, sess := r.srv.sessions.sessionOf(requestWith(cookie))

	done := make(chan int, 1)
	go func() {
		done <- r.postForm("/settings/codex/start", cookie, "same-origin", url.Values{"csrf": {sess.csrf}}).StatusCode
	}()
	<-entered
	r.postForm("/logout", cookie, "same-origin", nil)
	<-done
	close(release)
	r.srv.flows.mu.Lock()
	n := len(r.srv.flows.m)
	r.srv.flows.mu.Unlock()
	if n != 0 || polls.Load() != 0 {
		t.Errorf("flows %d, polls %d; a flow outlived its session", n, polls.Load())
	}
}

func TestFailedDeviceCodeRequestIsShownOnce(t *testing.T) {
	r := newRig(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "", http.StatusInternalServerError) }))
	defer bad.Close()
	r.srv.store.issuer = bad.URL
	cookie := r.signIn("ann")
	_, sess := r.srv.sessions.sessionOf(requestWith(cookie))
	r.postForm("/settings/codex/start", cookie, "same-origin", url.Values{"csrf": {sess.csrf}})
	page := func() string {
		req, _ := http.NewRequest("GET", r.front.URL+"/settings/codex", nil)
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if !strings.Contains(page(), "failed") {
		t.Error("the first view does not show the error")
	}
	if strings.Contains(page(), "failed") {
		t.Error("the error repeats on reload")
	}
}

func TestPagesLabelTheirFields(t *testing.T) {
	r := newRig(t)
	for path, want := range map[string]string{"/login": `<label for="user">`, "/": `<label for="prompt">`} {
		resp, err := http.Get(r.front.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s has no %s", path, want)
		}
	}
}
