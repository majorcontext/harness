package protocol

// Capability names advertised by GET /health. A name, once shipped, is never
// removed or repurposed.
const (
	// CapabilityDeltaRowIdentity: every item.delta frame names the item that it
	// extends, which is the item of the message that its item.completed record
	// holds.
	CapabilityDeltaRowIdentity = "delta_row_identity"
)

// Health is the answer of GET /health. It names the build and the start of the
// process, so a canary can check which harness runs, in which durability mode,
// and since when, with no token and no session.
type Health struct {
	// Status is "ok".
	Status  string `json:"status"`
	Version string `json:"version"`
	// VCSRevision and VCSTime come from the build of the binary, and are empty
	// for a binary with no build information.
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
	// SessionSync is the durability mode that the process runs in: fsync or volume.
	SessionSync string `json:"session_sync"`
	// StartedAt is the start of the runtime, RFC 3339 in UTC.
	StartedAt    string   `json:"started_at"`
	Capabilities []string `json:"capabilities"`
}
