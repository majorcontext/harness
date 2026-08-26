package engine

import (
	"log/slog"
	"os"

	"github.com/majorcontext/harness/message"
)

// TurnMetrics summarizes one completed streamTurn model call: request
// latency broken into time-to-first-token and stream duration, the token and
// prompt-cache accounting the provider reported, and the shape of the
// request that produced it. See Config.OnTurnMetrics.
//
// SessionID, Model, SystemLen, and ToolsCount are deliberately computed the
// same way the server's request.meta record computes them (see
// server/journal.go's OnRequest: SystemLen is len(strings.Join(req.System,
// "\n")), ToolsCount is len(req.Tools)) so a turn_metrics log line and the
// request.meta record for the same turn share a natural join key —
// session_id, model, and system_len together identify the same request on
// both sides, without threading a new shared ID through the provider
// boundary.
type TurnMetrics struct {
	// SessionID is the firing session's own ID, mirroring OnRequest's
	// sessionID parameter (see its doc comment for why this is passed
	// explicitly rather than closed over: a Spawn'd child must report its
	// own ID, never its parent's).
	SessionID string
	// Model is the full "provider/model" ref this turn was sent to.
	Model message.ModelRef
	// Attempt is streamTurnWithRetry's 1-indexed attempt counter: 1 for a
	// turn that succeeded on its first try, 2+ when a prior attempt this
	// turn failed retryably and was re-issued. Named "retry" on the wire
	// (see defaultTurnMetricsLog) because that is the operator-facing
	// question: did this completed call cost more than one model request.
	Attempt int
	// TTFTMillis is the elapsed time from just before the request was sent
	// (prov.Stream) to the first non-activity stream event — the first
	// event carrying content or, if the provider streams nothing before its
	// terminal event, the usage-bearing EventDone itself.
	TTFTMillis int64
	// StreamMillis is the elapsed time from that first delta to EventDone.
	// Zero when EventDone was itself the first delta (TTFTMillis already
	// covers the whole call in that case).
	StreamMillis int64
	// InputTokens, OutputTokens, CacheReadTokens, and CacheWriteTokens are
	// provider.Usage passed through verbatim from EventDone.
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	// SystemLen is the byte length of this request's joined system prompt
	// (see the type doc comment above for why this must match request.meta's
	// own computation exactly).
	SystemLen int
	// ToolsCount is the number of tools offered on this request.
	ToolsCount int
}

// defaultTurnMetricsStderr is the JSON handler every default turn_metrics
// emit writes through. It is a package-level var (never per-Session) so a
// test that swaps Config.OnTurnMetrics for a recorder pays nothing for it,
// and so every session in one process shares one handler exactly like
// slog.Default() would, but pinned to os.Stderr regardless of what
// slog.SetDefault has been called with elsewhere.
//
// This deliberately targets stderr, not the stderr every other structured
// log line in this repo uses (see cmd/harness/main.go's serveCmd/runCmd
// "Structured logging: JSON to stderr" comment) — a per-turn line is
// operational telemetry a deployment's log pipeline scrapes from a
// process's stderr stream (see the architecture note "event streams on
// stderr" in AGENTS.md), not a diagnostic a human tails on the terminal
// alongside everything else on stderr.
var defaultTurnMetricsStderr = slog.New(slog.NewJSONHandler(os.Stderr, nil))

// defaultTurnMetricsLog is Config.OnTurnMetrics's default when the embedder
// sets none: one structured "turn_metrics" line per completed model call.
// Field names match the wire vocabulary a log query (grep, a BetterStack/
// Vector-style query) is expected to filter on.
func defaultTurnMetricsLog(m TurnMetrics) {
	defaultTurnMetricsStderr.Info("turn_metrics",
		"session_id", m.SessionID,
		"model", m.Model.String(),
		"ttft_ms", m.TTFTMillis,
		"stream_ms", m.StreamMillis,
		"input_tokens", m.InputTokens,
		"output_tokens", m.OutputTokens,
		"cache_read_tokens", m.CacheReadTokens,
		"cache_write_tokens", m.CacheWriteTokens,
		"system_len", m.SystemLen,
		"tools_count", m.ToolsCount,
		"retry", m.Attempt,
	)
}

// emitTurnMetrics dispatches m to Config.OnTurnMetrics, or
// defaultTurnMetricsLog when the embedder configured none. Unlike OnRequest
// (nil means "no observer, skip the call entirely"), turn metrics always go
// somewhere: a box or CLI process with no embedder-supplied callback still
// gets the stderr line, which is the whole point of the seam — see the
// Config.OnTurnMetrics doc comment.
func (s *Session) emitTurnMetrics(m TurnMetrics) {
	cb := s.cfg.OnTurnMetrics
	if cb == nil {
		cb = defaultTurnMetricsLog
	}
	cb(m)
}
