// Package modelapi runs turns on a model API through one provider client
// per wire. It makes one model call per Run; the turn loop runs tools.
package modelapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/turn"
)

// maxTokens caps a response that sets no cap. The Anthropic API rejects a request without a cap.
const maxTokens = 8192

// errIncomplete is a response that the output cap cut off. The turn treats it as a stream that ended before its terminal event.
var errIncomplete = errors.New("incomplete response returned, reason: max_tokens")

// errPaused ends a call that still paused after its last resume. It is not retryable: a new call runs the searches again, and bills them.
var errPaused = fmt.Errorf("incomplete response returned, reason: %s", "pause_turn")

// errFiltered is a response that the provider refused or filtered. The turn retries it, as the Codex CLI retries a content filter.
var errFiltered = errors.New("response refused or filtered by the provider")

// defaultContextWindow is the window, in tokens, of a model that has no
// configured window and no entry in the modelmeta table.
const defaultContextWindow = 128000

// warned holds the models that ran on the default window and have logged it, once per process.
var warned sync.Map

// Backend is a turn.Backend over one provider client.
type Backend struct {
	client provider.Provider
	// window overrides the modelmeta context window when positive.
	window int
	log    *slog.Logger
}

// New returns the Backend of client p. A positive window replaces the
// modelmeta context window of every model. A nil log is slog.Default().
func New(p provider.Provider, window int, log *slog.Logger) *Backend {
	return &Backend{client: p, window: window, log: cmp.Or(log, slog.Default())}
}

// Capabilities reports the context window of model: the configured window,
// else the modelmeta window, else defaultContextWindow marked as estimated.
// It is the only place that resolves a window.
func (b *Backend) Capabilities(model string) turn.Capabilities {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return turn.Capabilities{}
	}
	control, _ := b.effortControl(ref)
	caps := turn.Capabilities{Efforts: control.Levels, WebSearch: b.nativeSearch(ref.Model)}
	if b.window > 0 {
		caps.ContextWindow = b.window
		return caps
	}
	window, ok := modelmeta.ContextWindow(ref)
	if ok {
		caps.ContextWindow = window
		return caps
	}
	if _, seen := warned.LoadOrStore(model, true); !seen {
		b.log.Warn("harness: the model has no known context window, so it runs on the default", "model", model, "window", defaultContextWindow)
	}
	caps.ContextWindow, caps.WindowEstimated = defaultContextWindow, true
	return caps
}

// cacheTTLSource is a provider client that states how long its API keeps an idle prompt prefix.
type cacheTTLSource interface {
	CacheLifetime() time.Duration
}

// nativeSearch reports whether the client offers the web search of its API for model.
func (b *Backend) nativeSearch(model string) bool {
	w, ok := b.client.(provider.WebSearcher)
	return ok && w.NativeWebSearch(model)
}

// effortSource is a provider client that knows the effort control of its models.
type effortSource interface {
	EffortControl(model string) (modelmeta.EffortControl, bool)
}

// effortControl returns the effort control of ref from the client, or false
// when the client does not know the model.
func (b *Backend) effortControl(ref modelref.ModelRef) (modelmeta.EffortControl, bool) {
	if s, ok := b.client.(effortSource); ok {
		return s.EffortControl(ref.Model)
	}
	return modelmeta.EffortControl{}, false
}

