package turn

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/protocol"
)

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

// ErrEmptySummary reports a summary call that returned no text.
var ErrEmptySummary = errors.New("turn: the compaction summary is empty")

// Summarize makes one model call, with no tools, that summarizes
// req.History, and returns the plain summary and the usage of the calls. lim bounds the silence and the retries of a call, as it does for a turn.
func Summarize(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
	req.Instructions, req.MaxTokens = summaryPrompt, summaryMaxTokens
	req.History = append(slices.Clone(req.History), protocol.LogMessage{Role: protocol.RoleUser,
		Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: summaryInstruction}}})
	text, usage, err := ask(ctx, b, req, lim)
	if err != nil {
		return "", usage, err
	}
	if strings.TrimSpace(text) == "" {
		return "", usage, ErrEmptySummary
	}
	return text, usage, nil
}

// Ask makes a model call of req with no tools, as Summarize does, and returns its text and usage.
func Ask(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
	return ask(ctx, b, req, lim)
}

func ask(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
	req.Input, req.Tools, req.Call, req.Steered = nil, nil, nil, nil
	req.AcceptCut, req.OneShot = true, true
	if b.Capabilities(req.Model).OwnsLoop {
		lim.Retries = 0
	}
	system, next := req.Instructions, req.Sent
	req.Sent = func(s string) {
		system = s
		if next != nil {
			next(s)
		}
	}
	start := time.Now()
	var a answer
	err := watch(ctx, b, req, &a, lim.Idle)
	for n := 1; n <= lim.Retries && a.text.Len() == 0 && retryable(err); n++ {
		if err = sleep(ctx, retryDelay(n, err)); err == nil {
			err = watch(ctx, b, req, &a, lim.Idle)
		}
	}
	logOneShot(cmp.Or(lim.Log, slog.Default()), req, system, a.text.String(), a.usage, err, time.Since(start))
	if err != nil {
		return "", a.usage, err
	}
	return a.text.String(), a.usage, nil
}

// answer is the Sink of ask. It keeps the text and the usage.
type answer struct {
	text  strings.Builder
	usage protocol.Usage
}

func (a *answer) Item(m protocol.LogMessage) error {
	for _, p := range m.Parts {
		if p.Type == protocol.LogPartText {
			a.text.WriteString(p.Text)
		}
	}
	return nil
}

func (a *answer) Telemetry(t Telemetry) { a.usage = a.usage.Add(t.Usage) }

func (*answer) Delta(string, Delta)                   {}
func (*answer) Alive()                                {}
func (*answer) Discard(string)                        {}
func (*answer) Steer() ([]protocol.LogMessage, error) { return nil, nil }
func (*answer) State(string) (Snapshot, error)        { return Snapshot{}, nil }
func (*answer) SaveState(string, Snapshot) error      { return nil }
func (*answer) Compacted(string) error                { return nil }
func (*answer) Status(protocol.StatusFrame)           {}

func (*answer) Ask(string, string, json.RawMessage) error {
	return errors.New("turn: a model call with no tools opens no request")
}

func (*answer) Asked(string) (json.RawMessage, bool) { return nil, false }

func (*answer) Resolution(string) (protocol.RequestResolved, bool) {
	return protocol.RequestResolved{}, false
}
