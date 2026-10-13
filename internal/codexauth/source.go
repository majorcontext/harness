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
type Source struct {
	store  Store
	issuer string
	hc     *http.Client
	now    func() time.Time
	log    *slog.Logger

	mu     sync.Mutex
	cached Credentials
	stamp  fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// NewSource returns a Source. It reads nothing.
func NewSource(o SourceOptions) *Source {
	s := &Source{store: Store{Path: o.Path}, issuer: strings.TrimRight(o.Issuer, "/"), hc: o.HTTP, now: o.Now, log: o.Log}
	if s.issuer == "" {
		s.issuer = DefaultIssuer
	}
	if s.hc == nil {
		s.hc = &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
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
	defer s.mu.Unlock()
	c, err := s.read(false)
	if err != nil {
		return Credentials{}, err
	}
	if !s.stale(c, rejected) {
		return c, nil
	}
	unlock, err := s.store.Lock(ctx)
	if err != nil {
		return Credentials{}, err
	}
	defer unlock()
	if c, err = s.read(true); err != nil {
		return Credentials{}, err
	}
	if !s.stale(c, rejected) {
		return c, nil
	}
	next, err := s.refresh(ctx, c)
	if err != nil {
		if exp, ok := c.Expiry(); ok && rejected == "" && exp.After(s.now()) && !IsLoginRequired(err) {
			s.log.Warn("codex login refresh failed; the access token has not ended yet", "err", err)
			return c, nil
		}
		return Credentials{}, err
	}
	if err := s.store.Save(next); err != nil {
		return Credentials{}, err
	}
	s.stamp = fileStamp{}
	s.cached = next
	if st, err := os.Stat(s.store.Path); err == nil {
		s.stamp = fileStamp{st.ModTime(), st.Size()}
	}
	return next, nil
}

func (s *Source) stale(c Credentials, rejected string) bool {
	return c.AccessToken == rejected && rejected != "" || c.expiring(s.now())
}

// read returns the file, from the cache when its modification time and size are unchanged.
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

// refresh exchanges the refresh token of c for a new access token. The old
// refresh token stays when the answer carries none.
func (s *Source) refresh(ctx context.Context, c Credentials) (Credentials, error) {
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
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Credentials{}, &LoginRequiredError{Reason: fmt.Sprintf("the refresh token was rejected, HTTP %d", resp.StatusCode)}
	case resp.StatusCode != http.StatusOK:
		return Credentials{}, fmt.Errorf("codex login refresh: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" {
		return Credentials{}, errors.New("codex login refresh: the answer holds no access token")
	}
	next := Credentials{AccessToken: tok.AccessToken, RefreshToken: c.RefreshToken, AccountID: c.AccountID}
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	s.log.Info("codex login refreshed")
	return next, nil
}
