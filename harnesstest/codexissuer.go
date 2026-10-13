package harnesstest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// CodexClientID is the OAuth client id that the issuer requires.
const CodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

const (
	// CodexInitialRefresh is the refresh token that a sign-in receives.
	CodexInitialRefresh = "refresh-0"
	// CodexAuthCode is the authorization code that the issuer accepts from a browser sign-in.
	CodexAuthCode = "browser-code"
	// CodexUserCode is the user code of the device flow.
	CodexUserCode       = "ABCD-1234"
	codexDeviceID       = "device-1"
	codexDeviceCd       = "device-code"
	codexDeviceVerifier = "device-verifier"
	authClaim           = "https://api.openai.com/auth"
)

// CodexIssuerOptions scripts a CodexIssuer.
type CodexIssuerOptions struct {
	// Lifetime is the lifetime of an access token. 0 is one hour.
	Lifetime time.Duration
	// Rotate makes a refresh answer carry a new refresh token.
	Rotate bool
	// RefreshStatus, when not 0, is the HTTP status of every refresh grant.
	RefreshStatus int
	// DevicePending is the number of device polls that answer 403 before the code is entered.
	DevicePending int
	// AccountID is the chatgpt_account_id claim. Empty is "acct-1".
	AccountID string
	// Secret, when set, is a value that no request header may hold. SawSecret reports a request that did.
	Secret string
	// HoldRefresh makes a refresh grant wait for ReleaseRefresh before it answers.
	HoldRefresh bool
}

// CodexIssuer is a scripted OpenAI OAuth issuer for the Codex sign-in: the
// token endpoint (authorization code and refresh grants) and the device code
// endpoints. Its access tokens are JWTs that name their issue number.
type CodexIssuer struct {
	srv  *httptest.Server
	opts CodexIssuerOptions

	mu           sync.Mutex
	issued       int
	refreshes    int
	exchanges    int
	polls        int
	valid        map[string]bool // refresh tokens that a refresh grant accepts
	lastRedirect string
	secretSeen   bool
	challenge    string // the code_challenge that a browser sign-in sent
	arrived      chan struct{}
	gate         chan struct{}
}

// NewCodexIssuer starts an issuer that t closes.
func NewCodexIssuer(t testing.TB, opts CodexIssuerOptions) *CodexIssuer {
	t.Helper()
	i := &CodexIssuer{opts: opts, valid: map[string]bool{CodexInitialRefresh: true}, arrived: make(chan struct{}), gate: make(chan struct{})}
	i.srv = httptest.NewServer(http.HandlerFunc(i.serve))
	t.Cleanup(i.srv.Close)
	return i
}

func (i *CodexIssuer) serve(w http.ResponseWriter, r *http.Request) {
	if i.opts.Secret != "" && headersHold(r.Header, i.opts.Secret) {
		i.mu.Lock()
		i.secretSeen = true
		i.mu.Unlock()
	}
	switch {
	case r.Method != http.MethodPost:
		http.NotFound(w, r)
	case r.URL.Path == "/oauth/token":
		i.token(w, r)
	case r.URL.Path == "/api/accounts/deviceauth/usercode":
		i.userCode(w, r)
	case r.URL.Path == "/api/accounts/deviceauth/token":
		i.devicePoll(w, r)
	default:
		http.NotFound(w, r)
	}
}

// SawSecret reports whether a request carried CodexIssuerOptions.Secret in a header.
func (i *CodexIssuer) SawSecret() bool { i.mu.Lock(); defer i.mu.Unlock(); return i.secretSeen }

// ExpectChallenge sets the code_challenge of the browser sign-in. The issuer
// accepts the browser code only with a verifier whose S256 hash is the challenge.
func (i *CodexIssuer) ExpectChallenge(challenge string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.challenge = challenge
}

// AwaitRefresh reports whether a refresh grant is waiting at the issuer within bound. It needs HoldRefresh.
func (i *CodexIssuer) AwaitRefresh(bound time.Duration) bool {
	select {
	case <-i.arrived:
		return true
	case <-time.After(bound):
		return false
	}
}

// ReleaseRefresh lets the held refresh grants answer.
func (i *CodexIssuer) ReleaseRefresh() { closeOnceCh(i.gate) }

func closeOnceCh(c chan struct{}) {
	select {
	case <-c:
	default:
		close(c)
	}
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// URL is the base URL of the issuer.
func (i *CodexIssuer) URL() string { return i.srv.URL }

// Refreshes counts the refresh grants that the issuer answered.
func (i *CodexIssuer) Refreshes() int { i.mu.Lock(); defer i.mu.Unlock(); return i.refreshes }

// Exchanges counts the authorization code grants that the issuer accepted.
func (i *CodexIssuer) Exchanges() int { i.mu.Lock(); defer i.mu.Unlock(); return i.exchanges }

// Polls counts the device polls.
func (i *CodexIssuer) Polls() int { i.mu.Lock(); defer i.mu.Unlock(); return i.polls }

// LastRedirectURI is the redirect_uri of the newest accepted authorization code grant.
func (i *CodexIssuer) LastRedirectURI() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastRedirect
}

