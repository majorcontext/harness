package protocol

const (
	// CapabilityDeltaRowIdentity says that each item.delta frame names its item.
	CapabilityDeltaRowIdentity = "delta_row_identity"
	// CapabilityResolutionSource says that a Resolution takes source, source_id, source_label, and response.
	CapabilityResolutionSource = "resolution_source"
)

// Health is the answer of GET /health: the build, the durability mode, and the start of the process.
type Health struct {
	Status      string `json:"status"`
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
	// StartedAt is the start of the runtime, RFC 3339 in UTC.
	StartedAt    string   `json:"started_at"`
	Capabilities []string `json:"capabilities"`
}
