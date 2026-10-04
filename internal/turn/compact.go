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

// summaryMaxTokens caps the response of a summary call: a concise summary,
// not another turn.
const summaryMaxTokens = 1024

var errEmptySummary = errors.New("turn: the compaction summary is empty")

// Summarize makes one model call, with no tools, that summarizes
// req.History, and returns the summary after SummaryBanner and the usage of
// the call. A positive idle bounds the silence of the call, as Limits.Idle
// does.
func Summarize(ctx context.Context, b Backend, req Request, idle time.Duration) (string, eventlog.Usage, error) {
	req.Instructions, req.MaxTokens = summaryPrompt, summaryMaxTokens
	req.History = append(slices.Clone(req.History), eventlog.Message{Role: eventlog.RoleUser,
		Parts: []eventlog.Part{{Type: eventlog.PartText, Text: summaryInstruction}}})
	text, usage, err := Ask(ctx, b, req, idle)
	if err != nil {
		return "", usage, err
	}
	if strings.TrimSpace(text) == "" {
		return "", usage, errEmptySummary
	}
	return SummaryBanner + text, usage, nil
}

// Ask makes one model call of req with no tools and returns its text and
// the usage of the call.
func Ask(ctx context.Context, b Backend, req Request, idle time.Duration) (string, eventlog.Usage, error) {
	req.Input, req.Tools, req.Call, req.Steered = nil, nil, nil, nil
	var a answer
	if _, err := watch(ctx, b, req, &a, idle); err != nil {
		return "", a.usage, err
	}
	return a.text.String(), a.usage, nil
}

// answer is the Sink of Ask. It keeps the text and the usage.
type answer struct {
	text  strings.Builder
	usage eventlog.Usage
}

func (a *answer) Item(m eventlog.Message) error {
	for _, p := range m.Parts {
		if p.Type == eventlog.PartText {
			a.text.WriteString(p.Text)
		}
	}
	return nil
}

func (a *answer) Telemetry(t Telemetry) { a.usage = a.usage.Add(t.Usage) }

func (*answer) Delta(string, Delta)                {}
func (*answer) Alive()                             {}
func (*answer) Steer() ([]eventlog.Message, error) { return nil, nil }
func (*answer) State(string) ([]byte, error)       { return nil, nil }
func (*answer) SaveState(string, []byte) error     { return nil }
func (*answer) Compacted(string) error             { return nil }
