package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
)

// STAND-IN. This is not an authentication system. /login trusts any user name.
// A real application already has a session layer: replace this file and the
// /login and /logout routes with it, and keep only the contract: sessionOf(r)
// returns the session of the request, or nil. It rotates the session id on
// login and ends the session on logout; a real layer must do both.

const cookieName = "demo_session"

type session struct {
	user string
	csrf string // binds the forms of /settings/codex to this session
}

type sessions struct {
	secure bool // the cookie carries Secure even when the request is plain http
	mu     sync.Mutex
	m      map[string]*session

	// file, when set, keeps the sessions on disk so that a restart keeps them.
	file   string
	saveMu sync.Mutex
}

func newSessions(secure bool) *sessions { return &sessions{secure: secure, m: map[string]*session{}} }

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
	// A login that is not saved is lost at a restart, which only signs the user out.
	_ = s.save()
	c := &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secure || r.TLS != nil}
	if s.file != "" {
		// The session outlives the server, so the cookie outlives the browser.
		c.MaxAge = 30 * 24 * 60 * 60
	}
	http.SetCookie(w, c)
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

// end deletes the session and clears the cookie. When the session file cannot
// be written, the session stays and end returns the error: the file still
// holds the session, so a restart would bring the cookie back.
func (s *sessions) end(w http.ResponseWriter, r *http.Request, id string) error {
	s.mu.Lock()
	sess := s.m[id]
	delete(s.m, id)
	s.mu.Unlock()
	if err := s.save(); err != nil {
		s.mu.Lock()
		if sess != nil && s.m[id] == nil {
			s.m[id] = sess
		}
		s.mu.Unlock()
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secure || r.TLS != nil})
	return nil
}

// alive reports whether the session id is still valid.
func (s *sessions) alive(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id] != nil
}
