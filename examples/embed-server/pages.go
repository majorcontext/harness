package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html><meta charset="utf-8"><title>Demo sign-in</title>
<h1>Demo sign-in</h1>
<p>This is a stand-in for the sign-in of your application. It trusts any user name.</p>
<form method="post" action="/login"><input name="user" value="demo" required> <button>Sign in</button></form>`))

var settingsTmpl = template.Must(template.New("settings").Parse(`<!doctype html><meta charset="utf-8"><title>Codex login</title>
{{if .Pending}}<meta http-equiv="refresh" content="3">{{end}}
<h1>ChatGPT login for {{.User}}</h1>
{{if .Connected}}
<p>Signed in. The server holds the token; this page and the model module never see it.</p>
<form method="post" action="/settings/codex/disconnect"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Disconnect</button></form>
<p><a href="/">Back to the app</a></p>
{{else if .Pending}}
<p>1. Open <a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.URL}}</a> and sign in.</p>
<p>2. Enter this one-time code (it expires in 15 minutes):</p>
<p><code style="font-size:2em">{{.Code}}</code></p>
<p>Continue only if you started this sign-in. This page checks every 3 seconds.</p>
{{else}}
{{if .Err}}<p>The last sign-in failed: {{.Err}}</p>{{end}}
<form method="post" action="/settings/codex/start"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Sign in with ChatGPT</button></form>
{{end}}`))

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = loginTmpl.Execute(w, nil)
}

func (s *server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	user := r.PostFormValue("user")
	if user == "" || len(user) > 64 || !sameOrigin(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.sessions.login(w, r, user)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// whoami tells the page the user and whether the server holds a Codex login.
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	_, sess := s.sessions.sessionOf(r)
	out := struct {
		User  *string `json:"user"`
		Codex bool    `json:"codex"`
	}{}
	if sess != nil {
		out.User, out.Codex = &sess.user, s.store.Has(sess.user)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) settingsPage(w http.ResponseWriter, r *http.Request) {
	id, sess := s.sessions.sessionOf(r)
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	fl := s.flows.get(id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	data := map[string]any{"User": sess.user, "CSRF": sess.csrf, "Connected": s.store.Has(sess.user), "Err": fl.err}
	if fl.code != nil && !fl.done {
		data["Pending"], data["URL"], data["Code"] = true, fl.code.URL, fl.code.UserCode
	}
	_ = settingsTmpl.Execute(w, data)
}

// settingsStart begins the device code flow for the user of the session. The
// flow, and the tokens it yields, belong to that session's user only.
func (s *server) settingsStart(w http.ResponseWriter, r *http.Request) {
	id, sess := s.sessions.sessionOf(r)
	if sess == nil || !csrfOK(r, sess) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	code, err := s.store.startDevice(r.Context())
	s.flows.mu.Lock()
	if old := s.flows.m[id]; old != nil && old.cancel != nil {
		old.cancel()
	}
	if err != nil {
		s.flows.m[id] = &flow{err: err.Error()}
		s.flows.mu.Unlock()
		http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	fl := &flow{code: code, cancel: cancel}
	s.flows.m[id] = fl
	s.flows.mu.Unlock()

	user := sess.user
	go func() {
		defer cancel()
		tok, err := s.store.wait(ctx, code)
		s.flows.mu.Lock()
		defer s.flows.mu.Unlock()
		if s.flows.m[id] != fl {
			return // replaced by a newer start
		}
		fl.done = true
		if err != nil {
			fl.err = err.Error()
			slog.Warn("codex sign-in failed", "user", user, "err", err)
			return
		}
		s.store.Set(user, tok)
		slog.Info("codex sign-in finished", "user", user)
	}()
	http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
}

func (s *server) settingsDisconnect(w http.ResponseWriter, r *http.Request) {
	id, sess := s.sessions.sessionOf(r)
	if sess == nil || !csrfOK(r, sess) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.flows.mu.Lock()
	if fl := s.flows.m[id]; fl != nil && fl.cancel != nil {
		fl.cancel()
	}
	delete(s.flows.m, id)
	s.flows.mu.Unlock()
	s.store.Delete(sess.user)
	http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
}
