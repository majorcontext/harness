// Package modelapi runs turns on a model API through one provider client
// per wire. It makes one model call per Run; the turn loop runs tools.
package modelapi

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
)

// maxTokens caps a response that sets no cap. The Anthropic API rejects a request without a cap.
const maxTokens = 8192

// defaultContextWindow is the window, in tokens, of a model that has no
// configured window and no entry in the modelmeta table.
const defaultContextWindow = 128000

// warned holds the models that ran on the default window and have logged it,
// once per process.
var warned sync.Map

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

// Capabilities reports the context window of model: the configured window,
// else the modelmeta window, else defaultContextWindow marked as estimated.
// It is the only place that resolves a window.
func (b *Backend) Capabilities(model string) turn.Capabilities {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return turn.Capabilities{}
	}
	if b.window > 0 {
		return turn.Capabilities{ContextWindow: b.window}
	}
	window, ok := modelmeta.ContextWindow(ref)
	if ok {
		return turn.Capabilities{ContextWindow: window}
	}
	if _, seen := warned.LoadOrStore(model, true); !seen {
		slog.Warn("harness: the model has no known context window, so it runs on the default", "model", model, "window", defaultContextWindow)
	}
	return turn.Capabilities{ContextWindow: defaultContextWindow, WindowEstimated: true}
}

// Run makes one model call on the history of req and reports its assistant item.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	preq, err := request(ctx, req)
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
			out.Telemetry(b.telemetry(req.Model, ev.Usage, ev.SubscriptionUsage))
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
	preq, err := request(ctx, req)
	if err != nil {
		return err
	}
	return b.client.(provider.StartupPrewarmer).Warm(ctx, preq)
}

// telemetry reports u, and the prompt of the call as the context reading.
// A call with no prompt tokens reports no reading, so the earlier reading stays.
func (b *Backend) telemetry(model string, u provider.Usage, sub *message.SubscriptionUsage) turn.Telemetry {
	usage := eventlog.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens)}
	t := turn.Telemetry{Usage: usage, SubscriptionUsage: subscription(sub)}
	if tokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens; tokens > 0 {
		caps := b.Capabilities(model)
		t.Context = eventlog.ContextMeasured{Source: b.client.Name(), Tokens: tokens, Window: int64(caps.ContextWindow), WindowEstimated: caps.WindowEstimated}
	}
	return t
}

// subscription maps the snapshot of a provider. The actor stamps the capture time.
func subscription(s *message.SubscriptionUsage) *eventlog.SubscriptionUsage {
	if s == nil {
		return nil
	}
	u := &eventlog.SubscriptionUsage{Provider: s.Provider, Plan: s.Plan, Windows: make([]eventlog.SubscriptionUsageWindow, len(s.Windows))}
	for i, w := range s.Windows {
		u.Windows[i] = eventlog.SubscriptionUsageWindow(w)
	}
	if o := s.Overage; o != nil {
		u.Overage = &eventlog.SubscriptionOverage{InUse: o.InUse, Status: o.Status, ResetsAt: o.ResetsAt}
	}
	return u
}

// Close closes the pooled connections of a client that pools them. Call it
// when no Run is active. A later Run dials again.
func (b *Backend) Close() {
	if c, ok := b.client.(interface{ Close() }); ok {
		c.Close()
	}
}

func request(ctx context.Context, req turn.Request) (*provider.Request, error) {
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
		msgs[i], err = toMessage(ctx, req, m)
		if err != nil {
			return nil, err
		}
	}
	if req.Banner != "" {
		banner := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.EngineContext{Text: req.Banner}}}
		msgs = slices.Insert(msgs, min(req.BannerAt, len(msgs)), banner)
	}
	tools := make([]provider.ToolDef, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = provider.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	preq := &provider.Request{Model: ref, Messages: msgs, Tools: tools, MaxTokens: cmp.Or(req.MaxTokens, maxTokens), Effort: effort, ServiceTier: req.Settings.ServiceTier,
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
