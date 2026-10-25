package turn

import "time"

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
	ChainRefusal, ChainRefusalDetail                string
	ChainRefusalItem                                *int
	ChainRefusalOffset                              *int
	ChainRefusalPreviousLen, ChainRefusalCurrentLen *int
	// ChainRefusalTool names the first tool whose entry changed, and ChainRefusalToolPart the part of it.
	ChainRefusalTool, ChainRefusalToolPart string
	// ResponseID is the provider id of the response. StopReason is the stop reason as the backend maps it,
	// IncompleteReason the raw reason of an incomplete response, and ErrorClass the class of a call that failed.
	ResponseID, StopReason, IncompleteReason, ErrorClass string
	// PreviousResponseID is the response that a chained call continued; it is for the model-call span and no log line.
	PreviousResponseID string
	// ResponseIDs are the provider ids of the model calls that one call span covers, in order; they are for the span events and no log line.
	ResponseIDs []string
	// ProviderTokens is the usage as the provider reported it, or nil when its terminal event had none.
	ProviderTokens *ProviderTokens
	// The byte sizes of the request and its parts. RequestBytes 0 means the backend reports none.
	// MaxOutputTokens is 0 when the request sent none.
	RequestBytes, InstructionsBytes, ToolsBytes, HistoryBytes, MaxOutputTokens int
}

// ProviderTokens is the token count that a provider reported, before the backend splits it.
type ProviderTokens struct {
	Input, Output, Cached int
}

// Chained reports a call that sent only the input that the earlier response
// lacked, and did not recover from a miss.
func (m CallMetrics) Chained() bool {
	return m.RequestMode == "incremental" && m.PreviousResponseUsed && !m.ChainRecovered
}

// Purpose names the operation of a model call on its turn_metrics line.
type Purpose string

// These constants keep metric labels consistent across request builders.
const (
	PurposeTurn       Purpose = "turn"
	PurposeCompaction Purpose = "compaction"
	PurposeJudge      Purpose = "judge"
	PurposeTitle      Purpose = "title"
	PurposeGenerate   Purpose = "generate"
)

// callInfo is the part of a model call request that its turn_metrics line names.
type callInfo struct {
	sessionID, model, tier, effort string
	purpose                        Purpose
	systemLen, tools               int
	// retry counts the earlier attempts of the call that failed.
	retry int
	callIdent
}

// callOf names a call. A backend that owns the loop reports no request size
// and no tier or effort.
func callOf(req Request, id callIdent, retry int, ownsLoop bool) callInfo {
	if ownsLoop {
		return callInfo{sessionID: req.SessionID, model: req.Model, purpose: req.Purpose, retry: retry, callIdent: id}
	}
	return callInfo{sessionID: req.SessionID, model: req.Model, tier: req.Settings.ServiceTier, effort: req.Settings.Effort,
		purpose: req.Purpose, systemLen: len(req.Instructions), tools: len(req.Tools), retry: retry, callIdent: id}
}

// Telemetry logs the turn_metrics line of a completed model call, then passes t on.
func (s *sink) Telemetry(t Telemetry) {
	if t.Call != nil {
		s.rec.record(t)
	}
	s.Turn.Telemetry(t)
}

// callArgs are the keys of the turn_metrics line of a completed model call.
// A key that means "the backend chose" when absent is left out, so a query
// counts it apart from any named value.
func callArgs(c callInfo, t Telemetry) []any {
	m := t.Call
	args := []any{
		"session_id", c.sessionID,
		"call_id", c.callID,
		"model", c.model,
		"purpose", string(c.purpose),
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
	if c.turnID != "" {
		args = append(args, "turn_id", c.turnID, "call_seq", c.seq)
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
		if m.ChainRefusalOffset != nil {
			args = append(args, "chain_refusal_offset", *m.ChainRefusalOffset)
		}
		if m.ChainRefusalPreviousLen != nil && m.ChainRefusalCurrentLen != nil {
			args = append(args, "chain_refusal_previous_len", *m.ChainRefusalPreviousLen, "chain_refusal_current_len", *m.ChainRefusalCurrentLen)
		}
	}
	return append(args, requestArgs(m)...)
}

// requestArgs are the keys of a turn_metrics line that name the response and the size of the request.
func requestArgs(m *CallMetrics) []any {
	var args []any
	for _, kv := range []struct{ k, v string }{
		{"response_id", m.ResponseID}, {"stop_reason", m.StopReason}, {"incomplete_reason", m.IncompleteReason},
		{"chain_refusal_tool", m.ChainRefusalTool}, {"chain_refusal_tool_part", m.ChainRefusalToolPart}, {"error_class", m.ErrorClass},
	} {
		if kv.v != "" {
			args = append(args, kv.k, kv.v)
		}
	}
	if p := m.ProviderTokens; p != nil {
		args = append(args, "provider_input_tokens", p.Input, "provider_output_tokens", p.Output, "provider_cached_tokens", p.Cached)
	}
	if m.RequestBytes > 0 {
		var maxOutput any = "omitted"
		if m.MaxOutputTokens > 0 {
			maxOutput = m.MaxOutputTokens
		}
		args = append(args, "request_bytes", m.RequestBytes, "instructions_bytes", m.InstructionsBytes,
			"tools_bytes", m.ToolsBytes, "history_bytes", m.HistoryBytes, "max_output_tokens", maxOutput)
	}
	return args
}