// Run makes one model call on the history of req and reports its assistant item.
// A response that the output cap cut off records no item and fails as a retryable error.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	preq, err := b.request(ctx, req)
	if err != nil {
		return err
	}
	sentAt := time.Now()
	st, err := b.client.Stream(ctx, preq)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = st.Close() }()
	var firstAt time.Time
	for {
		// A truncated stream wraps io.EOF, so only the bare sentinel is a clean end.
		ev, err := st.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return classify(err)
		}
		if firstAt.IsZero() && ev.Type != provider.EventActivity {
			firstAt = time.Now()
		}
		switch ev.Type {
		case provider.EventTextDelta:
			out.Delta(ev.ID, turn.Delta{Type: protocol.LogPartText, Text: ev.Text})
		case provider.EventReasoningDelta:
			out.Delta(ev.ID, turn.Delta{Type: protocol.LogPartReasoning, Text: ev.Text})
		case provider.EventDone:
			tel := b.telemetry(req.Model, ev.Usage, ev.Prompt, ev.SubscriptionUsage)
			tel.Call = callMetrics(sentAt, firstAt, time.Now(), ev.RequestMetadata)
			out.Telemetry(tel)
			if err := incomplete(ev, req.AcceptCut); err != nil {
				return classify(err)
			}
			m := fromMessage(ev.Message)
			if len(m.Parts) > 0 {
				if err := out.Item(m); err != nil {
					return err
				}
			}
			if ev.StopReason == provider.StopPaused {
				return errPaused
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
	preq, err := b.request(ctx, req)
	if err != nil {
		return err
	}
	return b.client.(provider.StartupPrewarmer).Warm(ctx, preq)
}

// callMetrics measures a call that was sent at sentAt, streamed its first
// content at firstAt, and ended at doneAt. A call whose first event was its
// end has no stream time.
func callMetrics(sentAt, firstAt, doneAt time.Time, md *provider.RequestMetadata) *turn.CallMetrics {
	m := &turn.CallMetrics{TTFT: firstAt.Sub(sentAt), Stream: doneAt.Sub(firstAt)}
	if md != nil {
		m.RequestMode = string(md.Mode)
		m.CompleteInputItems, m.SentInputItems = md.CompleteInputItems, md.SentInputItems
		m.PreviousResponseUsed, m.ChainRecovered = md.PreviousResponseUsed, md.ChainRecovered
		m.ChainRefusal, m.ChainRefusalDetail, m.ChainRefusalItem = string(md.ChainRefusal), md.ChainRefusalDetail, md.ChainRefusalItem
		m.ChainRefusalOffset = md.ChainRefusalOffset
		m.ChainRefusalPreviousLen, m.ChainRefusalCurrentLen = md.ChainRefusalPreviousLen, md.ChainRefusalCurrentLen
	}
	return m
}

// telemetry reports u, and the prompt of the call as the context reading.
// A call with no prompt tokens reports no reading, so the earlier reading stays.
func (b *Backend) telemetry(model string, u provider.Usage, prompt int, sub *message.SubscriptionUsage) turn.Telemetry {
	usage := protocol.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens), WebSearchRequests: int64(u.WebSearchRequests)}
	t := turn.Telemetry{Usage: usage, SubscriptionUsage: subscription(sub)}
	tokens := cmp.Or(int64(prompt), usage.InputTokens+usage.CacheReadTokens+usage.CacheWriteTokens)
	if tokens > 0 {
		caps := b.Capabilities(model)
		t.Context = protocol.ContextMeasured{Source: b.client.Name(), Tokens: tokens, Window: int64(caps.ContextWindow), WindowEstimated: caps.WindowEstimated}
		if c, ok := b.client.(cacheTTLSource); ok {
			t.Context.CacheTTLSeconds = int64(c.CacheLifetime() / time.Second)
		}
	}
	return t
}

// subscription maps the snapshot of a provider. The actor stamps the capture time.
func subscription(s *message.SubscriptionUsage) *protocol.SubscriptionUsage {
	if s == nil {
		return nil
	}
	u := &protocol.SubscriptionUsage{Provider: s.Provider, Plan: s.Plan, Windows: make([]protocol.SubscriptionUsageWindow, len(s.Windows))}
	for i, w := range s.Windows {
		u.Windows[i] = protocol.SubscriptionUsageWindow(w)
	}
	if o := s.Overage; o != nil {
		u.Overage = &protocol.SubscriptionOverage{InUse: o.InUse, Status: o.Status, ResetsAt: o.ResetsAt}
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

func (b *Backend) request(ctx context.Context, req turn.Request) (*provider.Request, error) {
	ref, err := modelref.ParseModelRef(req.Model)
	if err != nil {
		return nil, err
	}
	effort, err := modelref.ParseEffort(req.Settings.Effort)
	if err != nil {
		return nil, err
	}
	control, _ := b.effortControl(ref)
	effort = effort.Fit(control.Levels)
	msgs := make([]message.Message, len(req.History))
	for i, m := range req.History {
		msgs[i], err = b.toMessage(ctx, req, m)
		if err != nil {
			return nil, err
		}
	}
	for i, p := range req.Pins {
		pin := message.Message{Role: message.RoleUser, Parts: message.Parts{&message.EngineContext{Text: p.Text}}}
		msgs = slices.Insert(msgs, min(p.At, len(req.History))+i, pin)
	}
	tools := make([]provider.ToolDef, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = provider.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	preq := &provider.Request{Model: ref, Messages: msgs, Tools: tools, MaxTokens: cmp.Or(req.MaxTokens, maxTokens), Effort: effort, EffortField: control.Field, BudgetTokens: control.Budget, ThinksByDefault: control.ThinksByDefault, OffThinking: control.OffThinking, ServiceTier: req.Settings.ServiceTier,
		SessionKey: req.SessionID, WebSearch: req.WebSearch && b.nativeSearch(ref.Model),
		Temperature: req.Temperature, TopP: req.TopP, OneShot: req.Purpose != turn.PurposeTurn}
	if req.Instructions != "" {
		preq.System = []string{req.Instructions}
	}
	return preq, nil
}

// incomplete returns the retryable error of a response that did not finish, or nil.
func incomplete(ev provider.Event, acceptCut bool) error {
	switch ev.StopReason {
	case provider.StopMaxTokens:
		if !acceptCut {
			return provider.MarkStreamTruncated(errIncomplete)
		}
	case provider.StopRefusal:
		return provider.MarkRetryable(errFiltered, provider.RetryableContentFilter)
	case provider.StopIncomplete:
		return provider.MarkStreamTruncated(fmt.Errorf("incomplete response returned, reason: %s", ev.IncompleteReason))
	}
	return nil
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
