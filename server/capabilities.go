package server

// CapabilityDeltaRowIdentity: text.delta, reasoning.delta, and tool.start
// carry the owning message's id and created_at once those are known. A
// consumer must still tolerate an empty id, which engine.Event documents as
// "Empty until known" and the claude-code backend handles as a real case, so
// this name says the fields are populated at all -- not that every delta is
// identified.
const CapabilityDeltaRowIdentity = "delta_row_identity"

// capabilities is the sorted list GET /health advertises. A name, once
// shipped, is never removed or repurposed.
var capabilities = []string{
	CapabilityDeltaRowIdentity,
}
