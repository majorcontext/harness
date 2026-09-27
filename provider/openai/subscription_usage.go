package openai

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/message"
)

// CodexFamily is the conventional provider/openai Client.Family value for a
// providers-map entry that speaks the ChatGPT Codex backend's Responses
// wire (chatgpt.com/backend-api/codex/responses) — see cmd/harness's
// registerOpenAIProviders, where a config.TypeOpenAI entry's Family is set
// to its own providers-map key. Only a client whose resolved family equals
// this constant captures a Codex subscription-usage signal — the x-codex-*
// HTTP response headers (see Client.Stream) or the codex.rate_limits
// websocket event (see stream.handle); an ordinary "openai" entry never
// reads or reports either.
//
// This is a naming convention, not something buildsResponsesAdapter or any
// other config validation enforces — the same "the operator's own key IS
// the signal" precedent engine.ClaudeCodeProviderFamily documents for the
// Claude Code delegated backend, applied here because nothing in a
// provider.Request or an HTTP response can otherwise tell this package
// "this endpoint is the ChatGPT Codex backend" without adding a dedicated
// config field for a single conventionally-named deployment.
const CodexFamily = "codex"

// codexWindowLabel maps an x-codex-*-window-minutes value to the human
// label message.SubscriptionUsageWindow.Label reports. 10080 (7 days) and
// 300 (5 hours) are the two windows a real Codex backend sends today;
// anything else falls back to "<minutes>-min" rather than a hardcoded
// guess for a window this file has not seen.
func codexWindowLabel(minutes int64) string {
	switch minutes {
	case 10080:
		return "Weekly"
	case 300:
		return "5-hour"
	default:
		return strconv.FormatInt(minutes, 10) + "-min"
	}
}

