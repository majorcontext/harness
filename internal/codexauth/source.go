package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// refreshTimeout bounds a refresh. A refresh of a token that is still valid gets the short bound.
	refreshTimeout      = 30 * time.Second
	validRefreshTimeout = 10 * time.Second
	// refreshBackoff is the first wait after a failed refresh of a token that is still valid. It doubles with each failure.
	refreshBackoff = 30 * time.Second
	saveLockWait   = 2 * time.Second
)

// SourceOptions configure a Source.
type SourceOptions struct {
	// Path is the credential file.
	Path string
	// Issuer is the OAuth issuer. Empty is DefaultIssuer.
	Issuer string
	// HTTP sends the refresh request. nil: a client with a 30 s timeout.
	HTTP *http.Client
	// Now is the clock. nil: time.Now.
	Now func() time.Time
	// Log receives the lines about a refresh. They hold no token. nil: slog.Default().
	Log *slog.Logger
}

// Source gives the credentials of the login to the requests of one process.
// It reads the file at the first request, keeps it by modification time and
// size, and refreshes an access token that ends within 5 minutes or that the
// backend rejected. One refresh serves every concurrent request, and a lock
// file fences it against other processes.
//
// A refresh runs to its end even when the request that started it is
// canceled: the issuer may have rotated the refresh token, and a lost answer
// would leave the file with a token that the issuer rejects. The
// refreshed login stays in memory until it is written.
type Source struct {
	store  Store
	issuer string
	hc     *http.Client
	now    func() time.Time
	log    *slog.Logger

	mu          sync.Mutex // guards the fields below; never held across the network
	cached      Credentials
	stamp       fileStamp
	pending     *Credentials // a refreshed login that the file does not hold yet
	pendingBase string       // the access token of the file that pending replaces
	flight      *flight
	failures    int
	retryAt     time.Time
	dead        string // a refresh token that the issuer rejected
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// flight is one refresh in progress. creds and err are set before done closes.
type flight struct {
	done  chan struct{}
	creds Credentials
	err   error
}

// NewSource returns a Source. It reads nothing.
func NewSource(o SourceOptions) *Source {
	s := &Source{store: Store{Path: o.Path}, issuer: strings.TrimRight(o.Issuer, "/"), hc: o.HTTP, now: o.Now, log: o.Log}
	if s.issuer == "" {
		s.issuer = DefaultIssuer
	}
	if s.hc == nil {
		s.hc = &http.Client{CheckRedirect: noRedirect}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Credentials returns a login whose access token is not about to end. rejected
// is the access token that the backend refused, or empty: the Source then
// refreshes unless the file already holds another token that is fresh.
func (s *Source) Credentials(ctx context.Context, rejected string) (Credentials, error) {
	s.mu.Lock()
	c, err := s.current()
	if err != nil {
		s.mu.Unlock()
		return Credentials{}, err
	}
	if !s.stale(c, rejected) {
		s.mu.Unlock()
		return c, nil
	}
	if s.dead != "" && s.dead == c.RefreshToken {
		s.mu.Unlock()
		return Credentials{}, &LoginRequiredError{Reason: "the issuer rejected the refresh token"}
	}
	_, valid := c.remaining(s.now())
	if rejected == "" && valid && s.now().Before(s.retryAt) {
		s.mu.Unlock()
		return c, nil
	}
	f := s.flight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		s.flight = f
		go s.refresh(f, c, rejected)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	}
	switch {
	case f.err == nil:
		return f.creds, nil
	case rejected == "" && valid && !IsLoginRequired(f.err):
		return c, nil
	}
	return Credentials{}, f.err
}

func (s *Source) stale(c Credentials, rejected string) bool {
	return rejected != "" && c.AccessToken == rejected || c.expiring(s.now())
}

// current returns the newest login: the refreshed one that the file lacks, else the file. The caller holds mu.
func (s *Source) current() (Credentials, error) {
	fc, err := s.read(false)
	if s.pending == nil {
		return fc, err
	}
	if err == nil && fc.AccessToken != s.pendingBase && fc.AccessToken != s.pending.AccessToken {
		s.pending = nil // another process wrote the file after the refresh began
		return fc, nil
	}
	c := *s.pending
	if s.flight == nil && s.save(c) == nil {
		s.pending = nil
	}
	return c, nil
}

// read returns the file, from the cache when its modification time and size are unchanged. The caller holds mu.
func (s *Source) read(force bool) (Credentials, error) {
	st, err := os.Stat(s.store.Path)
	if err != nil {
		s.stamp = fileStamp{}
		return s.store.Load()
	}
	stamp := fileStamp{st.ModTime(), st.Size()}
	if !force && stamp == s.stamp && s.cached.AccessToken != "" {
		return s.cached, nil
	}
	c, err := s.store.Load()
	if err != nil {
		return Credentials{}, err
	}
	s.cached, s.stamp = c, stamp
	return c, nil
}

// save writes c under the file lock and updates the cache. The caller holds mu.
func (s *Source) save(c Credentials) error {
	ctx, cancel := context.WithTimeout(context.Background(), saveLockWait)
	defer cancel()
	unlock, err := s.store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return s.write(c)
}

// write saves c and updates the cache. The caller holds the file lock and mu.
func (s *Source) write(c Credentials) error {
	if err := s.store.Save(c); err != nil {
		return err
	}
	s.cached, s.stamp = c, fileStamp{}
	if st, err := os.Stat(s.store.Path); err == nil {
		s.stamp = fileStamp{st.ModTime(), st.Size()}
	}
	return nil
}

// refresh runs one refresh and ends f. It uses no context of a request.
func (s *Source) refresh(f *flight, c Credentials, rejected string) {
	timeout := refreshTimeout
	if _, valid := c.remaining(s.now()); valid {
		timeout = validRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	next, err := s.exchange(ctx, c, rejected)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flight = nil
	f.creds, f.err = next, err
	if err != nil {
		s.failed(c, err)
	} else {
		s.failures = 0
	}
	close(f.done)
}

// exchange takes the file lock, reads the file again (another process may have refreshed),
// and posts the refresh token when the login is still stale. A refreshed login is kept in
// memory, and written when it can be.
func (s *Source) exchange(ctx context.Context, c Credentials, rejected string) (Credentials, error) {
	unlock, err := s.store.Lock(ctx)
	if err != nil {
		return Credentials{}, err
	}
	defer unlock()
	s.mu.Lock()
	fc, err := s.read(true)
	s.mu.Unlock()
	if err == nil && fc.AccessToken != c.AccessToken && !s.stale(fc, rejected) {
		return fc, nil
	}
	if err == nil {
		c.RefreshToken = fc.RefreshToken
	}
	next, err := s.post(ctx, c)
	if err != nil {
		return Credentials{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending, s.pendingBase = &next, c.AccessToken
	if err := s.write(next); err != nil {
		s.log.Warn("codex login refreshed but not saved; the new login stays in memory", "err", err)
		return next, nil
	}
	s.pending = nil
	s.log.Info("codex login refreshed")
	return next, nil
}

// failed records a failed refresh. The caller holds mu.
func (s *Source) failed(c Credentials, err error) {
	var le *LoginRequiredError
	if errors.As(err, &le) && c.RefreshToken != "" && le.refreshRejected {
		s.dead = c.RefreshToken
		return
	}
	s.failures++
	if remaining, valid := c.remaining(s.now()); valid {
		wait := min(refreshBackoff<<min(s.failures-1, 10), remaining/2)
		s.retryAt = s.now().Add(wait)
	}
	s.log.Warn("codex login refresh failed", "err", err)
}

// post exchanges the refresh token of c for a new access token. The old refresh
// token stays when the answer carries none.
func (s *Source) post(ctx context.Context, c Credentials) (Credentials, error) {
	if c.RefreshToken == "" {
		return Credentials{}, &LoginRequiredError{Reason: "the login has no refresh token"}
	}
	body, _ := json.Marshal(map[string]string{"client_id": ClientID, "grant_type": "refresh_token", "refresh_token": c.RefreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.issuer+"/oauth/token", bytes.NewReader(body))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Credentials{}, fmt.Errorf("codex login refresh: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return Credentials{}, fmt.Errorf("codex login refresh: HTTP %d", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Credentials{}, &LoginRequiredError{Reason: fmt.Sprintf("the refresh token was rejected, HTTP %d", resp.StatusCode), refreshRejected: true}
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return Credentials{}, &LoginRequiredError{Reason: "the refresh answer holds no access token", refreshRejected: true}
	}
	next := Credentials{AccessToken: tok.AccessToken, RefreshToken: c.RefreshToken, AccountID: c.AccountID}
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	return next, nil
}
