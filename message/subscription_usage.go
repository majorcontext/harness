package message

// SubscriptionUsage is a provider-reported subscription limit snapshot. The engine captures it without an extra request.

type SubscriptionUsage struct {
	// Provider names which lane captured this snapshot: "claude" or
	// "codex" — see this type's own doc comment.
	Provider string `json:"provider"`
	// Plan is the subscription tier ("max"/"pro" for codex, from its own
	// x-codex-plan-type header). Empty when the capturing lane has no
	// cheap source for it — the claude lane's rate_limit_event carries no
	// plan field of its own, and this package does not shell out to
	// `claude auth status` just to learn one.
	Plan string `json:"plan"`
	// Windows is one entry per rate-limit window the provider reported on
	// this turn, in the provider's own order. Never nil when a
	// SubscriptionUsage exists, even if empty.
	Windows []SubscriptionUsageWindow `json:"windows"`
	// Overage describes a pay-as-you-go overage state riding on top of the
	// subscription (claude's rate_limit_event only — the codex lane's
	// x-codex-* headers carry no overage concept, so this is always nil
	// for provider "codex"). nil (omitted on the wire) when not
	// applicable.
	Overage *SubscriptionOverage `json:"overage,omitempty"`
	// CapturedAt is when this snapshot was captured — harness's own clock
	// (Config.Now), not a provider-reported time — Unix seconds.
	CapturedAt int64 `json:"captured_at"`
	// SessionCostUSD is this session's cumulative dollar cost across every
	// completed "claude"-lane delegated turn, summed turn over turn from
	// the `claude` CLI's own per-turn total_cost_usd accounting (see
	// engine/claude_code_backend.go's claudeCodeEnvelope.TotalCostUSD).
	// Always nil for provider "codex" (its x-codex-* headers carry no
	// cost figure).
	//
	// The `claude` CLI reports total_cost_usd on EVERY delegated turn's
	// "result" event, live-verified against a real `claude` 2.1.252
	// binary — not only during pay-as-you-go overage. A plain-
	// subscription turn still carries the dollar figure that turn would
	// have cost at metered API rates, informational even though the user
	// is not actually billed it. So this field goes non-nil the moment a
	// session completes its FIRST "claude"-lane turn, whether or not
	// Overage is ever set, and only grows from there. nil means "no
	// delegated turn has completed in this process yet" — never "zero
	// spend so far" — matching this type's own process-local capture
	// contract (see this type's own doc comment).
	//
	// A caller that wants to gate a dollar readout on ACTUAL pay-as-you-go
	// billing, not a hypothetical subscription-turn equivalent, must check
	// Overage.InUse alongside this field — a non-nil SessionCostUSD alone
	// is not proof the user was charged anything.
	SessionCostUSD *float64 `json:"session_cost_usd,omitempty"`
}

// SubscriptionUsageWindow is one rate-limit window inside a
// SubscriptionUsage snapshot — e.g. claude's "five_hour"/"seven_day", or
// codex's "primary"/"secondary"/"bengalfox_primary".
type SubscriptionUsageWindow struct {
	// Key is the capturing lane's own stable window identifier. A caller
	// tracking one particular window turn over turn keys off this, not
	// Label.
	Key string `json:"key"`
	// Label is a short human-readable name for the window (e.g. "5-hour",
	// "Weekly"), derived by the capturing lane — neither provider sends a
	// label on the wire.
	Label string `json:"label"`
	// UsedPercent is this window's utilization, 0-100.
	UsedPercent float64 `json:"used_percent"`
	// ResetsAt is when this window resets, Unix seconds.
	ResetsAt int64 `json:"resets_at"`
}

// SubscriptionOverage describes a subscription's pay-as-you-go overage
// state — see SubscriptionUsage.Overage's own doc comment for why this is
// claude-only today.
type SubscriptionOverage struct {
	InUse    bool   `json:"in_use"`
	Status   string `json:"status"`
	ResetsAt int64  `json:"resets_at"`
}