// codexHeaderFloat parses header key h.Get(key) as a float64, returning
// ok=false for an absent or unparseable value — the same permissive-
// decoding posture engine/claude_code_backend.go takes with the sibling
// subscription lane: a header this file cannot parse is treated as absent,
// never a hard failure.
func codexHeaderFloat(h http.Header, key string) (float64, bool) {
	v := h.Get(key)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// codexHeaderInt is codexHeaderFloat's integer twin, used for the
// window-minutes and reset-at (Unix seconds) headers.
func codexHeaderInt(h http.Header, key string) (int64, bool) {
	v := h.Get(key)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// codexWindow reads one x-codex-<prefix>-{used-percent,window-minutes,
// reset-at} header trio into a message.SubscriptionUsageWindow keyed
// exactly as prefix. ok is false when window-minutes is absent, unparsable,
// or not positive — the same "window-minutes>0" presence gate for every
// window this file reads, primary/secondary/bengalfox-primary alike: a
// window a real Codex response reports as 0-minute-wide (e.g. the example
// capture's unused x-codex-secondary-window-minutes: 0) is not in use and
// must not appear as a hollow zero-value entry.
func codexWindow(prefix string, h http.Header) (message.SubscriptionUsageWindow, bool) {
	minutes, ok := codexHeaderInt(h, "x-codex-"+prefix+"-window-minutes")
	if !ok || minutes <= 0 {
		return message.SubscriptionUsageWindow{}, false
	}
	used, _ := codexHeaderFloat(h, "x-codex-"+prefix+"-used-percent")
	resetsAt, _ := codexHeaderInt(h, "x-codex-"+prefix+"-reset-at")
	return message.SubscriptionUsageWindow{
		Key:         prefix,
		Label:       codexWindowLabel(minutes),
		UsedPercent: used,
		ResetsAt:    resetsAt,
	}, true
}

// codexSubscriptionUsageFromHeaders maps the ChatGPT Codex backend's
// x-codex-* response headers into message.SubscriptionUsage — present on
// every chatgpt.com/backend-api/codex/responses HTTP reply (see
// Client.codexSubscriptionUsage, its only caller: the websocket transport's
// upgrade response carries none of these headers, so its subscription
// snapshot comes from codexSubscriptionUsageFromRateLimitsEvent instead).
// Windows, in order: "primary" (the plan's own primary window — Weekly at
// 10080 minutes in the documented capture); "bengalfox_primary" (a second,
// separately-named 5-hour+weekly bucket riding alongside the plan windows —
// only its primary/5-hour window is captured); "secondary", when its own
// window-minutes is positive (the documented capture's secondary is
// unused: window-minutes 0, reset-at empty).
//
// Overage is never set: the codex lane's headers carry no overage concept
// (credits are a separate, out-of-scope system — see this file's own
// CONSTRAINTS). Returns nil when the response carries neither a plan nor
// any window at all — a "codex"-family client that reached a plain,
// non-Codex OpenAI-compatible endpoint by misconfiguration, or an older
// backend build that has not shipped these headers yet — so a caller only
// ever applies a genuinely captured signal, never a hollow zero-value one.
func codexSubscriptionUsageFromHeaders(h http.Header) *message.SubscriptionUsage {
	plan := h.Get("x-codex-plan-type")
	windows := []message.SubscriptionUsageWindow{}
	if w, ok := codexWindow("primary", h); ok {
		windows = append(windows, w)
	}
	if w, ok := codexWindow("bengalfox-primary", h); ok {
		w.Key = "bengalfox_primary"
		windows = append(windows, w)
	}
	if w, ok := codexWindow("secondary", h); ok {
		windows = append(windows, w)
	}
	if plan == "" && len(windows) == 0 {
		return nil
	}
	return &message.SubscriptionUsage{
		Provider: "codex",
		Plan:     plan,
		Windows:  windows,
	}
}

// codexRateLimitsEvent is the codex.rate_limits websocket event's wire
// shape (codex-rs/codex-api/src/rate_limits.rs's RateLimitEvent in the
// Codex CLI source). Fields this file does not read (credits,
// metered_limit_name, limit_name, allowed, limit_reached,
// code_review_rate_limits) are ignored.
type codexRateLimitsEvent struct {
	PlanType   string                       `json:"plan_type"`
	RateLimits *codexRateLimitsEventDetails `json:"rate_limits"`
}

type codexRateLimitsEventDetails struct {
	Primary   *codexRateLimitsEventWindow `json:"primary"`
	Secondary *codexRateLimitsEventWindow `json:"secondary"`
}

type codexRateLimitsEventWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes *int64  `json:"window_minutes"`
	ResetAt       *int64  `json:"reset_at"`
}

// codexSubscriptionUsageFromRateLimitsEvent maps one codex.rate_limits
// websocket event into message.SubscriptionUsage — the ws transport's
// analog of codexSubscriptionUsageFromHeaders. rate_limits, and each of
// its primary/secondary windows, can be absent; a present window's
// window_minutes and reset_at are themselves optional (RateLimitEventWindow
// declares both Option). Returns nil, nil when the event carries neither a
// plan nor any window.
func codexSubscriptionUsageFromRateLimitsEvent(data []byte) (*message.SubscriptionUsage, error) {
	var ev codexRateLimitsEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, err
	}
	windows := []message.SubscriptionUsageWindow{}
	if ev.RateLimits != nil {
		if w, ok := codexRateLimitEventWindow("primary", ev.RateLimits.Primary); ok {
			windows = append(windows, w)
		}
		if w, ok := codexRateLimitEventWindow("secondary", ev.RateLimits.Secondary); ok {
			windows = append(windows, w)
		}
	}
	if ev.PlanType == "" && len(windows) == 0 {
		return nil, nil
	}
	return &message.SubscriptionUsage{
		Provider: "codex",
		Plan:     ev.PlanType,
		Windows:  windows,
	}, nil
}

// codexRateLimitEventWindow maps one present window (ok=false only when w
// is nil, e.g. an absent secondary) to key, reusing codexWindowLabel for
// its human label.
func codexRateLimitEventWindow(key string, w *codexRateLimitsEventWindow) (message.SubscriptionUsageWindow, bool) {
	if w == nil {
		return message.SubscriptionUsageWindow{}, false
	}
	var label string
	if w.WindowMinutes != nil {
		label = codexWindowLabel(*w.WindowMinutes)
	}
	var resetsAt int64
	if w.ResetAt != nil {
		resetsAt = *w.ResetAt
	}
	return message.SubscriptionUsageWindow{
		Key:         key,
		Label:       label,
		UsedPercent: w.UsedPercent,
		ResetsAt:    resetsAt,
	}, true
}