func (i *CodexIssuer) account() string {
	if i.opts.AccountID != "" {
		return i.opts.AccountID
	}
	return "acct-1"
}

// FakeJWT returns an unsigned JWT with claims.
func FakeJWT(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

// AccessToken returns an access token with the label, an account claim, and
// the lifetime from now. The label is what TokenLabel reports.
func AccessToken(label, account string, lifetime time.Duration) string {
	return FakeJWT(map[string]any{
		"jti": label, "exp": time.Now().Add(lifetime).Unix(),
		authClaim: map[string]any{"chatgpt_account_id": account},
	})
}

// TokenLabel reports the label of an access token from AccessToken.
func TokenLabel(token string) (string, bool) {
	var parts [3]string
	n, start := 0, 0
	for k := 0; k <= len(token) && n < 3; k++ {
		if k == len(token) || token[k] == '.' {
			parts[n] = token[start:k]
			n++
			start = k + 1
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		JTI string `json:"jti"`
	}
	if json.Unmarshal(raw, &c) != nil || c.JTI == "" {
		return "", false
	}
	return c.JTI, true
}

// issue answers a grant with a new access token, and with refresh when it is not empty. The caller holds mu.
func (i *CodexIssuer) issue(w http.ResponseWriter, refresh string) {
	i.issued++
	life := i.opts.Lifetime
	if life == 0 {
		life = time.Hour
	}
	out := map[string]any{
		"access_token": AccessToken("access-"+strconv.Itoa(i.issued), i.account(), life),
		"id_token":     FakeJWT(map[string]any{authClaim: map[string]any{"chatgpt_account_id": i.account()}}),
		"expires_in":   int(life.Seconds()),
	}
	if refresh != "" {
		out["refresh_token"] = refresh
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, code)
}

func (i *CodexIssuer) token(w http.ResponseWriter, r *http.Request) {
	var f map[string]string
	if r.Header.Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(r.Body).Decode(&f)
	} else {
		_ = r.ParseForm()
		f = map[string]string{}
		for k := range r.PostForm {
			f[k] = r.PostForm.Get(k)
		}
	}
	if f["client_id"] != CodexClientID {
		oauthError(w, http.StatusBadRequest, "invalid_client")
		return
	}
	if f["grant_type"] == "refresh_token" && i.opts.HoldRefresh {
		closeOnceCh(i.arrived)
		<-i.gate
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	switch f["grant_type"] {
	case "refresh_token":
		i.refreshes++
		switch {
		case i.opts.RefreshStatus != 0:
			oauthError(w, i.opts.RefreshStatus, "invalid_grant")
		case !i.valid[f["refresh_token"]]:
			oauthError(w, http.StatusBadRequest, "invalid_grant")
		default:
			next := ""
			if i.opts.Rotate {
				next = "refresh-" + strconv.Itoa(i.issued+1)
				i.valid[next] = true
				delete(i.valid, f["refresh_token"])
			}
			i.issue(w, next)
		}
	case "authorization_code":
		want := i.challenge
		if f["code"] == codexDeviceCd {
			want = s256(codexDeviceVerifier)
		}
		if (f["code"] != CodexAuthCode && f["code"] != codexDeviceCd) || want == "" || s256(f["code_verifier"]) != want || f["redirect_uri"] == "" {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		i.exchanges++
		i.lastRedirect = f["redirect_uri"]
		i.valid[CodexInitialRefresh] = true
		i.issue(w, CodexInitialRefresh)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (i *CodexIssuer) userCode(w http.ResponseWriter, r *http.Request) {
	var f map[string]string
	_ = json.NewDecoder(r.Body).Decode(&f)
	if f["client_id"] != CodexClientID {
		oauthError(w, http.StatusBadRequest, "invalid_client")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"device_auth_id": codexDeviceID, "user_code": CodexUserCode, "interval": "1"})
}

func (i *CodexIssuer) devicePoll(w http.ResponseWriter, r *http.Request) {
	var f map[string]string
	_ = json.NewDecoder(r.Body).Decode(&f)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.polls++
	if f["device_auth_id"] != codexDeviceID || f["user_code"] != CodexUserCode {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if i.polls <= i.opts.DevicePending {
		oauthError(w, http.StatusForbidden, "authorization_pending")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"authorization_code": codexDeviceCd, "code_challenge": s256(codexDeviceVerifier), "code_verifier": codexDeviceVerifier})
}
