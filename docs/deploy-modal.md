# Running harness in a Modal sandbox

This guide runs the harness container image inside a [Modal](https://modal.com)
Sandbox (gVisor-isolated). It assumes the repo-root `Dockerfile`.

## Build and push the image

Modal builds from a registry image or a local Dockerfile. Simplest path is to
build with Modal directly from the Dockerfile:

```python
import modal

image = modal.Image.from_dockerfile("Dockerfile", build_args={"VERSION": "0.1.0"})
```

Or build/push yourself and reference the registry tag:

```bash
docker build --build-arg VERSION="$(git describe --tags --always)" -t <registry>/harness:latest .
docker push <registry>/harness:latest
```

```python
image = modal.Image.from_registry("<registry>/harness:latest")
```

## Two image targets: dist and sandbox

The Dockerfile has two named targets:

- **`dist`** (the default): harness binary + CA certificates on `scratch`,
  ~3 MB. A distribution artifact — in-process tools and model API calls
  work, but there is no `/bin/sh`, so the `bash` tool cannot. Use it as a
  versioned layer to copy the binary from.
- **`sandbox`** (`docker build --target sandbox`): the binary on
  `debian:stable-slim` with a curated toolbelt — shell, git, curl, jq,
  ripgrep, procps (`ps`/`pkill`), patch, file, unzip, zstd. ~75 MB. This is
  the image to run agents in; both the bash tool and the full agent loop
  are verified working inside it.

In-sandbox tools do not weaken the security model: with egress default-deny
through a credential-injecting proxy, a `curl` inside the sandbox has no
secrets to read and nowhere to send them.

Project-specific toolchains layer on top of `sandbox` (or copy the binary
from `dist` into an existing toolchain image):

```dockerfile
FROM ghcr.io/majorcontext/harness:latest AS harness   # dist target
FROM your-project-toolchain:latest
COPY --from=harness /harness /usr/local/bin/harness
```

### Repos with a devcontainer

When the target repo declares its environment via `.devcontainer/`
(containers.dev), prefer that over the generic `sandbox` toolbelt: build the
repo's image with `devcontainer build` in trusted CI (features and lifecycle
hooks run arbitrary scripts — always at bake time, never per-workspace), then
copy the harness binary in from `dist` as above. The agent then works in the
same environment a human contributor would. `sandbox` remains the fallback
for repos without one. Never rebuild an image from a `devcontainer.json` an
agent has modified in-workspace; image changes go through PR review like
code.

## Minimal Sandbox

```python
import modal

app = modal.App.lookup("harness", create_if_missing=True)
sessions = modal.Volume.from_name("harness-sessions", create_if_missing=True, version=2)

CPU = 2.0  # sandbox vCPU allocation

sb = modal.Sandbox.create(
    "run", "-p", "summarize the repo",
    image=image,
    app=app,
    cpu=CPU,
    # (a) GOMAXPROCS: under gVisor the Go runtime sees the host's core count,
    # not the sandbox allocation, so it spawns too many Ps and thrashes the
    # scheduler. Pin it to the CPU request.
    # (c) API keys via Modal Secret — the key lives in Modal, not the image.
    secrets=[modal.Secret.from_name("model-api-keys")],
    env={
        "GOMAXPROCS": str(int(CPU)),
        "HARNESS_SESSION_DIR": "/sessions",
    },
    # (b) Persist sessions across sandbox death.
    volumes={"/sessions": sessions},
)
print(sb.stdout.read())
sb.wait()
```

`modal.Secret.from_name("model-api-keys")` should carry `ANTHROPIC_API_KEY`
and/or `OPENAI_API_KEY`.

### (b) Session persistence and resume

`HARNESS_SESSION_DIR=/sessions` points harness at the mounted Volume, so the
append-only session log outlives the sandbox. A later sandbox on the same
Volume can `harness run -c` (continue the most recent session) or `-r <id>`
(resume a specific one). `harness serve` resumes the same log-backed
sessions.

**Use Volumes v2 (`version=2`).** Classic Volumes commit in the background
and can silently lose the tail of the session log and event journal when a
sandbox is terminated abruptly — verified empirically: an abrupt kill on a
classic Volume preserved 1 of 7 messages; the same test on a v2 Volume
preserved all of them. v2 syncs continuously and needs no explicit
`commit()` calls.

### (c) Keys via a credential-injecting proxy (alternative to Secrets)

