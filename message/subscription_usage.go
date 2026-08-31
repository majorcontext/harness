package message

// SubscriptionUsage is a normalized snapshot of a subscription-backed
// session's provider-reported rate-limit/quota signal, captured — not
// polled: nothing that produces one makes an extra outbound request for it.
// It exists for the two lanes that run a turn against a user's own model
// subscription rather than metered API access:
//
//   - "claude": engine/claude_code_backend.go decodes it from the `claude`
//     CLI's own rate_limit_event stream-json message.
//   - "codex": provider/openai decodes it from the ChatGPT Codex backend's
//     x-codex-* response headers (HTTP and websocket transports alike).
//
// A session that has never delegated a turn through either lane, or has
// but whose first turn has not yet completed in this process, has no
// SubscriptionUsage — see engine.Session.SubscriptionUsage's own doc
// comment for why this is a process-local snapshot, not a durable record.
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
