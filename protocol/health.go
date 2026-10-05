package protocol

// CapabilityDeltaRowIdentity says that each item.delta frame names its item.
const CapabilityDeltaRowIdentity = "delta_row_identity"

// Health is the answer of GET /health: the build, the durability mode, and the start of the process.
type Health struct {
	Status      string `json:"status"`
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
	SessionSync string `json:"session_sync"`
	// StartedAt is the start of the runtime, RFC 3339 in UTC.
	StartedAt    string   `json:"started_at"`
	Capabilities []string `json:"capabilities"`
}
