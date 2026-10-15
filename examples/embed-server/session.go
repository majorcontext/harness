package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
)

// STAND-IN. This is not an authentication system. A real application already
// has one: replace this file with the session layer of that application, and
// keep only the contract: sessionOf(r) returns the session of the request, or nil.

const cookieName = "demo_session"

type session struct {
	user string
	csrf string // binds the forms of /settings/codex to this session
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// login starts a session for user and sets the cookie. The cookie is HttpOnly
// and SameSite=Lax, so a cross-site POST does not carry it.
func (s *sessions) login(w http.ResponseWriter, r *http.Request, user string) {
	id := randomHex(32)
	s.mu.Lock()
	s.m[id] = &session{user: user, csrf: randomHex(16)}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
}

// sessionOf returns the session id and the session of the request, or nil.
func (s *sessions) sessionOf(r *http.Request) (string, *session) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.m[c.Value]; sess != nil {
		return c.Value, sess
	}
	return "", nil
}
