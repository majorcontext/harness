# Injected auth: a server injects the model credential

A model credential of a user must not live in a browser, in a module, or in a box. A server holds it. Harness sends the request with no credential, and the server adds it.

This document defines the provider auth mode `injected` and the login that the host delegates. The change is in `majorcontext/harness` only. Gatekeeper and boxes need no change, and their behavior does not change (sections 8 and 9).

## 1. Decisions

1. `auth: "injected"` is a credential source of a provider entry, like `auth: "chatgpt"`. It says: a credential-injecting server in front of this endpoint holds the credential and adds it to the request. It is valid on an entry of type `openai` or `anthropic`, and not with `api_key_env`. It does not name ChatGPT. The word "proxy" is not used for the mode, because `base_url` need not point at a proxy, and `harnessConnect` already has `proxy.*` options with another meaning.
2. `base_url` need not point at the server. A browser sets it to a path of the page origin. A box keeps the real URL, and a forward proxy (`HTTPS_PROXY`) adds the credential. The mode means "Harness holds no credential".
3. Harness sends no `Authorization`, no `x-api-key`, no `ChatGPT-Account-ID`, and no `originator` on such an entry. The server owns the backend contract.
4. Harness sends no user or session identifier. The server knows the caller from its own channel (section 4).
5. The server signals "no credential for this user" with HTTP 401. Only 401. Harness maps it to `login_required`.
6. The host delegates the login to a URL (`login_url`). `authenticate` shows it as a URL elicitation, then polls a status probe until the probe succeeds. The host reports nothing to Harness.
7. Wasm has the mode `injected` only. Native keeps the file login (`harness login codex`). This removes the Node device flow too.
8. The reference server keeps its own copy of the device-flow code. Harness exports no login package.

Three choices that a reader may not expect:

- 403 is not `login_required`. A 403 comes from Cloudflare challenges, from policy denials of the server (gatekeeper answers 403 for a Keep policy denial), and from the backend for a plan limit. A login cannot resolve those, and a login prompt would loop.
- The host does not report "done". ACP has no method for it (section 5), and the server already knows. The status probe is the one source of truth.
- Harness sends no identity header. Both deployments already carry identity in a channel that the server trusts more than a header (section 4).

## 2. Config

```json
{"providers": {"codex": {
  "type": "openai", "auth": "injected",
  "base_url": "https://chatgpt.com/backend-api/codex", "responses_path": "/responses",
  "omit_response_params": ["max_output_tokens", "temperature", "top_p", "metadata"],
  "sanitize_tool_schemas": true, "models_from": "chatgpt",
  "login_url": "https://console.example.com/settings/codex"}}}
```

- `auth: "injected"`: the adapter builds with no key and sets no credential header. `extra_headers` still apply, and so does `proxy.headers` in wasm.
- `login_url`: the page where the user signs in. It is absolute and `https`, or `http` on a loopback host. It is optional. It is valid only with `auth: "injected"`. It holds no secret and no per-user value, because it appears in error text and in logs.
- `models_from`: `"chatgpt"` says that the model list of the entry is the ChatGPT backend list (`GET <base_url>/models?client_version=<version>`, the fetch of `ChatGPT subscription login`). It is separate from `auth`, and it is valid on a `type: openai` entry with any auth. The fetch keys on this field alone. The native `codex` entry carries `models_from: "chatgpt"`, so no code branches on the provider name or on `auth` for the fetch. An entry that sets `auth: "chatgpt"` and no `models_from` has the static list. Any other value is invalid.
- A project file sets none of `auth`, `login_url`, and `models_from`. A project file could otherwise point the user to a page of its own. `rejectProjectProviderKeys` lists `login_url` and `models_from` next to `auth`.
- A `base_url` of an `injected` entry has no scheme rule, because Harness sends no credential to it.
- A `providers` entry of the same name replaces the native entry as a whole, so a deployment of the Codex lane writes eight keys. This document adds no overlay rule.

`claude-code-cli` has no injected mode: the CLI holds its own credential. `openai-compat` has none in this version.

## 3. Requests and the "no credential" signal

A request to an `injected` entry carries no credential. The server answers:

| Answer | Meaning | Harness |
| --- | --- | --- |
| 2xx | Signed in | Normal. |
| 401 | The server has no usable credential for this caller | A permanent error that implements `LoginRequired()`. The turn ends `failed` with class `login_required`. No retry, no refresh. |
| 3xx | A redirect | An error. Harness follows no redirect on an `injected` entry. |
| Other | As for any provider | Unchanged retry rules. A 502 from a forward proxy for a missing credential is a plain retryable error. |

