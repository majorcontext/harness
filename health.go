package harness

import (
	"regexp"
	"runtime/debug"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// healthOf describes a runtime that started at started.
func healthOf(version string, cfg config.Config, started time.Time) protocol.Health {
	rev, at := buildInfo()
	mode := "fsync"
	if cfg.SessionSync == "volume" {
		mode = "volume"
	}
	return protocol.Health{Status: "ok", Version: version, VCSRevision: rev, VCSTime: at, SessionSync: mode,
		StartedAt: started.UTC().Format(time.RFC3339), Capabilities: []string{protocol.CapabilityDeltaRowIdentity}}
}

// pseudoVersion matches the timestamp and the commit that end a Go module
// pseudo-version, such as v0.0.0-20240102150405-abcdef012345.
var pseudoVersion = regexp.MustCompile(`(\d{14})-([0-9a-f]{12})$`)

// buildInfo returns the commit and the commit time of the binary: the VCS
// settings of the build, or else the pseudo-version of a module install, or
// else the module version as it is. A build with no information returns "".
func buildInfo() (revision, at string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			at = s.Value
		}
	}
	if revision != "" {
		return revision, at
	}
	switch v := info.Main.Version; {
	case v == "" || v == "(devel)":
		return "", ""
	default:
		if m := pseudoVersion.FindStringSubmatch(v); m != nil {
			if t, err := time.Parse("20060102150405", m[1]); err == nil {
				return m[2], t.UTC().Format(time.RFC3339)
			}
		}
		return v, ""
	}
}
