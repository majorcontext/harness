package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is the example's own copy of the ChatGPT device code flow. The
// issuer, client id, and endpoints are the ones of the Codex CLI (and of
// harness internal/codexauth, which is internal and so cannot be imported).

const (
	defaultIssuer = "https://auth.openai.com"
	clientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	authClaim     = "https://api.openai.com/auth"
	refreshSkew   = 5 * time.Minute
	// staleCooldown limits how often a 401 from the backend forces a refresh.
	staleCooldown = time.Minute
	// refreshBackoff is the wait after a transient refresh failure.
	refreshBackoff = 30 * time.Second
	// refreshTimeout bounds a refresh, which runs even when its caller leaves.
	refreshTimeout = 30 * time.Second
	deviceTimeout  = 15 * time.Minute
	slowDownStep   = 5 * time.Second
)

// tokens is the login of one user. It never leaves the process: nothing logs
// it and no handler writes it to a response.
type tokens struct {
	Access, Refresh, Account string
}

func (t tokens) expiry() (time.Time, bool) {
	var c struct {
		Exp float64 `json:"exp"`
	}
	if !jwtClaims(t.Access, &c) || c.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(c.Exp), 0), true
}

func jwtClaims(token string, v any) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	return err == nil && json.Unmarshal(raw, v) == nil
}

func accountFromToken(token string) string {
	var claims map[string]json.RawMessage
	if !jwtClaims(token, &claims) {
		return ""
	}
	var a struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	_ = json.Unmarshal(claims[authClaim], &a)
	return a.AccountID
}

// tokenStore holds the tokens per app user in memory. A real server keeps
// them encrypted in a database.
type tokenStore struct {
	issuer string
	hc     *http.Client
	now    func() time.Time
	// sleep waits d, and returns false when ctx ends first.
	sleep func(ctx context.Context, d time.Duration) bool

	mu sync.Mutex
	m  map[string]*userTokens
}

type userTokens struct {
	mu        sync.Mutex
	t         tokens
	stale     bool      // the backend rejected t
	refreshed time.Time // the last good refresh or sign-in
	retryAt   time.Time // no new refresh before this time, after a transient failure
	flight    *refreshFlight
}

// refreshFlight is the one refresh in progress for a user. It serves every
// request that needs it, and it ends even when all of them leave.
type refreshFlight struct {
	done     chan struct{}
	rejected bool // the issuer refused the refresh token; the login is gone
}

func newTokenStore(issuer string, hc *http.Client) *tokenStore {
	if issuer == "" {
		issuer = defaultIssuer
	}
	return &tokenStore{issuer: strings.TrimRight(issuer, "/"), hc: hc, now: time.Now, sleep: sleepContext, m: map[string]*userTokens{}}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// errNoLogin means the user has no login, or the issuer rejected the refresh
// token and the login is gone. The user must sign in: answer 401.
var errNoLogin = errors.New("no login")

// errRefresh means that the login may still be good but could not be
// refreshed now (the issuer is down, rate limited, or sent a bad answer).
// The user need not sign in again: answer 503.
var errRefresh = errors.New("the login could not be refreshed")

func (s *tokenStore) entry(user string) *userTokens {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[user]
}

// Set stores the login of user.
func (s *tokenStore) Set(user string, t tokens) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[user] = &userTokens{t: t, refreshed: s.now()}
}

// Delete forgets the login of user.
func (s *tokenStore) Delete(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, user)
}

// Has reports whether the store holds a login for user.
func (s *tokenStore) Has(user string) bool { return s.entry(user) != nil }

// MarkStale makes the next Fresh refresh the login. The relay calls it when
// the backend rejects the token. A cooldown stops a loop on a backend that
// answers 401 or 429 for another reason.
func (s *tokenStore) MarkStale(user string) {
	e := s.entry(user)
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.now().Sub(e.refreshed) >= staleCooldown {
		e.stale = true
	}
}

// Fresh returns the login of user, refreshed when it ends within five minutes
// or when the backend rejected it. It returns errNoLogin when there is no
// login, or when the issuer rejected the refresh token (the login is then
// dropped). A transient refresh failure returns errRefresh and keeps the
// login; for refreshBackoff the old token stays in use while it is still valid,
// and an expired token is never used.
//
// The refresh runs on its own bounded context: the issuer may have rotated the
// refresh token, so the answer must be saved even if the caller leaves.
func (s *tokenStore) Fresh(ctx context.Context, user string) (tokens, error) {
	e := s.entry(user)
	if e == nil {
		return tokens{}, errNoLogin
	}
	for {
		e.mu.Lock()
		now := s.now()
		exp, known := e.t.expiry()
		if !e.stale && !(known && !exp.After(now.Add(refreshSkew))) {
			defer e.mu.Unlock()
			return e.t, nil
		}
		usable := !known || exp.After(now) // an unknown expiry is the backend's to judge
		if e.flight == nil {
			if now.Before(e.retryAt) {
				defer e.mu.Unlock()
				if usable {
					return e.t, nil
				}
				return tokens{}, errRefresh
			}
			e.flight = &refreshFlight{done: make(chan struct{})}
			go s.refreshFlight(user, e, e.flight, e.t)
		}
		f := e.flight
		e.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return tokens{}, ctx.Err()
		}
		if f.rejected {
			return tokens{}, errNoLogin
		}
	}
}

