//go:build e2e

package workload

import "testing"

func TestParseGARImageRef(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		want    garImageRef
		wantErr bool
	}{
		{
			name: "tag",
			ref:  "us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web:latest",
			want: garImageRef{
				Registry:   "us-central1-docker.pkg.dev",
				Repository: "neptune-agent-sbx/workspace-images/web",
				Reference:  "latest",
			},
		},
		{
			name: "no tag defaults to latest",
			ref:  "us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web",
			want: garImageRef{
				Registry:   "us-central1-docker.pkg.dev",
				Repository: "neptune-agent-sbx/workspace-images/web",
				Reference:  "latest",
			},
		},
		{
			name: "digest",
			ref:  "us-central1-docker.pkg.dev/neptune-agent-sbx/workspace-images/web@sha256:" + testDigestHex,
			want: garImageRef{
				Registry:   "us-central1-docker.pkg.dev",
				Repository: "neptune-agent-sbx/workspace-images/web",
				Reference:  "sha256:" + testDigestHex,
			},
		},
		{
			name:    "no registry host",
			ref:     "web:latest",
			wantErr: true,
		},
		{
			name:    "empty",
			ref:     "",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGARImageRef(tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseGARImageRef(%q) = %+v, want error", tc.ref, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGARImageRef(%q) unexpected error: %v", tc.ref, err)
			}
			if got != tc.want {
				t.Errorf("parseGARImageRef(%q) = %+v, want %+v", tc.ref, got, tc.want)
			}
		})
	}
}

// testDigestHex is a syntactically valid (but not necessarily real) sha256
// hex digest, long enough to satisfy digestRefPattern.
const testDigestHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b852"

func TestIsManifestUnknown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "404 with NAME_UNKNOWN body (real GAR response for an absent repository)",
			err:  &registryError{StatusCode: 404, Body: `{"errors":[{"code":"NAME_UNKNOWN","message":"Repository \"workspace-images\" not found"}]}`},
			want: true,
		},
		{
			name: "404 with MANIFEST_UNKNOWN body (real GAR response for an absent tag)",
			err:  &registryError{StatusCode: 404, Body: `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"Failed to fetch \"latest\""}]}`},
			want: true,
		},
		{
			name: "plain 404 with no recognizable body",
			err:  &registryError{StatusCode: 404, Body: ""},
			want: true,
		},
		{
			name: "401 unauthorized is not a missing-image condition",
			err:  &registryError{StatusCode: 401, Body: `{"errors":[{"code":"UNAUTHORIZED"}]}`},
			want: false,
		},
		{
			name: "500 is not a missing-image condition",
			err:  &registryError{StatusCode: 500, Body: "internal error"},
			want: false,
		},
		{
			name: "non-registry error",
			err:  errPlain("boom"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isManifestUnknown(tc.err); got != tc.want {
				t.Errorf("isManifestUnknown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }
