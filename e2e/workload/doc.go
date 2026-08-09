//go:build e2e

// Package workload holds the phase-2.5 workload-validation suite: black-box
// tests that prove real fleet workloads run on the boxes stack (harness
// image + a fleet control plane, deployed on GKE/gVisor as the Modal
// replacement) once that stack is wired and deployed.
//
// The suite is gated behind the `e2e` build tag, so `go test ./...` (no
// tag) never builds it and CI stays fast and offline by default. Run it
// explicitly:
//
//	go test -tags=e2e ./e2e/workload/...            # everything reachable now
//	go test -tags=e2e ./e2e/workload/... -run TestPrecondition_HarnessImageNonRoot -v
//
// Every test but the GAR precondition check talks to a deployed fleet
// control plane ("the boxes service") over HTTP, named by two env vars:
//
//	BOXES_URL   base URL, e.g. https://boxes.neptune-agent-sbx.example
//	BOXES_TOKEN bearer token for that service
//
// Neither var is required to run the suite — a test whose precondition is
// not met (the service unreachable, the image not yet published, a
// deployment gap the acceptance table calls out) calls t.Skip with a clear
// reason instead of failing. See docs/design/workload-validation.md for the
// full acceptance table, the pass bar per row, and which rows are
// runnable today versus blocked on other in-flight work.
//
// The boxes-service wire shapes in client_boxes.go are PROVISIONAL: no
// OpenAPI spec for that service exists in this repo yet (it is owned by
// the infra/deploy work landing in parallel). They encode the shapes named
// in the phase-2.5 acceptance table (POST /v1/boxes to spawn, GET to poll,
// an SSE event stream, hibernate) and are expected to need adjustment once
// the real spec lands — isolated to that one file for exactly that reason.
// client_harness.go, by contrast, mirrors the ALREADY-SPECIFIED
// server/openapi.yaml surface (the harness `serve` API a box exposes on
// port 4096) and should not need to change independent of that spec.
package workload
