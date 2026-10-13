package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// browserPort is the redirect port that the Codex OAuth client registers.
	// Source: codex-rs/login/src/server.rs DEFAULT_PORT.
	browserPort = "1455"
	// scope is the scope that the Codex CLI asks for. Source: codex-rs/login/src/server.rs build_authorize_url.
	scope         = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	deviceTimeout = 15 * time.Minute
	loginTimeout  = 10 * time.Minute
)

// LoginOptions configure a sign-in.
type LoginOptions struct {
	// Store receives the login.
	Store Store
	// Issuer is the OAuth issuer. Empty is DefaultIssuer.
	Issuer string
	// HTTP sends the requests to the issuer. nil: a client with a 30 s timeout.
	HTTP *http.Client
	// Out receives the instructions for the user. They hold no token.
	Out io.Writer
	// Open opens the sign-in URL in a browser. nil: the user opens it.
	Open func(url string) error
}

func (o *LoginOptions) issuer() string {
	if o.Issuer == "" {
		return DefaultIssuer
	}
	return strings.TrimRight(o.Issuer, "/")
}

func (o *LoginOptions) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
}

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
	// Only the default issuer knows the registered redirect port. Another issuer is a test issuer.
	port := "0"
	if o.issuer() == DefaultIssuer {
		port = browserPort
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return fmt.Errorf("codex login: the callback port is busy: %w (run `%s --device` instead)", err, LoginCommand)
	}
	redirect := "http://localhost:" + portOf(ln) + "/auth/callback"
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
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("codex login: the sign-in was refused: %s", q.Get("error"))
		case subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1:
			res.err = errors.New("codex login: the callback is from another sign-in (state mismatch)")
		case q.Get("code") == "":
			res.err = errors.New("codex login: the callback holds no code")
		default:
			res.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if res.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "Sign-in failed. Return to the terminal.\n")
		} else {
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
	}()
	select {
	case res := <-done:
		return res.code, res.err
	case <-ctx.Done():
		return "", fmt.Errorf("codex login: no sign-in before the end: %w", ctx.Err())
	}
}

// LoginDevice runs the device code flow: it prints a code and the URL where the user enters it, then polls until the user did.
func LoginDevice(ctx context.Context, o LoginOptions) error {
	api := o.issuer() + "/api/accounts/deviceauth"
	var uc struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Alias        string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
	}
	status, err := postJSON(ctx, o.client(), api+"/usercode", map[string]string{"client_id": ClientID}, &uc)
	switch {
	case err != nil:
		return err
	case status == http.StatusNotFound:
		return errors.New("codex login: device code sign-in is not enabled for this account; use the browser sign-in")
	case status != http.StatusOK:
		return fmt.Errorf("codex login: the device code request failed: HTTP %d", status)
	}
	if uc.UserCode == "" {
		uc.UserCode = uc.Alias
	}
	if uc.UserCode == "" || uc.DeviceAuthID == "" {
		return errors.New("codex login: the device code answer is incomplete")
	}
	_, _ = fmt.Fprintf(o.Out, "Follow these steps to sign in with ChatGPT:\n\n1. Open this link in your browser and sign in:\n   %s/codex/device\n\n2. Enter this one-time code (it expires in 15 minutes):\n   %s\n\nContinue only if you started this sign-in.\n", o.issuer(), uc.UserCode)
	grant, err := pollDevice(ctx, o, api+"/token", uc.DeviceAuthID, uc.UserCode, intervalOf(uc.Interval))
	if err != nil {
		return err
	}
	return exchange(ctx, o, "authorization_code", grant.AuthorizationCode, grant.CodeVerifier, o.issuer()+"/deviceauth/callback")
}

// intervalOf reads the poll interval, which the issuer sends as a string or a number.
func intervalOf(raw json.RawMessage) time.Duration {
	var n int
	if json.Unmarshal(raw, &n) != nil {
		var s string
		_ = json.Unmarshal(raw, &s)
		fmt.Sscanf(s, "%d", &n) //nolint:errcheck // 0 on a bad value
	}
	return time.Duration(max(n, 1)) * time.Second
}

type deviceGrant struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
}

// pollDevice asks for the grant until the user enters the code. A 403 or 404 means not yet.
func pollDevice(ctx context.Context, o LoginOptions, endpoint, id, code string, interval time.Duration) (deviceGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	for {
		var g deviceGrant
		status, err := postJSON(ctx, o.client(), endpoint, map[string]string{"device_auth_id": id, "user_code": code}, &g)
		switch {
		case err != nil:
			return g, err
		case status == http.StatusOK && g.AuthorizationCode != "":
			return g, nil
		case status != http.StatusForbidden && status != http.StatusNotFound:
			return g, fmt.Errorf("codex login: the device sign-in failed: HTTP %d", status)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return g, errors.New("codex login: the device code expired before it was entered")
		case <-t.C:
		}
	}
}

// postJSON posts body as JSON and decodes a 200 answer into out. It returns the status.
func postJSON(ctx context.Context, hc *http.Client, endpoint string, body, out any) (int, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(hc, req, out)
}

func do(hc *http.Client, req *http.Request, out any) (int, error) {
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, fmt.Errorf("codex login: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, errors.New("codex login: the issuer sent an answer that is not JSON")
		}
	}
	return resp.StatusCode, nil
}

// exchange trades an authorization code for tokens and stores the login.
func exchange(ctx context.Context, o LoginOptions, grantType, code, verifier, redirect string) error {
	form := url.Values{"grant_type": {grantType}, "client_id": {ClientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirect}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.issuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	status, err := do(o.client(), req, &tok)
	switch {
	case err != nil:
		return err
	case status == http.StatusBadRequest || status == http.StatusUnauthorized:
		return errors.New("codex login: the sign-in expired or was used already; start again")
	case status != http.StatusOK || tok.AccessToken == "":
		return fmt.Errorf("codex login: the issuer could not finish the sign-in: HTTP %d", status)
	}
	account := AccountFromToken(tok.AccessToken)
	if account == "" {
		account = AccountFromToken(tok.IDToken)
	}
	if account == "" {
		return errors.New("codex login: the sign-in token names no ChatGPT account")
	}
	unlock, err := o.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return o.Store.Save(Credentials{AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, AccountID: account})
}
