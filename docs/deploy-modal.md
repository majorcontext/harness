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

## Minimal Sandbox

```python
import modal

app = modal.App.lookup("harness", create_if_missing=True)
sessions = modal.Volume.from_name("harness-sessions", create_if_missing=True)

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
(resume a specific one). The upcoming `harness serve` mode will resume the
same log-backed sessions.
Commit the Volume after a run so writes are durable:

```python
sessions.commit()
```

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

## (d) Memory snapshots

Modal memory snapshots after boot are safe for harness: it holds no open
network connections and no auth state at rest. Provider auth is validated on
first message send, not at boot, and nothing touches the network before then —
so a post-boot snapshot captures no live sockets or credentials to go stale.

```python
sb = modal.Sandbox.create(..., experimental_options={"enable_memory_snapshot": True})
```
