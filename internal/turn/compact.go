package turn

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
)

// SummaryBanner leads each compaction summary, so that no reader takes the
// summary for text that the user typed.
const SummaryBanner = "[compacted summary of earlier conversation]\n\n"

const summaryPrompt = `You are summarizing a prefix of an ongoing agent conversation so it can be folded into one message, freeing context for future turns.

Write a concise, information-preserving summary. Preserve:
- the user's intent and goals
- decisions made and their rationale
- concrete facts a later turn depends on: file paths, commands, values, error text

Do not transcribe tool-call arguments or outputs verbatim; describe what happened and why it matters instead. Be dense; omit anything a later turn would not need.`

// summaryInstruction ends the request with a user message: some models
// refuse a conversation that ends with an assistant message.
const summaryInstruction = "Summarize the conversation above, following the system prompt's instructions."

var errEmptySummary = errors.New("turn: the compaction summary is empty")

// Summarize makes one model call, with no tools, that summarizes
// req.History, and returns the summary after SummaryBanner. A positive idle
// bounds the silence of the call, as Limits.Idle does.
func Summarize(ctx context.Context, b Backend, req Request, idle time.Duration) (string, error) {
	req.Instructions = summaryPrompt
	req.History = append(slices.Clone(req.History), eventlog.Message{Role: eventlog.RoleUser,
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: summaryInstruction}}})
	req.Input, req.Tools, req.Call, req.Steered = nil, nil, nil, nil
	var s summary
	if _, err := watch(ctx, b, req, &s, idle); err != nil {
		return "", err
	}
	if strings.TrimSpace(s.text.String()) == "" {
		return "", errEmptySummary
	}
	return SummaryBanner + s.text.String(), nil
}

// summary is the Sink of a summary call. It keeps only the text.
type summary struct{ text strings.Builder }

func (s *summary) Item(m eventlog.Message) error {
	for _, p := range m.Parts {
		if p.Type == eventlog.PartText {
			s.text.WriteString(p.Text)
		}
	}
	return nil
}

func (*summary) Delta(string, Delta)                {}
func (*summary) Alive()                             {}
func (*summary) Telemetry(Telemetry)                {}
func (*summary) Steer() ([]eventlog.Message, error) { return nil, nil }
func (*summary) State(string) ([]byte, error)       { return nil, nil }
func (*summary) SaveState(string, []byte) error     { return nil }
func (*summary) Compacted(string) error             { return nil }
