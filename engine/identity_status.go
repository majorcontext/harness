// Ambient engine-identity status block. Structurally mirrors
// engine/process.go's processStatusSegment (see that file's doc comment):
// computed fresh every streamTurn call from Config fields set once at
// session construction, appended only to the newest user message via the
// shared withAmbientStatus, and never persisted to the session log.
//
// Unlike the process/MCP/goal-parked segments, which report NOTABLE state
// (something started, degraded, or parked) and are absent the rest of the
// time, this segment reports STANDING identity: which engine build is
// serving this session, under which durability mode, since when. It exists
// so an agent can answer "what engine am I running under" from ambient
// context alone, with zero tool calls — the same standing-answer guarantee
// managed-process status already gives for "what's running" — rather than
// relying on `harness version` in bash (reads the binary on disk, which can
// diverge from the currently-serving process after a redeploy) or grepping
// the serve log's boot line (reads what the process logged AT boot, which
// can equally diverge from its current, possibly-since-reconfigured state).
package engine

import (
	"strings"
	"time"
)

// identityStatusSegment renders the ambient engine-identity block request
// assembly appends to the newest user message (see streamTurn):
//
//	[engine: harness <version> · session_sync=<mode> · started <UTC RFC3339>]
//
// sessionSync is rendered as its EFFECTIVE mode, always — "fsync" for the
// zero value or any value other than SessionSyncVolume, exactly like
// Session.volumeSync's own default-is-fsync treatment (see store.go) —
// never omitted and never left for the reader to infer from silence, since
// self-describing config means an agent should not have to know the
// default to know what mode it's actually in.
//
// version and startedAt are each independently optional: an empty version
// (Config.EngineVersion's zero value — see its doc comment) omits just the
// "harness <version>" clause, and a zero startedAt omits just the "started
// ..." clause, so a Config built directly (bypassing cmd/harness, which
// always sets both) still gets a useful block rather than losing the whole
// thing to one missing field. The block itself is present whenever at
// least one of version/startedAt is set; when NEITHER is set — the common
// case for every existing test and embedder that predates this field, and
// for a session builder that never threads either one — this renders "",
// the same zero happy-path cost the other ambient segments already commit
// to, so no unrelated test asserting on request/message shape needs to
// change.
func identityStatusSegment(version string, startedAt time.Time, sessionSync string) string {
	if version == "" && startedAt.IsZero() {
		return ""
	}
	var clauses []string
	if version != "" {
		clauses = append(clauses, "harness "+version)
	}
	clauses = append(clauses, "session_sync="+effectiveSessionSync(sessionSync))
	if !startedAt.IsZero() {
		clauses = append(clauses, "started "+startedAt.UTC().Format(time.RFC3339))
	}
	return "[engine: " + strings.Join(clauses, " · ") + "]"
}

// effectiveSessionSync normalizes a raw Config.SessionSync value to the
// mode it actually behaves as: any value other than SessionSyncVolume
// (including the zero value, and any value config.validateSessionSync would
// have rejected before ever reaching here) is fsync mode. Mirrors
// Session.volumeSync's own comparison in store.go; kept as a separate,
// package-level helper here (rather than reused from a Session method)
// since this segment is built from raw Config field values, not a
// constructed *Session.
func effectiveSessionSync(mode string) string {
	if mode == SessionSyncVolume {
		return SessionSyncVolume
	}
	return SessionSyncFsync
}