- A server must answer 401, never a redirect to a sign-in page, for a request that it cannot authorize. A 401 that the backend sends (the server forwarded a stale token) has the same meaning. A 401 needs a `WWW-Authenticate` header by RFC 9110. Use `Bearer realm="harness"` with no `error` parameter, or `error="invalid_token"`. Harness reads the status only.
- The status probe (section 6) is authorized exactly like a model call: the same session, the same credential lookup, the same checks, and no cache of its answer. A server that answers the probe from a weaker check, or from a cache, can say 200 and then answer 401 to the model call.
- If a model call answers 401 within 2 minutes after a probe of the same entry answered 200, the turn does not fail with `login_required`. It ends `permanent`, with the message `the server accepted the sign-in check but rejected the model call; sign in again at <login_url>, or check how the server authorizes model calls` (without the URL part when the entry has no `login_url`), because a real revocation between the probe and the call gives the same signal, and the entry's probe cache holds "signed in, model calls rejected" until the next `authenticate`. Then `authenticate` → 200 probe → 401 on the call → `authenticate` cannot loop: the second pass reports the server fault and stops.
- The message of a `login_required` error is `not logged in to <provider>; sign in at <login_url>`, or `not logged in to <provider>` without a URL. It holds no header value.
- The websocket handshake of the Codex lane follows the same rule: a 401 fails the request and does not fall back to HTTP.

### Injection styles

Two styles of injection exist, and `injected` serves both.

- Add-or-set injection: the server sets the credential headers whether or not the request has them. Gatekeeper `SetCredentialWithGrant`, moat's Claude grant, boxes, and the reference server of section 8 work this way. Harness sends nothing for it.
- Placeholder substitution: the server replaces a placeholder that the client sends, and injects nothing when the placeholder is absent. Moat's Codex grant is a credential bundle on `chatgpt.com`, scoped to `/backend-api/codex`, with `RequireAll`: it replaces the placeholder `Authorization` and `ChatGPT-Account-ID` together. For this style the entry sets the placeholders in `extra_headers`, which stay valid with `auth: "injected"`:

```json
{"type": "openai", "auth": "injected", "base_url": "https://chatgpt.com/backend-api/codex",
 "responses_path": "/responses", "models_from": "chatgpt",
 "extra_headers": {"Authorization": "Bearer <moat placeholder JWT>", "ChatGPT-Account-ID": "moat-proxy-codex-account"}}
```

`auth: "chatgpt"` must never be used behind a placeholder server. Harness would read the placeholder as a real token and try to refresh it.

A project file cannot set `extra_headers` on a provider that the user config defines or that is native: `rejectProjectProviderKeys` rejects it. So a repository cannot supply or change the placeholders of such an entry, and this style adds no new risk. The placeholders come from the user config or the host. Harness never logs a header value.

## 4. Who the caller is

Harness adds nothing. The server identifies the caller as follows.

- Browser: the app server is on the page origin. `fetch` sends the cookies of the origin, and the server reads its own session. The module never sets `credentials: "omit"`. The server must also defend against cross-site requests: `SameSite` cookies, a check of `Sec-Fetch-Site`, or a custom header that the page sets with `proxy.headers`.
- Box: Harness honors `HTTPS_PROXY`. The userinfo of the proxy URL is the proxy-auth username and password, which gatekeeper reads as the subject and the actor token (`subject_from: proxy-auth`, `actor_token_from: proxy-auth-password`). Boxes already puts the user and the box id there.
- Anything else: the app sets `extra_headers`.

A header from Harness would be self-asserted. The server would have to ignore it for authorization. So Harness does not send one.

## 5. Login in ACP

Checked against `e2e/testdata/acp/schema/schema.json`. ACP has `authenticate`, `elicitation/create` with `mode: "url"` (`url` is `format: uri`, scope `requestId`), `elicitation/complete`, and the client capability `elicitation.url`. It has no method for a host to report the end of a login. The flow uses only these messages.

