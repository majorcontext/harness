package turn

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// summaryInstruction ends the request with a user message: some models
// refuse a conversation that ends with an assistant message.
const summaryInstruction = `Summarize the conversation above so that it can be folded into one message, freeing context for future turns.

Write a concise, information-preserving summary. Preserve:
- the user's intent and goals
- decisions made and their rationale
- concrete facts a later turn depends on: file paths, commands, values, error text

Do not transcribe tool-call arguments or outputs verbatim; describe what happened and why it matters instead. Be dense; omit anything a later turn would not need.

Do not call a tool. Answer only with the summary text.`

// summaryMaxTokens caps the response of a summary call: a concise summary,
// not another turn.
const summaryMaxTokens = 1024

// ErrEmptySummary reports a summary call that returned no text.
var ErrEmptySummary = errors.New("turn: the compaction summary is empty")

// SummaryRequest returns the request that the next model call of the session
// would send, described from src as a turn describes it, with the cap of a
// summary. Summarize adds the question. Backends that cache a prefix of the
// tools, the instructions, and the history read the same prefix in both.
func SummaryRequest(ctx context.Context, b Backend, req Request, src Source) (Request, error) {
	caps := b.Capabilities(req.Model)
	call := req
	if err := applyParams(ctx, b, caps, req, &call); err != nil {
		return Request{}, err
	}
	call.WebSearch = call.SearchAllowed && b.Capabilities(call.Model).WebSearch
	describe(&call, toolsetOf(ctx, &call, src), nil, caps.OwnsLoop)
	call.Call, call.Ask, call.MaxTokens = nil, nil, summaryMaxTokens
	return call, nil
}

// Summarize makes one model call of req, from SummaryRequest, that ends with the
// question for a summary, and returns the plain summary and the usage of the
// calls. A response with no text is ErrEmptySummary. lim bounds the silence and
// the retries of a call, as it does for a turn.
func Summarize(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
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

// Ask makes a model call of req with no tools and returns its text and usage.
func Ask(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
	req.Tools, req.Call = nil, nil
	return ask(ctx, b, req, lim)
}

func ask(ctx context.Context, b Backend, req Request, lim Limits) (string, protocol.Usage, error) {
	req.Input, req.Steered = nil, nil
	req.AcceptCut, req.OneShot = true, true
	ownsLoop := b.Capabilities(req.Model).OwnsLoop
	if ownsLoop {
		lim.Retries = 0
	}
	a := answer{log: cmp.Or(lim.Log, slog.Default())}
	a.call = callOf(req, 0, ownsLoop)
	err := watch(ctx, b, req, &a, lim.Idle)
	for n := 1; n <= lim.Retries && a.text.Len() == 0 && retryable(err); n++ {
		if err = sleep(ctx, retryDelay(n, err)); err == nil {
			a.call = callOf(req, n, ownsLoop)
			err = watch(ctx, b, req, &a, lim.Idle)
		}
	}
	if err != nil {
		return "", a.usage, err
	}
	return a.text.String(), a.usage, nil
}

// answer is the Sink of ask. It keeps the text and the usage.
type answer struct {
	text  strings.Builder
	usage protocol.Usage
	log   *slog.Logger
	call  callInfo
}

func (a *answer) Item(m protocol.LogMessage) error {
	for _, p := range m.Parts {
		if p.Type == protocol.LogPartText {
			a.text.WriteString(p.Text)
		}
	}
	return nil
}

func (a *answer) Telemetry(t Telemetry) {
	if t.Call != nil {
		logCall(a.log, a.call, t)
	}
	a.usage = a.usage.Add(t.Usage)
}

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
