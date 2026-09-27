package server

// CapabilityDeltaRowIdentity: text.delta, reasoning.delta, and tool.start
// carry both the owning message's id and its created_at.
const CapabilityDeltaRowIdentity = "delta_row_identity"

// Capabilities is the sorted list GET /health advertises. A name, once
// shipped, is never removed or repurposed.
var Capabilities = []string{
	CapabilityDeltaRowIdentity,
}