1. `initialize`. For each provider entry of the user config with `auth: "injected"` and a `login_url`, and only for a client that advertises `elicitation.url`, the agent offers a method of type `agent` with id `injected-<provider>` and name `Log in to <provider>`. The method `chatgpt` is offered only where the file login exists (native). `claude-code` is unchanged. `agentCapabilities.auth.logout` is present only when `chatgpt` is offered: the app page owns the logout of a injected login.
2. `session/new`, `session/load`, and `session/resume` call `Check` for the model. For an `injected` entry, `Check` sends the status probe (section 6) and fails with `-32000`, `data.code` `login_required`, when the probe answers 401. `session/prompt` sends no probe: the 401 of the turn itself is the signal (section 3). The message holds the sign-in URL, so a client without `elicitation.url` can still show it. `Check` caches the probe for 30 s, and reads it again after `authenticate` and after a failed turn. A probe that fails for another reason, or that takes more than 3 s, is unknown and raises no error.
3. `authenticate` with `methodId: "injected-<provider>"`:
   1. The agent sends the probe. A 200 answers `{}` at once.
   2. The agent sends `elicitation/create`: `mode` `url`, no `sessionId`, `requestId` the id of the `authenticate` request, a fresh `elicitationId`, `url` the `login_url`, and `message` "Sign in on the page that opens, then return here."
   3. `accept` means that the user opens the URL. `decline` ends with `-32603`, "The sign-in was declined". `cancel` ends with `-32800`.
   4. After `accept`, the agent sends the probe at once, then every 2 s (4 s after 30 s, 8 s after 2 minutes) for at most 10 minutes. The 10 minutes start at the accept. A 200 ends the loop. The agent sends `elicitation/complete` with the `elicitationId` and answers `{}`. A probe that answers 404 or 405 means that the server has no status probe: the agent then answers `{}` (after `elicitation/complete`) as soon as the user accepts, because the accept is the only signal of the user.
   5. A timeout is `-32603`. `$/cancel_request` and a new `authenticate` end the flow as for the other methods, and the agent sends `$/cancel_request` for an elicitation that has no answer.
4. The page behind `login_url` runs the device or OAuth flow on the server. It stores the token on the server. It never returns a token to Harness. After it finishes, the server stops answering 401 for this caller.

The login page must bind the flow to the session of the user and must defend against cross-site requests (section 13). The agent reads no token, stores no file, and sends no frame that holds a secret.

## 6. Status probe and model list

A probe is the model list request of the entry, sent with no credential:

- `openai`: `GET <base_url>/models`, where the path is `responses_path` with its last segment replaced by `models`. With `models_from: "chatgpt"` it is the catalog request, so one request serves both purposes.
- `anthropic`: `GET <base_url>/v1/models`.

A 200 is signed in. A 401 is signed out. A 404 or 405 means that the server has no status probe: `login_required` then comes only from the 401 of a turn, never from the probe. Anything else is unknown. The probe makes no model call and spends no quota. Section 3 sets how the server authorizes it. The probe uses the transport of the model calls of the connection (a `proxy.headers` option in wasm and `HTTPS_PROXY` natively reach it too).

The composition root of a connection (`cmd/harness acp`, `cmd/harness-wasm`) makes one `injectedauth.Logins` owner. It holds the model transport, the clock, and the function that returns the probe of an entry. The backend and the ACP logins of the connection share it, so the probe cache and the 2-minute rule have one owner and no package-level state.

`Runtime.Models` fetches the live list of an entry with `models_from: "chatgpt"` and no `models`: first call, 3 s wait, cache for the life of the process, retry after 5 minutes, and the static `codex` list on failure. A missing file login, and a 401 or 403 answer, give the static list and no log line. The next call tries again after the next successful `authenticate` or after 5 minutes. An `injected` entry is listed in wasm, because it needs no file.

Startup rules hold. Harness makes no network call before a command needs it, and `initialize` never probes.

## 7. WebAssembly and `sdk/embed`

The wasm build holds no login: the browser and Node both use `auth: "injected"`. Native keeps the file login.

Changes of `harnessConnect` and `createHarness`:

- Remove `pageOrigin`, `FetchRT.OnlyOrigin`, and `ForeignOriginError`. They exist only to keep a token on the page origin, and there is no token in a page.
- Remove the wasm login: `acpauth.Options.DeviceCode` and `LoginStore`, `acpauth/device.go`, `Options.IssuerTransport`, the `login()` of `cmd/harness-wasm`, and the Node login e2e tests (`e2e/acp_node_login_test.go`). `internal/codexauth` keeps its device flow, which `harness login codex --device` uses. A request on an `auth: "chatgpt"` entry under js fails before it is sent with a permanent `login_required` error, and reads no file. The module offers no `chatgpt` method.
- Keep `redirect: "manual"` for every model request. A redirect that the host hides is an error. A server must answer 401, not 302.
- Replace `proxy.providerBaseURLs` with `proxy.providers`: a map from a provider name to `{baseURL, loginURL}`. For a native provider such as `codex`, the module copies the native entry and sets `auth: "injected"`, `base_url`, and `login_url`; `models_from` comes from the native entry. `sdk/embed` resolves a relative `baseURL` and `loginURL` against `location.href` before it passes them, because the elicitation `url` must be absolute. For a provider in `providers` of the config, the user sets `auth` and `login_url` in the config.
- Keep `proxy.baseURL` and `proxy.headers`.
- `sdk/embed/README.md`: the section "Log in with ChatGPT" becomes "Log in through your server". `index.d.ts` changes with the options above.

This repository ships no production server. It ships a reference server (section 8) and the README gives the contract of sections 3 and 4.

## 8. Native and boxes

A native process needs only the config of section 2. `harness login codex` on a machine whose effective `codex` entry has `auth: "injected"` prints `codex uses a injected login; sign in at <login_url>` and exits 1.

Boxes needs no change, and its behavior does not change. Its `codex` entry with a dummy `api_key_env` stays a normal entry, and the gatekeeper in front of it works as before. Boxes may adopt `auth: "injected"` and `models_from` in its entry when it wants the live model list and the `login_required` error.

### Reference server

`examples/embed-server/` is its own Go module (its own `go.mod`). It imports `github.com/majorcontext/gatekeeper/proxy`. The Harness module, its `go build ./...`, and the wasm module take no gatekeeper dependency. The example serves the wasm module, `wasm_exec.js`, and `/model-proxy/`. Gatekeeper and boxes need no change.

The page config: `providers.codex = {type: "openai", auth: "injected", base_url: "/model-proxy/relay/codex", responses_path: "/responses", models_from: "chatgpt", login_url: "/settings/codex", omit_response_params: [...], sanitize_tool_schemas: true}` (the other keys of section 2).

Choice: a gatekeeper credential resolver, not `SetContextResolver`, and not `TokenExchangeResolver`.

- `SetContextResolver` cannot serve a relay. `ServeHTTP` sends `/relay/` paths to `handleRelay` before it calls the context resolver, and `runContextKey` is unexported, so a middleware cannot attach a `RunContextData` to a relay request.
- `TokenExchangeResolver` returns one header, so it cannot add `ChatGPT-Account-ID`, and it needs an STS endpoint in the example. A `proxy.CredentialResolver` returns a list of headers and reads the store directly.

The middleware authenticates the app session and strips the page headers. It sets one header, `X-App-User`, from the session. The resolver for `chatgpt.com` reads that header (registered as a strip header, so it never reaches the upstream), loads the token of the user from the store, refreshes it near expiry, and returns `Authorization` and `ChatGPT-Account-ID`. The store is filled by the `/settings/codex` page, which runs the device code flow on the server against `auth.openai.com`. Harness `internal/codexauth` is internal, so the example cannot import it. Harness exports no login package. The example holds about 80 lines of its own for the device flow, as boxes does.

