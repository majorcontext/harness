//go:build !js

package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	// browserPort is the redirect port that the Codex OAuth client registers.
	// Source: codex-rs/login/src/server.rs DEFAULT_PORT.
	browserPort = "1455"
	// fallbackPort is the second redirect port, for when browserPort is busy. Source: codex-rs/login/src/server.rs FALLBACK_PORT.
	fallbackPort = "1457"
	// scope is the scope that the Codex CLI asks for. Source: codex-rs/login/src/server.rs build_authorize_url.
	scope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	loginTimeout = 10 * time.Minute
)

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// LoginBrowser runs the authorization code flow with PKCE. It prints the
// sign-in URL, waits for the one callback of the browser on a loopback
// listener, exchanges the code, and stores the login.
func LoginBrowser(ctx context.Context, o LoginOptions) error {
	verifier, err := randomSecret()
	if err != nil {
		return err
	}
	state, err := randomSecret()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	ln, err := listenCallback(o.issuer())
	if err != nil {
		return fmt.Errorf("codex login: the callback ports are busy: %w (run `%s --device` instead)", err, LoginCommand)
	}
	// The Codex CLI redirects to 127.0.0.1 (codex-rs/login/src/server.rs:194).
	redirect := "http://127.0.0.1:" + portOf(ln) + "/auth/callback"
	signIn := o.issuer() + "/oauth/authorize?" + url.Values{
		"response_type":              {"code"},
		"client_id":                  {ClientID},
		"redirect_uri":               {redirect},
		"scope":                      {scope},
		"code_challenge":             {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {Originator},
	}.Encode()
	_, _ = fmt.Fprintf(o.Out, "Open this URL in your browser to sign in with ChatGPT:\n\n%s\n\n", signIn)
	if o.Open != nil {
		_ = o.Open(signIn)
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()
	code, err := waitForCallback(ctx, ln, state)
	if err != nil {
		return err
	}
	return exchange(ctx, o, "authorization_code", code, verifier, redirect)
}

// listenCallback listens for the browser callback on loopback. Only the default
// issuer knows the registered redirect ports, so another issuer (a test issuer) gets a free port.
func listenCallback(issuer string) (net.Listener, error) {
	ports := []string{"0"}
	if issuer == DefaultIssuer {
		ports = []string{browserPort, fallbackPort}
	}
	var err error
	for _, port := range ports {
		var ln net.Listener
		if ln, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", port)); err == nil {
			return ln, nil
		}
	}
	return nil, err
}

func portOf(ln net.Listener) string {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

type callbackResult struct {
	code string
	err  error
}

// waitForCallback serves /auth/callback on ln until the first request to it, and returns its code.
func waitForCallback(ctx context.Context, ln net.Listener, state string) (string, error) {
	done := make(chan callbackResult, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res callbackResult
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch {
		case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1, q.Get("code") == "" && q.Get("error") == "":
			// A request from another sign-in, or a stray one, ends nothing: the real callback can still come.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "This is not the callback of the sign-in that is waiting.\n")
			return
		case q.Get("error") != "":
			res.err = fmt.Errorf("codex login: the sign-in was refused: %q", q.Get("error"))
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Sign-in failed. Return to the terminal.\n")
		default:
			res.code = q.Get("code")
			_, _ = io.WriteString(w, "Signed in. You can close this tab.\n")
		}
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		// A cancel that comes before Serve registers the listener can leave it open after Shutdown.
		_ = ln.Close()
	}()
	select {
	case res := <-done:
		return res.code, res.err
	case <-ctx.Done():
		return "", fmt.Errorf("codex login: no sign-in before the end: %w", ctx.Err())
	}
}
