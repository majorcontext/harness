package server

import "testing"

// TestParseModuleVersion covers buildInfo's module-mode fallback directly:
// the pseudo-version shape `go install pkg@<sha>` produces (revision +
// UTC-RFC3339 time both extracted), a real tag (revision only, verbatim),
// "(devel)", and "" (both empty).
func TestParseModuleVersion(t *testing.T) {
	cases := []struct {
		name        string
		version     string
		wantRev     string
		wantBuildAt string
	}{
		{
			name:        "pseudo-version, no base version",
			version:     "v0.0.0-20240102150405-abcdef012345",
			wantRev:     "abcdef012345",
			wantBuildAt: "2024-01-02T15:04:05Z",
		},
		{
			name:        "pseudo-version, tagged pre-release form",
			version:     "v1.2.4-0.20240102150405-abcdef012345",
			wantRev:     "abcdef012345",
			wantBuildAt: "2024-01-02T15:04:05Z",
		},
		{
			name:        "real tag",
			version:     "v1.2.3",
			wantRev:     "v1.2.3",
			wantBuildAt: "",
		},
		{
			name:        "devel",
			version:     "(devel)",
			wantRev:     "",
			wantBuildAt: "",
		},
		{
			name:        "empty",
			version:     "",
			wantRev:     "",
			wantBuildAt: "",
		},
		{
			name:        "hex-ish but wrong lengths, not a pseudo-version",
			version:     "v1.2.3-4-abc123",
			wantRev:     "v1.2.3-4-abc123",
			wantBuildAt: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev, buildAt := parseModuleVersion(tc.version)
			if rev != tc.wantRev {
				t.Errorf("revision = %q, want %q", rev, tc.wantRev)
			}
			if buildAt != tc.wantBuildAt {
				t.Errorf("buildTime = %q, want %q", buildAt, tc.wantBuildAt)
			}
		})
	}
}