To keep the sandbox holding **no** API keys at all, route egress through
[gatekeeper](https://github.com/majorcontext/gatekeeper), a credential-injecting
TLS-intercepting proxy. Harness makes no auth decisions itself (auth lives at
the network layer); point it at the proxy and gatekeeper injects the real
`Authorization` header per destination host:

```python
env={
    "GOMAXPROCS": str(int(CPU)),
    "HARNESS_SESSION_DIR": "/sessions",
    "HTTP_PROXY": "http://gatekeeper:8080",
    "HTTPS_PROXY": "http://gatekeeper:8080",
    # Trust gatekeeper's CA for TLS interception.
    "SSL_CERT_FILE": "/certs/gatekeeper-ca.pem",
}
```

The API keys then live only in gatekeeper's config, never in the sandbox image,
env, or Modal Secret attached to the workload.

## Inspecting sessions

`tools/inspector/index.html` is a standalone browser UI for a running
`harness serve` instance: a session list, live timelines (streaming text,
tool calls, reasoning, errors), and a prompt box. It is **not** served by
harness — the box exposes only the API — so you open the file yourself and
point it at the tunnel.

Because a browser page enforces the same-origin policy, `harness serve` must
opt into CORS for the inspector's origin:

```bash
# In the sandbox, alongside HARNESS_RUN_TOKEN:
harness serve -cors-origin '*'          # dev: allow any origin
# or, tighter, the exact origin you'll open the inspector from:
harness serve -cors-origin 'https://your-inspector-host.example'
```

`-cors-origin` echoes its literal value in `Access-Control-Allow-Origin` on
every response (including the SSE stream and 401s, so the browser can read
errors) and answers unauthenticated `OPTIONS` preflights with 204. Leaving it
unset keeps the current behavior — no CORS headers at all. When you serve the
inspector from a real static host, prefer that host's exact origin over `*`.

Then:

1. Open `tools/inspector/index.html` — straight from `file://`, or from any
   static host.
2. Paste the tunnel base URL (e.g. the Modal `encrypted_ports` URL, or
   `http://localhost:4096` locally) and the run token into the connect bar,
   then **Connect**. Both are remembered in `localStorage`.
3. Pick a session (or **+ new session**) and watch its timeline stream live.
   The inspector reconnects automatically with backoff if the sandbox restarts
   or the tunnel URL drops, replaying from the last durable event it saw.

## Ephemerality e2e

`scripts/modal-e2e.py` is an on-demand test (not run in CI) that proves session
durability survives an abrupt sandbox kill on a Modal Volume — the real-infra
counterpart to the in-repo `e2e/` package (which fakes the provider and kills a
local process). It exercises the exact deployment shape this guide documents:
a `golang:1.25` image plus a linux/amd64 `harness` binary, `harness serve`
behind an `encrypted_ports` tunnel, a generated run token, and a named Volume
mounted at `/sessions`.

What it does:

1. Builds a static linux/amd64 binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`).
2. Launches a sandbox on a **v2** Volume, creates a session, and drives one
   tiny real prompt through the tunnel (a bash `echo`) using the real
   `ANTHROPIC_API_KEY` from the environment. Records the message count `N` and
   the max event seq.
3. `sb.terminate()`s the sandbox abruptly (no graceful shutdown).
4. Relaunches a fresh sandbox on the same Volume and asserts: the session is
   listed, the message count is still `N`, the event journal's seq continues
   above the prior max (the counter resumed from disk rather than resetting),
   and the session is still promptable with one more tiny prompt.

Sandboxes it creates are terminated on exit (including on exception), and it
prints a final `PASS`/`FAIL` line with the counts.

```bash
# Auth: ~/.modal.toml for Modal; ANTHROPIC_API_KEY for the live prompt.
python scripts/modal-e2e.py                    # v2 durability test (the real one)
python scripts/modal-e2e.py --classic-volume   # also run the v1 negative control
```

The optional `--classic-volume` flag repeats the flow on a `version=1` Volume as
an **informational negative control**: classic Volumes commit in the background
and can lose the tail of the session log / event journal on an abrupt kill (see
"Use Volumes v2" above). The control only reports its delta — it never affects
the exit code either way.

## (d) Memory snapshots

Modal memory snapshots after boot are safe for harness: it holds no open
network connections and no auth state at rest. Provider auth is validated on
first message send, not at boot, and nothing touches the network before then —
so a post-boot snapshot captures no live sockets or credentials to go stale.

```python
sb = modal.Sandbox.create(..., experimental_options={"enable_memory_snapshot": True})
```
