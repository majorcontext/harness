package server

// CapabilityDeltaRowIdentity: text.delta, reasoning.delta, and tool.start
// carry the owning message's id and created_at once those are known. A
// consumer must still tolerate an empty id, which engine.Event documents as
// "Empty until known" and the claude-code backend handles as a real case, so
// this name says the fields are populated at all -- not that every delta is
// identified.
const CapabilityDeltaRowIdentity = "delta_row_identity"

// CapabilitySubscriptionUsageRefresh: POST
// /session/{id}/subscription-usage/refresh exists and answers its own
// documented "unsupported" outcome (see subscriptionUsageRefreshResponseJSON)
// rather than a bare 404 route-not-found. A caller uses this to skip a
// doomed request on an older harness build without waiting on a live probe
// — the fleet runs a spread of commits at once, so "the code merged" does
// not mean every running box has it yet.
const CapabilitySubscriptionUsageRefresh = "subscription_usage_refresh"

// capabilities is the sorted list GET /health advertises. A name, once
// shipped, is never removed or repurposed.
var capabilities = []string{
	CapabilityDeltaRowIdentity,
	CapabilitySubscriptionUsageRefresh,
}
