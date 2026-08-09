//go:build e2e

package workload

import (
	"fmt"
	"os"
	"strings"
)

// defaultImageRef is the harness image the phase-2.5 acceptance table names:
// the meetneptune/web .devcontainer build, published to Google Artifact
// Registry. It carries no digest by default because, as of this suite's
// authoring, GAR project neptune-agent-sbx has no `workspace-images`
// repository at all (only an empty `boxes` repository exists) — see
// docs/design/workload-validation.md's row 1 finding. Override with
// HARNESS_IMAGE_REF once a real digest-pinned image is published.
const defaultImageRef = "us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web:latest"

// boxesEnv is the resolved BOXES_URL/BOXES_TOKEN pair a test needs to reach
// the deployed fleet control plane.
type boxesEnv struct {
	URL   string
	Token string
}

// loadBoxesEnv reads BOXES_URL and BOXES_TOKEN. It never errors — an unset
// var simply means the caller's precondition is not met, and the caller is
// responsible for skipping with a clear reason (see requireBoxesEnv).
func loadBoxesEnv() boxesEnv {
	return boxesEnv{
		URL:   strings.TrimRight(os.Getenv("BOXES_URL"), "/"),
		Token: os.Getenv("BOXES_TOKEN"),
	}
}

// ready reports whether both BOXES_URL and BOXES_TOKEN are set. It does not
// probe the network — callers combine it with a live reachability check
// (see (*BoxesClient).Ping) before treating the service as usable.
func (e boxesEnv) ready() bool {
	return e.URL != "" && e.Token != ""
}

// missing describes which of BOXES_URL/BOXES_TOKEN is unset, for a clear
// skip message.
func (e boxesEnv) missing() string {
	var want []string
	if e.URL == "" {
		want = append(want, "BOXES_URL")
	}
	if e.Token == "" {
		want = append(want, "BOXES_TOKEN")
	}
	return strings.Join(want, ", ")
}

// imageRef resolves the harness image reference under test: HARNESS_IMAGE_REF
// if set, else defaultImageRef.
func imageRef() string {
	if v := os.Getenv("HARNESS_IMAGE_REF"); v != "" {
		return v
	}
	return defaultImageRef
}

// skipReason formats a uniform, greppable skip message: every row names its
// own precondition and what is missing, never a bare "skip".
func skipReason(precondition string, detail string) string {
	if detail == "" {
		return fmt.Sprintf("precondition not met: %s", precondition)
	}
	return fmt.Sprintf("precondition not met: %s (%s)", precondition, detail)
}