```go
p := proxy.NewProxy()
_ = p.AddRelay("codex", "https://chatgpt.com/backend-api/codex")
p.SetCredentialResolverWithStripHeaders("chatgpt.com",
	func(ctx context.Context, _, r *http.Request, _ string) ([]proxy.CredentialHeader, error) {
		tok, ok := store.Fresh(ctx, r.Header.Get("X-App-User"))
		if !ok {
			return nil, errNoToken
		}
		return []proxy.CredentialHeader{
			{Name: "Authorization", Value: "Bearer " + tok.Access, Grant: "codex"},
			{Name: "ChatGPT-Account-ID", Value: tok.Account, Grant: "codex"},
		}, nil
	}, "X-App-User")

mux.Handle("/model-proxy/", http.StripPrefix("/model-proxy", appAuth(func(w http.ResponseWriter, r *http.Request) {
	user := sessionUser(r)
	if user == "" || !store.Has(user) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="app"`)
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	for _, h := range []string{"Cookie", "Origin", "Referer", "Authorization", "X-Api-Key", "ChatGPT-Account-ID", "X-App-User"} {
		r.Header.Del(h)
	}
	for k := range r.Header {
		if strings.HasPrefix(strings.ToLower(k), "sec-") {
			r.Header.Del(k)
		}
	}
	r.Header.Set("X-App-User", user)
	p.ServeHTTP(w, r)
})))
```

Checks against gatekeeper `origin/main`:

| Question | Answer |
| --- | --- |
| A relay with no proxy-auth when `SetDelegateAuth` or `SetAuthToken` is set | `ServeHTTP` sends `/relay/` paths to `handleRelay` before the auth chain and the context resolver (`proxy/serve.go:33-36`, chain at `:60-90`). `handleRelay` answers 407 only for delegate auth with no token (`relay.go:61-64`). It rejects a presented password that mismatches `authToken`, and never rejects an absent one (`relay.go:69`, `auth.go:69-77`). So the relay accepts a request with no proxy-auth, and the middleware cannot give it a per-user context: `runContextKey` is unexported and set only in `serve.go:71`, `intercept.go:240`, and `gateway.go:213,241`. The example sets neither option; the proxy is a handler behind the middleware and has no listener of its own. |
| Browser headers | The relay copies every request header except `Proxy-Authorization` and `Proxy-Connection` (`relay.go:160-167`). `prepareOutbound` removes only declared and captured headers (`inject.go:112-129`). The middleware must strip `Cookie`, `Origin`, `Referer`, `Sec-*`, and credential headers. |
| Injection on relay targets | `handleRelay` calls `getCredentialsForRequest` (`relay.go:142`). With a `RunContextData` it uses only the context credentials (`inject.go:398-404`), which a relay request never has. Otherwise the host resolver runs (`inject.go:405-456`) and falls back to static credentials. The result is set with `Header.Set` (`inject.go:305`), and one host can have several headers. So `Authorization` and `ChatGPT-Account-ID` both come from the resolver. |
| SSE and `/models?client_version=` | `forwardStreaming` flushes the headers and every chunk (`stream.go:24-35`, `61-70`). The query is kept (`relay.go:119-120`). The relay follows no redirect (`relay.go:16-17`) and waits 5 minutes for response headers (`relay.go:26`). |
| No credential for the user | A resolver that returns no header lets the request go upstream with no credential (`inject.go:455`), and the answer is whatever the upstream sends (401, or a Cloudflare 403). A resolver error is `502 credential resolution failed` (`relay.go:142-149`). Neither is a reliable 401. So the middleware answers 401 itself when the store has no token for the user, before the request reaches the proxy. A token that the backend rejects later gives the backend 401, which passes through. No gatekeeper change. |

## 9. Later, optional

Gatekeeper needs no change for this design. A server that sends 401 gets the `login_required` handling. Gatekeeper today answers `502 credential resolution failed` for a missing credential, and Harness treats that as a plain retryable error.

Two gatekeeper changes would improve a deployment that wants them. Neither is part of this work.

- A typed "no credential" answer: a typed error that carries the STS status and the OAuth code to the three 502 sites (`proxy/proxy.go`, HTTP and CONNECT, and `proxy/relay.go`), and an opt-in source option that turns a 404 `invalid_grant` into a 401. The STS contract would then say that the STS verifies the actor token before it looks up the subject, because the 401 tells a caller that a subject has no connection.
- A per-user `ChatGPT-Account-ID`, for a workspace plan.
- A moat `harness` agent provider, like `internal/providers/pi`, that reuses the claude, anthropic, and codex grants and writes a Harness config with `auth: "injected"` plus the moat placeholders.

## 10. What is deleted

- `pageOrigin`, `FetchRT.OnlyOrigin`, `ForeignOriginError`, and the browser branch of `ModelTransport` in `cmd/harness-wasm/connect.go`.
- The wasm login: `acpauth.DeviceCode`, `acpauth.LoginStore`, `acpauth/device.go`, `Options.IssuerTransport`, the Node login e2e tests, and the browser tests of the device login.
- `proxy.providerBaseURLs` and `location.origin` in `sdk/embed`.
- The codex special case in `cmd/harness-wasm` (`UsesChatGPTLogin` in the transport choice).
- The spec text that says the token is in the page, the page-origin rule, the CORS paragraph for `auth.openai.com`, the "device login is the exception" sentence, and "keyed on `auth`" for the model list fetch.

## 11. Spec sections to change

- `Authentication` (ACP): the method list, the WebAssembly login paragraph (replaced by the `injected` method and flow), `Check` for an `injected` entry, `logout`.
- `ChatGPT subscription login` and the provider entry text: add `auth: "injected"`, `login_url`, `models_from`, and the rule for a project file. `docs/architecture.md` line 1140, "The fetch is keyed on `auth`, not on the provider name", becomes "The fetch is keyed on `models_from`". The native `codex` entry gains `models_from`.
- The `Runtime.Models` bullet and the `modelmeta` row: the fetch keys on `models_from`.
- `WebAssembly`: the seams table (Issuer transport row removed, Host limits), the `harnessConnect` options (`proxy`, `pageOrigin`, `modelFetch`), and the sections `The ChatGPT login in a page` and `Model access from a page`. The latter becomes the contract of sections 3 and 4.
- `Errors` and the `login_required` bullets: a 401 of an `injected` entry, and the post-probe rule.
- `CHANGELOG.md`: one entry under the unreleased heading.

## 12. Tests

Rows in `e2e/`, on both drivers. The fake server is a `harnesstest` endpoint that answers 401 until a flag flips.

- An `injected` entry sends none of `Authorization`, `x-api-key`, `ChatGPT-Account-ID`, and `originator`.
- A 401 fails the turn with class `login_required`, and, after the failed turn, the next `session/prompt` answers `-32000` with the sign-in URL from the cached `Check`.
- A 401 on a model call within 2 minutes after a 200 probe ends the turn `permanent`, not `login_required`.
- `authenticate` sends the URL elicitation, polls, sends `elicitation/complete`, and answers `{}` after the flag flips. `decline`, `cancel`, and timeout end it as section 5 says.
- The live model list comes through the server, and a 401 gives the static list.
- A project file that sets `login_url` fails the load.
- Under wasm, the `chatgpt` method is not offered, and an `auth: "chatgpt"` request fails before it is sent.

## 13. Security

- No token in the page, the module, or the logs. The page and the module never receive one: the login runs on the server, and the module only sees 200 and 401. Harness sends no credential header, so none can reach a log or an error text. `FetchError` holds no header, query, or body, and the new messages hold only the provider name and the `login_url`, which holds no secret.
- An XSS on the page can still: call the server as the user and spend quota, within the limits of the server; read the model answers and the session store; and open the `login_url`.
- An XSS can also substitute the account. Page script can start the server-side login and finish it with the account of the attacker, so the user then spends the plan of the attacker, or the attacker reads the conversation through that account. The login page must bind the flow to the session of the user (a state value that only that session holds), and must defend against cross-site requests on its start and finish routes. Gatekeeper cannot do this: it is the job of the app that owns the login page.
- An XSS cannot read the access token or the refresh token, and cannot use them after the session cookie ends or the user disconnects on the server. The server limits the damage with a path allowlist, rate limits, short sessions, and a check against cross-site requests. A strict CSP lowers the odds.
- `login_url` and `auth` come from the user config or the host, never from a project file, so a repository cannot send a user to its own sign-in page.
- A server that cannot authorize a request answers 401. A misconfigured server that forwards an unauthenticated request makes the backend answer 401, which has the same meaning.

## 14. Non-goals

- A credential store, an OAuth client, or a login page in Harness for `injected` mode.
- A reverse-proxy mode in gatekeeper, and a production server from Harness. The reference server is an example.
- A logout through ACP for a injected login.
- A subscription login for the native `anthropic` provider: Anthropic subscription tokens work only with the Claude Code client, which keeps `claude-code-cli`. `auth: "injected"` on `anthropic` is for an API key that a server holds.
- `injected` mode for `openai-compat` and `claude-code-cli`.
- Per-session attribution headers, refresh of a token by Harness in `injected` mode, and a host callback for login completion.

## 15. Build order

Three pull requests, in this order. Each one is a Harness-only change.

1. Core `auth: "injected"`: the config (`auth`, `login_url`, `models_from`, the rules for a project file), the adapters that send no credential, 401 to `login_required`, `models_from: "chatgpt"` on the native `codex` entry, the ACP method `injected-<provider>` with the login and the probe, the permanent-error rule of section 3, and the spec sections they change.
2. wasm and `sdk/embed`: the browser uses `injected` only. Delete the in-browser token storage, the page-origin check, the Node device flow, `IssuerTransport`, `DeviceCode`, `LoginStore`, and `fileLogin`. Replace `proxy.providerBaseURLs` with `proxy.providers`.
3. `examples/embed-server`: the reference server of section 8, in its own Go module.

Harness ships the first two in one release. A deployment adopts `auth: "injected"` by config when it wants it.

