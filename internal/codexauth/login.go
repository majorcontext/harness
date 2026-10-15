package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	deviceTimeout = 15 * time.Minute
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

// DeviceCode is a device sign-in that the issuer started and the user has not finished.
type DeviceCode struct {
	// UserCode is the one-time code that the user enters.
	UserCode string
	// VerificationURL is the page where the user enters the code.
	VerificationURL string

	o        LoginOptions
	id       string
	interval time.Duration
}

// URL returns the verification page with the user code filled in.
func (d *DeviceCode) URL() string {
	return d.VerificationURL + "?" + url.Values{"user_code": {d.UserCode}}.Encode()
}

// StartDevice asks the issuer for a device code. It reads and writes no file.
func StartDevice(ctx context.Context, o LoginOptions) (*DeviceCode, error) {
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
		return nil, err
	case status == http.StatusNotFound:
		return nil, errors.New("codex login: device code sign-in is not enabled for this account; use the browser sign-in")
	case status != http.StatusOK:
		return nil, fmt.Errorf("codex login: the device code request failed: HTTP %d", status)
	}
	if uc.UserCode == "" {
		uc.UserCode = uc.Alias
	}
	if uc.UserCode == "" || uc.DeviceAuthID == "" {
		return nil, errors.New("codex login: the device code answer is incomplete")
	}
	return &DeviceCode{UserCode: uc.UserCode, VerificationURL: o.issuer() + "/codex/device", o: o, id: uc.DeviceAuthID, interval: intervalOf(uc.Interval)}, nil
}

// Wait polls the issuer at the interval that it gave, until the user enters the code or the sign-in expires. Then it exchanges the grant and stores the login.
func (d *DeviceCode) Wait(ctx context.Context) error {
	o := d.o
	grant, err := pollDevice(ctx, o, o.issuer()+"/api/accounts/deviceauth/token", d.id, d.UserCode, d.interval)
	if err != nil {
		return err
	}
	return exchange(ctx, o, "authorization_code", grant.AuthorizationCode, grant.CodeVerifier, o.issuer()+"/deviceauth/callback")
}

// LoginDevice runs the device code flow: it prints a code and the URL where the user enters it, then polls until the user did.
func LoginDevice(ctx context.Context, o LoginOptions) error {
	d, err := StartDevice(ctx, o)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(o.Out, "Follow these steps to sign in with ChatGPT:\n\n1. Open this link in your browser and sign in:\n   %s\n\n2. Enter this one-time code (it expires in 15 minutes):\n   %s\n\nContinue only if you started this sign-in.\n", d.VerificationURL, d.UserCode)
	return d.Wait(ctx)
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

// slowDownStep is what a slow_down answer adds to the polling interval (RFC 8628, section 3.5).
const slowDownStep = 5 * time.Second

// pollDevice asks for the grant until the user enters the code. A 403 or 404, and an authorization_pending answer, mean not yet.
// A slow_down answer adds slowDownStep to the interval. A sign-in that ctx ends fails with the error of ctx;
// one that deviceTimeout ends is expired.
func pollDevice(ctx context.Context, o LoginOptions, endpoint, id, code string, interval time.Duration) (deviceGrant, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, deviceTimeout)
	defer cancel()
	for {
		var g deviceGrant
		status, raw, err := postRaw(ctx, o.client(), endpoint, map[string]string{"device_auth_id": id, "user_code": code})
		if err != nil {
			return g, err
		}
		switch {
		case status == http.StatusOK:
			if json.Unmarshal(raw, &g) != nil {
				return g, errors.New("codex login: the issuer sent an answer that is not JSON")
			}
			if g.AuthorizationCode != "" {
				return g, nil
			}
			return g, fmt.Errorf("codex login: the device sign-in failed: HTTP %d", status)
		case status == http.StatusForbidden, status == http.StatusNotFound:
		case status == http.StatusBadRequest && errorCode(raw) == "slow_down":
			interval += slowDownStep
		case status == http.StatusBadRequest && errorCode(raw) == "authorization_pending":
		default:
			return g, fmt.Errorf("codex login: the device sign-in failed: HTTP %d", status)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			if parent.Err() != nil {
				return g, fmt.Errorf("codex login: %w", context.Cause(parent))
			}
			return g, errors.New("codex login: the device code expired before it was entered")
		case <-t.C:
		}
	}
}

// errorCode reads the error member of an OAuth error answer.
func errorCode(raw []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Error
}

// postJSON posts body as JSON and decodes a 200 answer into out. It returns the status.
func postJSON(ctx context.Context, hc *http.Client, endpoint string, body, out any) (int, error) {
	status, raw, err := postRaw(ctx, hc, endpoint, body)
	if err == nil && status == http.StatusOK && out != nil {
		if json.Unmarshal(raw, out) != nil {
			return status, errors.New("codex login: the issuer sent an answer that is not JSON")
		}
	}
	return status, err
}

// postRaw posts body as JSON and returns the status and the answer.
func postRaw(ctx context.Context, hc *http.Client, endpoint string, body any) (int, []byte, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRaw(hc, req)
}

func do(hc *http.Client, req *http.Request, out any) (int, error) {
	status, raw, err := doRaw(hc, req)
	if err == nil && status == http.StatusOK && out != nil {
		if json.Unmarshal(raw, out) != nil {
			return status, errors.New("codex login: the issuer sent an answer that is not JSON")
		}
	}
	return status, err
}

func doRaw(hc *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("codex login: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
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
