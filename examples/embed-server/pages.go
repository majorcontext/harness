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

//go:embed web/style.css
var styleCSS []byte

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Demo sign-in</title><link rel="stylesheet" href="/style.css">
<main class="page">
<h1>Sign in</h1>
<p class="dim">A stand-in for the sign-in of your application. It trusts any user name.</p>
<form method="post" action="/login"><p><label for="user">user</label> <input id="user" name="user" value="demo" required autocomplete="username" autofocus></p><button>Sign in</button></form>
</main>`))

var settingsTmpl = template.Must(template.New("settings").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>ChatGPT login</title><link rel="stylesheet" href="/style.css">
{{if .Pending}}<meta http-equiv="refresh" content="3">{{end}}
<main class="page">
<h1>ChatGPT login for {{.User}}</h1>
{{if .Connected}}
<p>Signed in. The server holds the token; this page and the model module never see it.</p>
<form method="post" action="/settings/codex/disconnect"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Disconnect</button></form>
{{else if .Pending}}
<p>1. Open <a href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.URL}}</a> and sign in.</p>
<p>2. Enter this one-time code (it expires in 15 minutes):</p>
<p class="code">{{.Code}}</p>
<p class="warn">Continue only if you started this sign-in.</p>
<p class="dim">This page checks every 3 seconds.</p>
{{else}}
{{if .Err}}<p class="err">The last sign-in failed: {{.Err}}</p>{{end}}
<form method="post" action="/settings/codex/start"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Sign in with ChatGPT</button></form>
{{end}}
<p class="dim"><a href="/">Back to the app</a></p>
</main>`))

func (s *server) loginPage(w http.ResponseWriter, r *http.Request) {
	_ = loginTmpl.Execute(w, nil)
}

func (s *server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	user := r.PostFormValue("user")
	if user == "" || len(user) > 64 || !sameOrigin(r) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Rotate: the old session id, if any, ends here.
	if old, _ := s.sessions.sessionOf(r); old != "" {
		if err := s.endSession(w, r, old); err != nil {
			http.Error(w, "the old session could not be ended; try again", http.StatusInternalServerError)
			return
		}
	}
	s.sessions.login(w, r, user)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// endSession ends the session and the sign-in flow that it started.
func (s *server) endSession(w http.ResponseWriter, r *http.Request, id string) error {
	if err := s.sessions.end(w, r, id); err != nil {
		return err
	}
	s.flows.drop(id)
	return nil
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	id, sess := s.sessions.sessionOf(r)
	if sess == nil || !sameOrigin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.endSession(w, r, id); err != nil {
		http.Error(w, "the logout could not be saved; try again", http.StatusInternalServerError)
		return
	}
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
	fl := s.flows.take(id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'")
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
	// Reserve the flow before the issuer I/O, so that logout, rotation, and
	// disconnect can cancel it. The reservation owns the context of the flow.
	ctx, cancel := context.WithCancel(context.Background())
	fl := &flow{cancel: cancel}
	s.flows.mu.Lock()
	if old := s.flows.m[id]; old != nil && old.cancel != nil {
		old.cancel()
	}
	s.flows.m[id] = fl
	s.flows.mu.Unlock()
	// A session that ended before the reservation: end sessions first and flows
	// second (endSession), so one of the two sides always sees the other.
	if !s.sessions.alive(id) {
		s.flows.drop(id)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	code, err := s.store.startDevice(ctx)

	s.flows.mu.Lock()
	if s.flows.m[id] != fl { // torn down or replaced during the issuer I/O
		s.flows.mu.Unlock()
		cancel()
		http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
		return
	}
	if err != nil {
		fl.done, fl.err = true, err.Error()
		s.flows.mu.Unlock()
		cancel()
		http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
		return
	}
	fl.code = code
	s.flows.mu.Unlock()
	go func() {
		defer cancel()
		s.runFlow(ctx, id, sess.user, fl, code)
	}()
	http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
}

// runFlow waits for the user to enter the code, then stores the login. A
// finished sign-in leaves nothing in the flows; a failed one stays until the
// page shows it.
func (s *server) runFlow(ctx context.Context, id, user string, fl *flow, code *deviceCode) {
	tok, err := s.store.wait(ctx, code)
	s.flows.mu.Lock()
	defer s.flows.mu.Unlock()
	if s.flows.m[id] != fl {
		return // replaced by a newer start, or the session ended
	}
	if err != nil {
		fl.done, fl.err = true, err.Error()
		slog.Warn("codex sign-in failed", "user", user, "err", err)
		return
	}
	delete(s.flows.m, id)
	s.store.Set(user, tok)
	slog.Info("codex sign-in finished", "user", user)
}

func (s *server) settingsDisconnect(w http.ResponseWriter, r *http.Request) {
	id, sess := s.sessions.sessionOf(r)
	if sess == nil || !csrfOK(r, sess) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.flows.drop(id)
	s.store.Delete(sess.user)
	http.Redirect(w, r, "/settings/codex", http.StatusSeeOther)
}
