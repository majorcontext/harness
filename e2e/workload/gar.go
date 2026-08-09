//go:build e2e

package workload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// garImageRef is a parsed "host/project/repo.../image[:tag|@digest]"
// reference, split into the three pieces the Docker Registry HTTP API v2
// (which GAR implements) needs: a registry host, a "repository" path (GAR
// calls this the image name, everything between the host and the final
// tag/digest), and a reference (a tag or a "sha256:..." digest).
type garImageRef struct {
	Registry   string
	Repository string
	Reference  string
}

var digestRefPattern = regexp.MustCompile(`^(.+)@(sha256:[0-9a-f]{64})$`)

// parseGARImageRef parses a fully-qualified GAR image reference. It rejects
// anything without an explicit registry host (a bare "image:tag" is not
// enough information to build a v2 API URL).
func parseGARImageRef(ref string) (garImageRef, error) {
	if ref == "" {
		return garImageRef{}, fmt.Errorf("empty image reference")
	}
	rest := ref
	digest := ""
	if m := digestRefPattern.FindStringSubmatch(ref); m != nil {
		// A digest form ("host/repo@sha256:...") never carries a tag
		// alongside it, so repo needs no further tag split below.
		rest, digest = m[1], m[2]
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return garImageRef{}, fmt.Errorf("image reference %q has no registry host", ref)
	}
	registry := rest[:slash]
	repo := rest[slash+1:]

	reference := digest
	if reference == "" {
		// A tag is the text after the LAST colon. A colon can also appear
		// in a port-bearing registry host, but that was already split off
		// above, so this is unambiguous here.
		reference = "latest"
		if i := strings.LastIndex(repo, ":"); i >= 0 {
			reference = repo[i+1:]
			repo = repo[:i]
		}
	}
	if registry == "" || repo == "" {
		return garImageRef{}, fmt.Errorf("could not parse image reference %q", ref)
	}
	return garImageRef{Registry: registry, Repository: repo, Reference: reference}, nil
}

// resolveGARAccessToken returns a bearer token for the Docker Registry HTTP
// API v2 GAR exposes. GAR_ACCESS_TOKEN (set by CI, e.g. via a workload-
// identity-federation step that runs before `go test`) wins when present;
// otherwise it shells out to `gcloud auth print-access-token`, the same
// credential a developer's interactive session already has.
func resolveGARAccessToken(ctx context.Context, tokenOverride string) (string, error) {
	if tokenOverride != "" {
		return tokenOverride, nil
	}
	cmd := exec.CommandContext(ctx, "gcloud", "auth", "print-access-token")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gcloud auth print-access-token: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("gcloud auth print-access-token: %w (is gcloud installed and authenticated?)", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("gcloud auth print-access-token returned an empty token")
	}
	return token, nil
}

// manifestAcceptHeader lists every manifest media type GAR may return for a
// modern multi-arch (or single-arch) image, per the OCI distribution spec.
const manifestAcceptHeader = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

type registryError struct {
	StatusCode int
	Body       string
}

func (e *registryError) Error() string {
	return fmt.Sprintf("registry API status %d: %s", e.StatusCode, e.Body)
}

// isManifestUnknown reports whether err is the registry's "this repository
// or tag/digest does not exist" response — the precondition-not-met case a
// test should skip on, distinct from every other failure mode (auth,
// malformed ref, GAR outage), which should fail loudly.
func isManifestUnknown(err error) bool {
	var rerr *registryError
	if !asRegistryError(err, &rerr) {
		return false
	}
	if rerr.StatusCode == http.StatusNotFound {
		return true
	}
	// GAR reports an absent repository as 404 with a distribution-spec
	// error envelope naming NAME_UNKNOWN in the body even when the HTTP
	// status itself is already 404, and (observed against a real, empty
	// GAR repository while authoring this suite) sometimes reports a
	// missing IMAGE within an existing repository as 400 NAME_UNKNOWN
	// instead. Match the error code, not only the status.
	return strings.Contains(rerr.Body, "NAME_UNKNOWN") || strings.Contains(rerr.Body, "MANIFEST_UNKNOWN")
}

func asRegistryError(err error, target **registryError) bool {
	rerr, ok := err.(*registryError)
	if ok {
		*target = rerr
	}
	return ok
}

// ociManifest covers both a single-image manifest (config+layers) and an
// index/manifest-list (manifests[]) — only the fields this suite reads.
type ociManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

// ociImageConfig is the blob a manifest's config.digest points at — the OCI
// image config spec. Only .config.User (this suite's whole reason for
// existing) and .architecture/.os (for logging) are modeled.
type ociImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		User string `json:"User"`
	} `json:"config"`
}

// garClient fetches OCI manifests and image config blobs from GAR's Docker
// Registry HTTP API v2 endpoint via plain net/http — no crane/docker
// dependency, so this runs anywhere `go test -tags=e2e` runs as long as a
// GAR access token is available (see resolveGARAccessToken).
type garClient struct {
	token string
	hc    *http.Client
}

func newGARClient(ctx context.Context, tokenOverride string) (*garClient, error) {
	token, err := resolveGARAccessToken(ctx, tokenOverride)
	if err != nil {
		return nil, err
	}
	return &garClient{token: token, hc: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *garClient) getManifest(ctx context.Context, ref garImageRef, reference string) (*ociManifest, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, reference)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", manifestAcceptHeader)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &registryError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest from %s: %w", url, err)
	}
	return &m, nil
}

func (c *garClient) getConfigBlob(ctx context.Context, ref garImageRef, digest string) (*ociImageConfig, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &registryError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var cfg ociImageConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse image config from %s: %w", url, err)
	}
	return &cfg, nil
}

// platformDigest picks the linux/amd64 entry from a manifest index/list,
// falling back to linux/arm64, then the first entry present — GAR images in
// this fleet are built for gVisor sandboxes, which run linux/amd64 today,
// but this stays useful if that ever changes.
func platformDigest(m *ociManifest) (string, error) {
	if len(m.Manifests) == 0 {
		return "", fmt.Errorf("manifest index has no entries")
	}
	pick := func(arch string) string {
		for _, e := range m.Manifests {
			if e.Platform.OS == "linux" && e.Platform.Architecture == arch {
				return e.Digest
			}
		}
		return ""
	}
	if d := pick("amd64"); d != "" {
		return d, nil
	}
	if d := pick("arm64"); d != "" {
		return d, nil
	}
	return m.Manifests[0].Digest, nil
}

// fetchImageConfig resolves ref end to end: manifest (following an
// index/manifest-list down to one platform's manifest when present), then
// that manifest's config blob.
func fetchImageConfig(ctx context.Context, client *garClient, ref garImageRef) (*ociImageConfig, error) {
	m, err := client.getManifest(ctx, ref, ref.Reference)
	if err != nil {
		return nil, err
	}
	if len(m.Manifests) > 0 {
		digest, err := platformDigest(m)
		if err != nil {
			return nil, err
		}
		m, err = client.getManifest(ctx, ref, digest)
		if err != nil {
			return nil, err
		}
	}
	if m.Config.Digest == "" {
		return nil, fmt.Errorf("manifest for %s/%s has no config digest", ref.Registry, ref.Repository)
	}
	return client.getConfigBlob(ctx, ref, m.Config.Digest)
}
