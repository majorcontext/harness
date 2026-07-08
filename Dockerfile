# syntax=docker/dockerfile:1

# Build stage: matches the go directive in go.mod (go 1.25.x).
FROM golang:1.25 AS build
WORKDIR /src

# Cache module downloads separately from source.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# VERSION is stamped into main.version; defaults to the source-tree value.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /harness ./cmd/harness

# sandbox target: the binary plus a curated toolbelt for agent work — shell,
# git, curl, process tools, search. This is the image to run agents in
# (`docker build --target sandbox`); layer language toolchains on top of it.
# In-sandbox tools do not weaken the security model when egress is
# default-deny through a credential-injecting proxy (see docs/deploy-modal.md).
FROM debian:stable-slim AS sandbox
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        jq \
        less \
        patch \
        procps \
        ripgrep \
        unzip \
        zstd \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /harness /usr/local/bin/harness
ENTRYPOINT ["/usr/local/bin/harness"]

# Default (dist) target: static binary on scratch, ~3 MB. A distribution
# artifact for COPY --from layering, not an agent runtime — it has no shell,
# so the bash tool cannot work here. ca-certificates is required for the
# outbound TLS calls harness makes to model APIs (and to any HTTPS proxy).
FROM scratch AS dist
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /harness /harness
ENTRYPOINT ["/harness"]
