package openai

import (
	"net/http"
	"strconv"

	"github.com/majorcontext/harness/message"
)

// CodexFamily is the conventional provider/openai Client.Family value for a
// providers-map entry that speaks the ChatGPT Codex backend's Responses
// wire (chatgpt.com/backend-api/codex/responses) — see cmd/harness's
// registerOpenAIProviders, where a config.TypeOpenAI entry's Family is set
// to its own providers-map key. Only a client whose resolved family equals
// this constant captures the x-codex-* response headers below (see
// Client.Stream and wsPool.stream); an ordinary "openai" entry never reads
// or reports them.
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
// every chatgpt.com/backend-api/codex/responses reply, HTTP response
// headers or the websocket upgrade response's own header alike (see
// Client.Stream and ws_pool.go's stream, the two callers). Windows, in
// order: "primary" (the plan's own primary window — Weekly at 10080
// minutes in the documented capture); "bengalfox_primary" (a second,
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
//
// Freshness differs by caller: the HTTP path (Client.codexSubscriptionUsage)
// calls this on every single request, so its result is always current as
// of that turn. The websocket path (wsPool.stream) calls this only when
// its pooled connection is dialed, NOT on every turn the connection then
// serves — see wsPoolEntry.subUsage's own doc comment for the resulting
// staleness bound on an actively-reused connection.
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