func (s *tokenStore) refreshFlight(user string, e *userTokens, f *refreshFlight, t tokens) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	next, rejected, err := s.refresh(ctx, t)
	e.mu.Lock()
	defer e.mu.Unlock()
	defer close(f.done)
	e.flight = nil
	switch {
	case err == nil:
		e.t, e.stale, e.refreshed, e.retryAt = next, false, s.now(), time.Time{}
	case rejected:
		f.rejected = true
		s.mu.Lock()
		if s.m[user] == e {
			delete(s.m, user)
		}
		s.mu.Unlock()
	default:
		e.stale, e.retryAt = false, s.now().Add(refreshBackoff)
	}
}

// refresh trades the refresh token for a new access token. rejected is true
// when the issuer refused the refresh token for good.
func (s *tokenStore) refresh(ctx context.Context, t tokens) (next tokens, rejected bool, err error) {
	if t.Refresh == "" {
		return t, true, errors.New("no refresh token")
	}
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": t.Refresh})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return t, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, raw, err := doRaw(s.hc, req)
	switch {
	case err != nil:
		return t, false, err
	case status == http.StatusTooManyRequests || status >= 500:
		return t, false, fmt.Errorf("refresh: HTTP %d", status)
	case status != http.StatusOK:
		return t, true, fmt.Errorf("refresh: HTTP %d", status)
	}
	var tok struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.Access == "" {
		return t, false, errors.New("refresh: the answer holds no access token")
	}
	next = tokens{Access: tok.Access, Refresh: t.Refresh, Account: t.Account}
	if tok.Refresh != "" {
		next.Refresh = tok.Refresh // the issuer rotates it
	}
	return next, false, nil
}

// deviceCode is a sign-in that the issuer started and the user has not finished.
type deviceCode struct {
	UserCode string
	URL      string
	id       string
	interval time.Duration
}

func (s *tokenStore) startDevice(ctx context.Context) (*deviceCode, error) {
	var uc struct {
		ID       string          `json:"device_auth_id"`
		UserCode string          `json:"user_code"`
		Alias    string          `json:"usercode"`
		Interval json.RawMessage `json:"interval"`
	}
	status, err := s.postJSON(ctx, s.issuer+"/api/accounts/deviceauth/usercode", map[string]string{"client_id": clientID}, &uc)
	switch {
	case err != nil:
		return nil, err
	case status != http.StatusOK:
		return nil, fmt.Errorf("the device code request failed: HTTP %d", status)
	}
	if uc.UserCode == "" {
		uc.UserCode = uc.Alias
	}
	if uc.UserCode == "" || uc.ID == "" {
		return nil, errors.New("the device code answer is incomplete")
	}
	n := 0
	if json.Unmarshal(uc.Interval, &n) != nil {
		var str string
		_ = json.Unmarshal(uc.Interval, &str)
		_, _ = fmt.Sscanf(str, "%d", &n)
	}
	return &deviceCode{UserCode: uc.UserCode, URL: s.issuer + "/codex/device", id: uc.ID, interval: time.Duration(max(n, 1)) * time.Second}, nil
}

// wait polls until the user enters the code, then exchanges the grant for tokens.
func (s *tokenStore) wait(ctx context.Context, d *deviceCode) (tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	interval := d.interval
	for {
		body, _ := json.Marshal(map[string]string{"device_auth_id": d.id, "user_code": d.UserCode})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/api/accounts/deviceauth/token", bytes.NewReader(body))
		if err != nil {
			return tokens{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		status, raw, err := doRaw(s.hc, req)
		if err != nil {
			if ctx.Err() != nil {
				return tokens{}, errors.New("the device code expired or the sign-in was canceled")
			}
			return tokens{}, err
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		switch {
		case status == http.StatusOK:
			var g struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
			}
			if json.Unmarshal(raw, &g) != nil || g.AuthorizationCode == "" {
				return tokens{}, errors.New("the device sign-in answer is incomplete")
			}
			return s.exchange(ctx, g.AuthorizationCode, g.CodeVerifier)
		case status == http.StatusForbidden, status == http.StatusNotFound, e.Error == "authorization_pending":
		case e.Error == "slow_down":
			interval += slowDownStep
		default:
			return tokens{}, fmt.Errorf("the device sign-in failed: HTTP %d", status)
		}
		if !s.sleep(ctx, interval) {
			return tokens{}, errors.New("the device code expired or the sign-in was canceled")
		}
	}
}

func (s *tokenStore) exchange(ctx context.Context, code, verifier string) (tokens, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {s.issuer + "/deviceauth/callback"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, raw, err := doRaw(s.hc, req)
	if err != nil {
		return tokens{}, err
	}
	var tok struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		ID      string `json:"id_token"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &tok) != nil || tok.Access == "" {
		return tokens{}, fmt.Errorf("the issuer could not finish the sign-in: HTTP %d", status)
	}
	account := accountFromToken(tok.Access)
	if account == "" {
		account = accountFromToken(tok.ID)
	}
	if account == "" {
		return tokens{}, errors.New("the sign-in token names no ChatGPT account")
	}
	return tokens{Access: tok.Access, Refresh: tok.Refresh, Account: account}, nil
}

func (s *tokenStore) postJSON(ctx context.Context, endpoint string, body, out any) (int, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, raw, err := doRaw(s.hc, req)
	if err == nil && status == http.StatusOK && json.Unmarshal(raw, out) != nil {
		err = errors.New("the issuer sent an answer that is not JSON")
	}
	return status, err
}

// doRaw sends req and returns the status and body. Errors hold no URL query
// and no header, so they are safe to show.
func doRaw(hc *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
