package turn

import (
	"log/slog"
	"time"
)

// CallMetrics is what a backend measured of one completed model call: the
// transport timing and, for a transport that projects the request, its shape.
type CallMetrics struct {
	// TTFT is the time from the request to its first content.
	TTFT time.Duration
	// Stream is the time from that first content to the end of the response.
	Stream time.Duration
	// RequestMode is "full" or "incremental"; empty when the transport reports none.
	RequestMode                          string
	CompleteInputItems, SentInputItems   int
	PreviousResponseUsed, ChainRecovered bool
	// ChainRefusal says why a call that could have chained sent the whole
	// input; ChainRefusalDetail names it and ChainRefusalItem locates it.
	ChainRefusal, ChainRefusalDetail string
	ChainRefusalItem                 *int
}

// Chained reports a call that sent only the input that the earlier response
// lacked, and did not recover from a miss.
func (m CallMetrics) Chained() bool {
	return m.RequestMode == "incremental" && m.PreviousResponseUsed && !m.ChainRecovered
}

// callInfo is the part of a model call request that its turn_metrics line names.
type callInfo struct {
	sessionID, model, tier, effort string
	systemLen, tools               int
	// retry counts the earlier attempts of the call that failed.
	retry int
}

// callOf names a call. A backend that owns its loop reports no request size
// and no tier or effort.
func callOf(req Request, retry int, ownsLoop bool) callInfo {
	if ownsLoop {
		return callInfo{sessionID: req.SessionID, model: req.Model, retry: retry}
	}
	return callInfo{sessionID: req.SessionID, model: req.Model, tier: req.Settings.ServiceTier, effort: req.Settings.Effort,
		systemLen: len(req.Instructions), tools: len(req.Tools), retry: retry}
}

// Telemetry logs the turn_metrics line of a completed model call, then passes t on.
func (s *sink) Telemetry(t Telemetry) {
	if t.Call != nil {
		logCall(s.call, t)
	}
	s.Turn.Telemetry(t)
}

// logCall writes one turn_metrics line. A key that means "the backend chose"
// when absent is left out, so a query counts it apart from any named value.
func logCall(c callInfo, t Telemetry) {
	m := t.Call
	args := []any{
		"session_id", c.sessionID,
		"model", c.model,
		"ttft_ms", m.TTFT.Milliseconds(),
		"stream_ms", m.Stream.Milliseconds(),
		"input_tokens", t.Usage.InputTokens,
		"output_tokens", t.Usage.OutputTokens,
		"cache_read_tokens", t.Usage.CacheReadTokens,
		"cache_write_tokens", t.Usage.CacheWriteTokens,
		"system_len", c.systemLen,
		"tools_count", c.tools,
		"retry", c.retry,
	}
	if m.RequestMode != "" {
		args = append(args,
			"request_mode", m.RequestMode,
			"complete_input_items", m.CompleteInputItems,
			"sent_input_items", m.SentInputItems,
			"previous_response_used", m.PreviousResponseUsed,
			"chain_recovered", m.ChainRecovered,
		)
	}
	if c.tier != "" {
		args = append(args, "service_tier", c.tier)
	}
	if c.effort != "" {
		args = append(args, "effort", c.effort)
	}
	if m.ChainRefusal != "" {
		args = append(args, "chain_refusal", m.ChainRefusal)
		if m.ChainRefusalDetail != "" {
			args = append(args, "chain_refusal_detail", m.ChainRefusalDetail)
		}
		// A number: the log ingest reads "input[139]" as a path and keeps only "input".
		if m.ChainRefusalItem != nil {
			args = append(args, "chain_refusal_item", *m.ChainRefusalItem)
		}
	}
	slog.Info("turn_metrics", args...)
}
