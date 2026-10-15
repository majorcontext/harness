# embed-server

A reference server for a page that runs the harness WebAssembly module with `auth: "injected"`. The module holds no model credential. This server holds the ChatGPT login of each user and adds it to the model requests. The design is `docs/design/proxy-auth.md`, section 8.

It is its own Go module. It imports `github.com/majorcontext/gatekeeper/proxy`, so the harness module and its `go build ./...` take no gatekeeper dependency.

## Run

From the repository root:

```bash
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -o /tmp/harness.wasm ./cmd/harness-wasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" /tmp/
cd examples/embed-server
go run . -wasm /tmp/harness.wasm -wasm-exec /tmp/wasm_exec.js
```

Open `http://127.0.0.1:8080/`:

1. Select "Sign in to the app" and sign in as any user name (a stand-in, see below).
2. Select "ChatGPT login". Open the link, enter the one-time code, and wait. The page shows "Signed in".
3. Go back to the first page, reload it, and send a prompt.

The `-embed` flag points to `sdk/embed/src` (default `../../sdk/embed/src`). `-upstream` sets the Codex backend.

Test: `go test -race ./...` in this directory. The test drives the middleware with a fake upstream and a fake issuer.

## What it does

| Path | Purpose |
| --- | --- |
| `/`, `/harness.wasm`, `/wasm_exec.js`, `/embed/` | The page, the module, and `sdk/embed`. |
| `/login`, `/logout`, `/whoami` | The stand-in session. Login rotates the session id. |
| `/settings/codex` | The `login_url`. It runs the device code flow against `auth.openai.com` on the server, shows the code and the link, and keeps the tokens of the user in memory. |
| `/model-proxy/` | The only door to the gatekeeper relay (`/relay/codex/`). |

The middleware on `/model-proxy/` checks the session cookie and the `Sec-Fetch-Site` header, allows only `/relay/codex/...`, and answers `401` itself when the store holds no usable login for the user. It then removes `Cookie`, `Origin`, `Referer`, `Sec-*`, and every credential header, and sets `X-App-User`. The resolver for `chatgpt.com` reads that header, removes it, and returns `Authorization` and `ChatGPT-Account-ID` from the store.

A token is refreshed when it ends within five minutes. When the backend answers `401`, `403`, or `429`, gatekeeper calls `Invalidate`, and the next request refreshes (at most once a minute). When the issuer rejects the refresh token, the login is dropped and the user gets `401`, which the module maps to `login_required`. No code logs or returns a token.

## Stand-ins

- `session.go` is a stand-in for the session of your application. `/login` trusts any user name and sets a random cookie. Replace it with your own session layer.
- The token store is memory. A real server keeps the tokens encrypted at rest, and survives a restart.
- Plain `http` on a loopback address. Serve a real deployment over `https` only. The `-secure` flag (`auto`, `true`, `false`) sets `Secure` on the session cookie; `auto` turns it on unless `-addr` is a loopback address, and the cookie also gets `Secure` on a TLS request.

## Security notes

- **Bind the flow to the session.** The device flow belongs to the session that started it, and its tokens go to the user of that session. The start and disconnect forms carry a per-session token and fail without it. Without this, page script (XSS) can finish a login with the account of an attacker.
- **CSRF.** `/settings/codex` and `/model-proxy/` use `SameSite=Lax` cookies and refuse a request that the browser marks `cross-site` or `same-site`. The POST forms also check the token.
- **A gatekeeper `Proxy` is a forward proxy.** Its relay branch depends on `r.URL.Host` being empty. An absolute-form request (`GET http://other-host/model-proxy/relay/codex/x`) or a `CONNECT` would be forwarded to that host, with the user's token added. The middleware refuses both with 400, rebuilds `r.URL` from the path and the query, and the test sends them as raw bytes. An app must never pass a gatekeeper `Proxy` an absolute-form request.
- **Never expose the gatekeeper relay directly.** The proxy has no listener and no authentication of its own. It accepts a request with no proxy credentials and relays it with the credential of whoever the middleware names in `X-App-User`. Mount it only behind the middleware, and keep the allowlist of paths.
- **The relay forwards every header** that the middleware does not remove. Keep the strip list.
- An XSS cannot read a token, but it can call the server as the user and spend the quota. Add rate limits, short sessions, and a strict CSP in a real deployment.
- Device code sign-in is a phishing target: the page tells the user to continue only if they started it.
