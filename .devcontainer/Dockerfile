# Development image for harness. Consumed two ways, deliberately:
#
#   1. VS Code / Codespaces / any devcontainer runtime, via
#      .devcontainer/devcontainer.json.
#   2. Headless sandbox providers that can build a plain Dockerfile
#      (e.g. Modal's Image.from_dockerfile) to run dev boxes hosting
#      `harness serve` — see docs/design/fleet-model.md.
#
# Because of (2), keep this a PLAIN Dockerfile: no devcontainer
# "features", no lifecycle hooks baked in — anything a non-devcontainer
# runtime could not reproduce belongs in devcontainer.json instead.
# Floating patch tags (1.25-bookworm, setup_24.x) are DELIBERATE: under
# the fleet model boxes are cattle, rebuilt often — drift-on-rebuild picks
# up toolchain patches by design, and go.mod's toolchain directive governs
# the Go version actually used to build regardless of the image's patch
# level. Pin by digest here only if spawn-time determinism ever becomes a
# requirement.
FROM golang:1.25-bookworm

# Node is required for the repo's build-free web tooling tests
# (node --test tools/inspector/*_test.mjs, tools/hub/*_test.mjs) and for
# agent work on JS-adjacent repos. nodesource over apt's node: bookworm
# ships node 18, below what current web tooling wants.
RUN curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && corepack enable \
    && rm -rf /var/lib/apt/lists/*

# Everyday tools agents and humans reach for during harness development.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        jq ripgrep procps unzip zstd patch file less \
    && rm -rf /var/lib/apt/lists/*
