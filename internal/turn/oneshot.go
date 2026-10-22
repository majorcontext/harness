package turn

import (
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/majorcontext/harness/internal/toolresult"
	"github.com/majorcontext/harness/protocol"
)

// The caps of a oneshot_call line keep its text near 73 KiB, so even with
// every byte escaped the line fits one batch of the log collector.
const (
	oneShotSystemBytes   = 32 << 10
	oneShotMessagesBytes = 32 << 10
	oneShotMessages      = 64
	oneShotAnswerBytes   = 8 << 10
	oneShotErrorBytes    = 1 << 10
)

type oneShotMessage struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// logOneShot writes the oneshot_call line of one model call with no tools:
// what the backend sent and the raw answer, which no event records.
func logOneShot(log *slog.Logger, req Request, system, answer string, usage protocol.Usage, err error, took time.Duration) {
	args := []any{
		"session_id", req.SessionID,
		"turn_id", req.TurnID,
		"model", req.Model,
		"duration_ms", took.Milliseconds(),
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"system_bytes", len(system),
		"system", masked(system, oneShotSystemBytes),
		"messages_count", len(req.History),
		"messages", oneShotMessagesOf(req.History),
		"answer", masked(answer, oneShotAnswerBytes),
	}
	if err != nil {
		args = append(args, "error", masked(err.Error(), oneShotErrorBytes))
	}
	log.Info("oneshot_call", args...)
}

// oneShotMessagesOf returns the text of each message in order, until the
// texts fill oneShotMessagesBytes or the count reaches oneShotMessages.
func oneShotMessagesOf(history []protocol.LogMessage) []oneShotMessage {
	out := []oneShotMessage{}
	left := oneShotMessagesBytes
	for _, m := range history {
		if left <= 0 || len(out) == oneShotMessages {
			break
		}
		var texts []string
		for _, p := range m.Parts {
			if p.Type == protocol.LogPartText {
				texts = append(texts, p.Text)
			}
		}
		text := masked(strings.Join(texts, "\n"), left)
		left -= len(text)
		out = append(out, oneShotMessage{Role: string(m.Role), Text: text})
	}
	return out
}

// masked masks the secrets that toolresult.Mask recognizes, then cuts the
// text to at most n bytes on a UTF-8 boundary.
func masked(s string, n int) string {
	s = toolresult.Mask(s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
