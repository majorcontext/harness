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

# Final stage: static binary on scratch. ca-certificates is required for the
# outbound TLS calls harness makes to model APIs (and to any HTTPS proxy).
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /harness /harness
ENTRYPOINT ["/harness"]
