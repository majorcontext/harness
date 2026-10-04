// Package modelapi runs turns on a model API through one provider client
// per wire. It makes one model call per Run; the turn loop runs tools.
package modelapi

import (
	"context"
	"fmt"
	"io"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	"github.com/majorcontext/harness/provider"
)

// maxTokens caps each response. The Anthropic API rejects a request without a cap.
const maxTokens = 8192

// Backend is a turn.Backend over one provider client.
type Backend struct {
	client provider.Provider
	// window overrides the modelmeta context window when positive.
	window int
}

// New returns the Backend of client p. A positive window replaces the
// modelmeta context window of every model.
func New(p provider.Provider, window int) *Backend {
	return &Backend{client: p, window: window}
}

// Capabilities reports the context window of model when it is known or overridden.
func (b *Backend) Capabilities(model string) turn.Capabilities {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return turn.Capabilities{}
	}
	if b.window > 0 {
		return turn.Capabilities{ContextWindow: b.window}
	}
	window, _ := modelmeta.ContextWindow(ref)
	return turn.Capabilities{ContextWindow: window}
}

// Run makes one model call on the history of req and reports its assistant item.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	preq, err := request(req)
	if err != nil {
		return turn.Result{}, err
	}
	st, err := b.client.Stream(ctx, preq)
	if err != nil {
		return turn.Result{}, classify(err)
	}
	defer func() { _ = st.Close() }()
	var res turn.Result
	for {
		// A truncated stream wraps io.EOF, so only the bare sentinel is a clean end.
		ev, err := st.Next()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return turn.Result{}, classify(err)
		}
		switch ev.Type {
		case provider.EventTextDelta:
			out.Delta(ev.ID, turn.Delta{Type: eventlog.PartText, Text: ev.Text})
		case provider.EventReasoningDelta:
			out.Delta(ev.ID, turn.Delta{Type: eventlog.PartReasoning, Text: ev.Text})
		case provider.EventDone:
			out.Telemetry(b.telemetry(req.Model, ev.Usage))
			res.MaxTokens = ev.StopReason == provider.StopMaxTokens
			m := fromMessage(ev.Message)
			if !hasOutput(m) {
				return turn.Result{}, fmt.Errorf("%w: modelapi: the response has no output", turn.ErrRetryable)
			}
			if err := out.Item(m); err != nil {
				return turn.Result{}, err
			}
		default:
			out.Alive()
		}
	}
}

// CanWarm reports whether the client has a warm-up for its transport. It has
// no side effect.
func (b *Backend) CanWarm(string) bool {
	w, ok := b.client.(provider.StartupPrewarmer)
	return ok && w.StartupPrewarmEnabled()
}

// Warm prepares the transport of the client for the first model call of req.
// It does nothing when CanWarm is false.
func (b *Backend) Warm(ctx context.Context, req turn.Request) error {
	if !b.CanWarm(req.Model) {
		return nil
	}
	preq, err := request(req)
	if err != nil {
		return err
	}
	return b.client.(provider.StartupPrewarmer).Warm(ctx, preq)
}

// telemetry reports u, and the prompt of the call as the context reading.
// A call with no prompt tokens reports no reading, so the earlier reading stays.
func (b *Backend) telemetry(model string, u provider.Usage) turn.Telemetry {
	usage := eventlog.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens)}
	t := turn.Telemetry{Usage: usage}
	if tokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens; tokens > 0 {
		t.Context = eventlog.ContextMeasured{Source: b.client.Name(), Tokens: tokens, Window: int64(b.Capabilities(model).ContextWindow)}
	}
	return t
}

// Close closes the pooled connections of a client that pools them. Call it
// when no Run is active. A later Run dials again.
func (b *Backend) Close() {
	if c, ok := b.client.(interface{ Close() }); ok {
		c.Close()
	}
}

func request(req turn.Request) (*provider.Request, error) {
	ref, err := message.ParseModelRef(req.Model)
	if err != nil {
		return nil, err
	}
	effort, err := message.ParseEffort(req.Settings.Effort)
	if err != nil {
		return nil, err
	}
	msgs := make([]message.Message, len(req.History))
	for i, m := range req.History {
		msgs[i] = toMessage(m)
	}
	tools := make([]provider.ToolDef, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = provider.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	preq := &provider.Request{Model: ref, Messages: msgs, Tools: tools, MaxTokens: maxTokens, Effort: effort, ServiceTier: req.Settings.ServiceTier,
		SessionKey: req.SessionID}
	if req.Instructions != "" {
		preq.System = []string{req.Instructions}
	}
	return preq, nil
}

func classify(err error) error {
	_, retryable := provider.AsRetryable(err)
	_, exhausted := provider.AsProviderExhausted(err)
	switch {
	case exhausted:
		return fmt.Errorf("%w: %w", turn.ErrExhausted, err)
	case provider.IsContextOverflow(err):
		return fmt.Errorf("%w: %w", turn.ErrContextOverflow, err)
	case retryable:
		return fmt.Errorf("%w: %w", turn.ErrRetryable, err)
	}
	return err
}
