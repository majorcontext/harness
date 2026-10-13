package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// loginHost is a cliHost whose config names the scripted OAuth issuer. The
// third result is the stored login file.
func loginHost(t *testing.T, opts harnesstest.CodexIssuerOptions) (*cliHost, *harnesstest.CodexIssuer, string) {
	t.Helper()
	issuer := harnesstest.NewCodexIssuer(t, opts)
	h := newCLIHost(t, map[string]any{"codex_issuer": issuer.URL()}, replyText("unused"))
	return h, issuer, filepath.Join(filepath.Dir(h.config), "auth", "codex.json")
}

func noSecret(t *testing.T, what, text string) {
	t.Helper()
	if strings.Contains(text, "eyJ") || strings.Contains(text, "refresh-") {
		t.Errorf("%s holds a token:\n%s", what, text)
	}
}

func TestContractCLILoginDeviceCodeStoresTheLoginUntilLogout(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h, issuer, file := loginHost(t, harnesstest.CodexIssuerOptions{})
	out, errOut, code := h.run("login", "codex", "--device")
	if code != 0 {
		t.Fatalf("login = %d\n%s", code, errOut)
	}
	for _, want := range []string{harnesstest.CodexUserCode, issuer.URL() + "/codex/device"} {
		if !strings.Contains(out, want) {
			t.Errorf("login output lacks %q:\n%s", want, out)
		}
	}
	noSecret(t, "login output", out+errOut)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("login stored no file: %v", err)
	}
	if dir, _ := os.Stat(filepath.Dir(file)); info.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Errorf("file mode %v, directory mode %v, want 0600 and 0700", info.Mode().Perm(), dir.Mode().Perm())
	}
	if issuer.Exchanges() != 1 {
		t.Errorf("code exchanges = %d, want 1", issuer.Exchanges())
	}

	out, errOut, code = h.run("login", "codex", "--status")
	if code != 0 || !strings.Contains(out, "acct-1") || !strings.Contains(out, "refresh token: yes") {
		t.Errorf("status = %d %q, want the account and a refresh token\n%s", code, out, errOut)
	}
	noSecret(t, "status output", out+errOut)

	if _, errOut, code = h.run("logout", "codex"); code != 0 {
		t.Fatalf("logout = %d\n%s", code, errOut)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("logout left the file: %v", err)
	}
	out, errOut, code = h.run("login", "codex", "--status")
	if code != 1 || !strings.Contains(out+errOut, "not logged in") {
		t.Errorf("status after logout = %d %q %q, want 1 and not logged in", code, out, errOut)
	}
}

// browserLogin starts the browser sign-in without a browser and returns the
// printed sign-in URL and the wait for the exit code.
func browserLogin(t *testing.T, h *cliHost) (signIn *url.URL, wait func() int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, harnessBin, "login", "codex", "--no-browser")
	cmd.Dir = h.work
	cmd.Env = cleanEnv(map[string]string{"HARNESS_CONFIG": h.config, "PWD": h.work})
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if u, err := url.Parse(strings.TrimSpace(sc.Text())); err == nil && u.Scheme != "" && u.Query().Get("state") != "" {
			go func() { _, _ = io.Copy(io.Discard, out) }()
			return u, func() int {
				_ = cmd.Wait()
				return cmd.ProcessState.ExitCode()
			}
		}
	}
	t.Fatal("login printed no sign-in URL")
	return nil, nil
}

// callback sends the one request of the browser to the redirect URI of the sign-in and returns the status.
// It speaks HTTP over a socket, because the callback is no route of the API that the wire client checks.
func callback(t *testing.T, signIn *url.URL, code, state string) int {
	t.Helper()
	redirect, err := url.Parse(signIn.Query().Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", redirect.Host)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = conn.Close() }()
	q := url.Values{"code": {code}, "state": {state}}
	if _, err := fmt.Fprintf(conn, "GET %s?%s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", redirect.Path, q.Encode(), redirect.Host); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	return res.StatusCode
}

func TestContractCLILoginBrowserCallbackStoresTheLogin(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h, issuer, file := loginHost(t, harnesstest.CodexIssuerOptions{})
	signIn, wait := browserLogin(t, h)
	q := signIn.Query()
	issuer.ExpectChallenge(q.Get("code_challenge"))
	if !strings.HasPrefix(q.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want a 127.0.0.1 address", q.Get("redirect_uri"))
	}
	for k, want := range map[string]string{
		"client_id": harnesstest.CodexClientID, "response_type": "code", "code_challenge_method": "S256",
		"originator": "harness", "codex_cli_simplified_flow": "true", "id_token_add_organizations": "true",
	} {
		if q.Get(k) != want {
			t.Errorf("sign-in URL %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.HasPrefix(signIn.String(), issuer.URL()+"/oauth/authorize?") || q.Get("code_challenge") == "" || !strings.Contains(q.Get("scope"), "offline_access") {
		t.Errorf("sign-in URL = %s, want the authorize endpoint with a challenge and offline_access", signIn)
	}
	if got := callback(t, signIn, harnesstest.CodexAuthCode, q.Get("state")); got != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", got)
	}
	if code := wait(); code != 0 {
		t.Fatalf("login exit = %d, want 0", code)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("login stored no file: %v", err)
	}
	if got, want := issuer.LastRedirectURI(), q.Get("redirect_uri"); got != want {
		t.Errorf("exchange redirect_uri = %q, want %q", got, want)
	}
}

func TestContractCLILoginBrowserCallbackWithAnotherStateKeepsWaiting(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h, issuer, file := loginHost(t, harnesstest.CodexIssuerOptions{})
	signIn, wait := browserLogin(t, h)
	issuer.ExpectChallenge(signIn.Query().Get("code_challenge"))
	if got := callback(t, signIn, harnesstest.CodexAuthCode, "other"); got != http.StatusBadRequest {
		t.Errorf("callback status = %d, want 400", got)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) || issuer.Exchanges() != 0 {
		t.Errorf("file %v, exchanges %d, want no file and no exchange after the wrong state", err, issuer.Exchanges())
	}
	if got := callback(t, signIn, harnesstest.CodexAuthCode, signIn.Query().Get("state")); got != http.StatusOK {
		t.Fatalf("second callback status = %d, want 200", got)
	}
	if code := wait(); code != 0 {
		t.Errorf("login exit = %d, want 0", code)
	}
}

func TestContractCLILoginRefusesAFileThatOthersCanRead(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h, _, file := loginHost(t, harnesstest.CodexIssuerOptions{})
	if _, errOut, code := h.run("login", "codex", "--device"); code != 0 {
		t.Fatalf("login = %d\n%s", code, errOut)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := h.run("login", "codex", "--status")
	if code != 1 || !strings.Contains(out+errOut, "chmod 600") {
		t.Errorf("status = %d %q %q, want 1 and the chmod 600 advice", code, out, errOut)
	}
	noSecret(t, "status output", out+errOut)
}
