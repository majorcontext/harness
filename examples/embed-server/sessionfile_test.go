package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNoSessionFileSetsASessionCookie(t *testing.T) {
	w := httptest.NewRecorder()
	newSessions(false).login(w, httptest.NewRequest("POST", "/login", nil), "ann")
	if c := w.Result().Cookies()[0]; c.MaxAge != 0 {
		t.Errorf("cookie MaxAge %d, want 0 without a session file", c.MaxAge)
	}
}

func TestSessionFileSkipsASessionWithNoCSRFToken(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(file, []byte(`{"abc":{"user":"ann","csrf":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSessions(false)
	s.file = file
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if s.alive("abc") {
		t.Error("a session with no CSRF token was loaded")
	}
}

func TestSessionFileKeepsSessionsAcrossRestarts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sessions.json")
	a := newSessions(false)
	a.file = file

	w := httptest.NewRecorder()
	a.login(w, httptest.NewRequest("POST", "/login", nil), "ann")
	cookie := w.Result().Cookies()[0]
	if cookie.MaxAge <= 0 {
		t.Errorf("cookie MaxAge %d, want a lasting cookie", cookie.MaxAge)
	}
	w2 := httptest.NewRecorder()
	a.login(w2, httptest.NewRequest("POST", "/login", nil), "bob")
	bob := w2.Result().Cookies()[0]
	if err := a.end(httptest.NewRecorder(), httptest.NewRequest("POST", "/logout", nil), bob.Value); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v, mode %v", err, info)
	}

	b := newSessions(false)
	b.file = file
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	req := func(c *http.Cookie) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(c)
		return r
	}
	_, sess := b.sessionOf(req(cookie))
	if sess == nil || sess.user != "ann" {
		t.Fatalf("after load the cookie of ann gives %+v", sess)
	}
	_, orig := a.sessionOf(req(cookie))
	if sess.csrf != orig.csrf {
		t.Error("the CSRF token changed across the restart")
	}
	if _, s := b.sessionOf(req(bob)); s != nil {
		t.Error("the session that ended came back")
	}
}

func TestEndKeepsTheSessionWhenTheFileIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	s := newSessions(false)
	s.file = filepath.Join(dir, "sessions.json")
	w := httptest.NewRecorder()
	s.login(w, httptest.NewRequest("POST", "/login", nil), "ann")
	id := w.Result().Cookies()[0].Value

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	out := httptest.NewRecorder()
	if err := s.end(out, httptest.NewRequest("POST", "/logout", nil), id); err == nil {
		t.Fatal("end succeeded with a session file that cannot be written")
	}
	if !s.alive(id) {
		t.Error("the session ended in memory although the file still holds it")
	}
	if len(out.Result().Cookies()) != 0 {
		t.Error("end cleared the cookie although the logout was not saved")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.end(httptest.NewRecorder(), httptest.NewRequest("POST", "/logout", nil), id); err != nil {
		t.Fatal(err)
	}
	reloaded := newSessions(false)
	reloaded.file = s.file
	if err := reloaded.load(); err != nil {
		t.Fatal(err)
	}
	if reloaded.alive(id) {
		t.Error("the session came back after a restart")
	}
}
