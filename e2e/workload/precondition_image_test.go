//go:build e2e

package workload

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPrecondition_HarnessImageNonRoot is the hard gate for acceptance row
// 1 (docs/design/workload-validation.md). It needs no deployed boxes
// service — only network access to Google Artifact Registry — so it is the
// one test in this suite runnable today with nothing else wired up.
//
// The finding it proves: a sandbox platform enforcing runAsNonRoot (gVisor
// on GKE, per the sandbox review this precondition is named for) refuses to
// start any container image whose config declares no non-root USER. That
// refusal happens at schedule time, after every other row's setup cost has
// already been paid — so this check must run FIRST and FAIL LOUDLY the
// moment the image itself is the problem, rather than surface as a
// mysterious spawn timeout three rows later.
//
// It fetches the real image manifest and config blob from GAR over the
// Docker Registry HTTP API v2 (no crane/docker binary required — see
// gar.go) and asserts .config.User is non-empty. A GAR access token comes
// from GAR_ACCESS_TOKEN if set, else `gcloud auth print-access-token`
// (see resolveGARAccessToken).
//
// If the image itself has not been published yet (the repository or the
// tag/digest is unknown to GAR), that is a DIFFERENT condition from the
// security finding this test exists to catch: it is a precondition gap,
// not a defect, so it skips rather than fails — same policy as every other
// row's SKIPPING-not-FAILING rule for an unmet precondition. Once the image
// is found, an EMPTY User is exactly the defect this test polices, and
// that failure is loud: t.Fatalf, not a skip.
func TestPrecondition_HarnessImageNonRoot(t *testing.T) {
	ref := imageRef()
	parsed, err := parseGARImageRef(ref)
	if err != nil {
		t.Fatalf("image reference %q is malformed: %v", ref, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := newGARClient(ctx, os.Getenv("GAR_ACCESS_TOKEN"))
	if err != nil {
		t.Skip(skipReason("GAR credentials", err.Error()))
	}

	cfg, err := fetchImageConfig(ctx, client, parsed)
	if err != nil {
		if isManifestUnknown(err) {
			t.Skip(skipReason(
				"harness image not yet published to GAR",
				"registry="+parsed.Registry+" repository="+parsed.Repository+" reference="+parsed.Reference+": "+err.Error(),
			))
		}
		t.Fatalf("fetching image config for %s: %v", ref, err)
	}

	t.Logf("image %s: os=%s arch=%s config.User=%q", ref, cfg.OS, cfg.Architecture, cfg.Config.User)

	if cfg.Config.User == "" {
		t.Fatalf(
			"image %s declares NO non-root USER (.config.User is empty) — "+
				"a gVisor/GKE sandbox enforcing runAsNonRoot will refuse to start "+
				"this image; every other acceptance row is blocked until the "+
				"image's Dockerfile sets a non-root USER",
			ref,
		)
	}
}
