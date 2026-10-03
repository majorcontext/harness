// Package openai runs turns on the OpenAI Responses API, the ChatGPT Codex
// lane included. It makes one model call per Run; the turn loop runs tools.
package openai

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	"github.com/majorcontext/harness/provider"
	responses "github.com/majorcontext/harness/provider/openai"
)

// Backend is a turn.Backend over one configured Responses provider.
type Backend struct {
	client *responses.Client
	// window overrides the modelmeta context window when positive.
	window int
}

// New returns the Backend of provider name. A nil rt uses the default
// transport for HTTP requests and the websocket dial alike. A non-nil rt may
// supply the credentials, so the key variable may be unset. A positive
// window replaces the modelmeta context window of every model.
func New(name string, p config.Provider, rt http.RoundTripper, window int) *Backend {
	c := &responses.Client{
		Family:                name,
		APIKey:                os.Getenv(cmp.Or(p.APIKeyEnv, "OPENAI_API_KEY")),
		BaseURL:               p.BaseURL,
		ExtraHeaders:          p.ExtraHeaders,
		ResponsesPath:         p.ResponsesPath,
		OmitResponseParams:    p.OmitResponseParams,
		SanitizeToolSchemas:   p.SanitizeToolSchemas,
		UseWebSocketTransport: p.UseWebSocketTransport,
	}
	if rt != nil {
		c.HTTPClient = &http.Client{Transport: rt}
	}
	return &Backend{client: c, window: window}
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
	for {
		// A truncated stream wraps io.EOF, so only the bare sentinel is a clean end.
		ev, err := st.Next()
		if err == io.EOF {
			return turn.Result{}, nil
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
			m := fromMessage(ev.Message)
			if !hasOutput(m) {
				return turn.Result{}, fmt.Errorf("%w: openai: the response has no output", turn.ErrRetryable)
			}
			if err := out.Item(m); err != nil {
				return turn.Result{}, err
			}
		}
	}
}

// telemetry reports u, and the prompt of the call as the context reading.
// A call with no prompt tokens reports no reading, so the earlier reading stays.
func (b *Backend) telemetry(model string, u provider.Usage) turn.Telemetry {
	usage := eventlog.Usage{InputTokens: int64(u.InputTokens), OutputTokens: int64(u.OutputTokens),
		CacheReadTokens: int64(u.CacheReadTokens), CacheWriteTokens: int64(u.CacheWriteTokens)}
	t := turn.Telemetry{Usage: usage}
	if tokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens; tokens > 0 {
		t.Context = eventlog.ContextMeasured{Source: b.client.Family, Tokens: tokens, Window: int64(b.Capabilities(model).ContextWindow)}
	}
	return t
}

// Close closes the pooled websocket connections. Call it when no Run is
// active. A later Run dials again.
func (b *Backend) Close() { b.client.Close() }

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
	preq := &provider.Request{Model: ref, Messages: msgs, Tools: tools, Effort: effort, ServiceTier: req.Settings.ServiceTier,
		SessionKey: req.SessionID}
	if req.Instructions != "" {
		preq.System = []string{req.Instructions}
	}
	return preq, nil
}

func classify(err error) error {
	if _, ok := provider.AsRetryable(err); ok {
		return fmt.Errorf("%w: %w", turn.ErrRetryable, err)
	}
	return err
}
