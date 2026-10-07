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
and can silently lose the tail of the session log when a
sandbox is terminated abruptly — verified empirically: an abrupt kill on a
classic Volume preserved 1 of 7 messages; the same test on a v2 Volume
preserved all of them. v2 syncs continuously and needs no explicit
`commit()` calls.

### Durability of the session log

`DiskStore` appends each record to the session log, `log.jsonl`, and calls
`fsync` on the file before it acknowledges the append. When it creates a
session, it also calls `fsync` on the directory of the session. This is the
right behavior on a local POSIX filesystem. On a Volume v2 mount the file
`fsync` adds nothing to the continuous sync of the volume, which is the
durability boundary (see "Use Volumes v2" above).

The config key `session_sync` accepts `"fsync"` (the default) and `"volume"`.
The runtime does not change how `DiskStore` writes for either value. It
reports the value in `GET /health` as `session_sync`, in the engine banner, and
in the config summary that `harness serve` logs at start, so a reader can see
which mode a given box is configured for.

A torn tail of the log, left by an abrupt kill, is repaired when the session
opens. The `store phase in flight` warning (see
[fleet-and-serve.md](fleet-and-serve.md)) names a store operation that hangs
on a mount.

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

### Project instructions in the box

`harness serve` (and `harness run`) sets each session's `WorkDir` to the
process's current directory, and the runtime injects the nearest `AGENTS.md`
found by walking up from `WorkDir`. So box sessions automatically pick up the
cloned repo's `AGENTS.md` — as long as `harness serve` is launched from inside
the clone (set the sandbox working directory to the repo root, or `cd` into it
before `serve`). Disable per-run with `-no-instructions`, or globally with
`instructions: false` (or an `instructions_path` override) in config.

Agent Skills discovered under `<WorkDir>/.agents/skills` (or config
`skills_dirs` / the repeatable `-skills-dir` flag) are advertised the same way,
so a cloned repo's skills are offered to box sessions automatically.

### Telling box sessions about the environment

Config `append_system_prompt` is an array of platform-owned facts for every
session created by `serve` or `run`. Use it for a fact an agent cannot
discover, such as a gateway URL template or a required `0.0.0.0` bind address,
and for the platform policy that depends on that fact. Do not use it for
project instructions or for tool shape. An MCP server states its own usage
through initialize instructions, which reach the model as their own segment.

Keep every segment byte-stable for the life of a session. A segment that
carries a timestamp, a pod name, or any live status re-processes the whole
conversation uncached on every request. Nothing reports that; the only signal
is the bill.

The key merges additively. Platform entries come first, then entries from the
cloned repository's `.harness.json`. A repository can add facts but cannot
remove platform entries through this key. A `claude-code/*` session sends the
entries as one `--append-system-prompt` value. Do not also put either Claude
Code append-prompt option in provider `extra_args`; Harness rejects that
conflict. See "Prompt" in [architecture.md](architecture.md).

### Verifying what reaches the model

For a session on a model API backend, the built-in `session_info` tool
returns the exact system segments that the model received in the turn, the
active tool names, the source of the project instructions, the discovered
skills, and the configured plugins.

Delegated Claude Code turns do not use Harness request assembly, so
`session_info` does not describe their prompt. Use child-process argv logging
or Claude Code diagnostics to verify the appended prompt that the CLI gets.

The `e2e/` suite verifies that a native request contains the exact configured
segments in their expected positions. It also verifies project instructions
and the skill catalog in their expected order.

## Browser clients and CORS

This repository ships no browser UI. A browser client of a running
`harness serve` instance — for example the `majorcontext/bailey` console —
lives outside this repo and talks to the box over the HTTP+SSE API.

Because a browser page enforces the same-origin policy, `harness serve` must
opt into CORS for that client's origin:

```bash
# In the sandbox, alongside HARNESS_RUN_TOKEN:
harness serve -cors-origin '*'          # dev: allow any origin
# or, tighter, the exact origin the client is served from:
harness serve -cors-origin 'https://your-console-host.example'
```

`-cors-origin` echoes its literal value in `Access-Control-Allow-Origin` on
every response (including the SSE stream and 401s, so the browser can read
errors) and answers unauthenticated `OPTIONS` preflights with 204. Leaving it
unset keeps the current behavior — no CORS headers at all. Prefer the client
host's exact origin over `*`.

The client needs the tunnel base URL (e.g. the Modal `encrypted_ports` URL, or
`http://localhost:4096` locally) and the run token. A browser client that
stores either one keeps it in the browser, so do not host such a client on a
shared origin with a long-lived token. Run tokens are workspace-scoped and
rotate with the workspace, so the blast radius of a leaked token is one
workspace until its next rotation.

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
   the `head_seq` of the session.
3. `sb.terminate()`s the sandbox abruptly (no graceful shutdown).
4. Relaunches a fresh sandbox on the same Volume and asserts: the session is
   listed, the message count is still `N`, the `head_seq` of the
   session continues above the prior value (the counter resumed from disk
   rather than resetting), and the session is still promptable with one more tiny prompt.

Sandboxes it creates are terminated on exit (including on exception), and it
prints a final `PASS`/`FAIL` line with the counts.

```bash
# Auth: ~/.modal.toml for Modal; ANTHROPIC_API_KEY for the live prompt.
python scripts/modal-e2e.py                    # v2 durability test (the real one)
python scripts/modal-e2e.py --classic-volume   # also run the v1 negative control
```

The optional `--classic-volume` flag repeats the flow on a `version=1` Volume as
an **informational negative control**: classic Volumes commit in the background
and can lose the tail of the session log on an abrupt kill (see
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
