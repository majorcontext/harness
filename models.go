package harness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/claudecode"
	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	responses "github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
)

// ErrModelUnavailable reports a model that no configured provider serves.
var ErrModelUnavailable = errors.New("harness: model unavailable")

// models routes each turn to the backend of its model's provider.
type models struct {
	backends map[string]turn.Backend
	// strict refuses a model that modelmeta does not know.
	strict bool
}

func newModels(cfg config.Config, workDir string, transport func(provider string) http.RoundTripper) *models {
	m := &models{backends: map[string]turn.Backend{},
		strict: cfg.ContextWindowRequiredValue() && cfg.ContextWindowTokens == 0}
	providers := maps.Clone(cfg.Providers)
	config.EnsureProviderDefaults(providers)
	for name, p := range providers {
		if p.Type == config.TypeClaudeCodeCLI {
			m.backends[name] = claudecode.New(p, cfg.AppendSystemPrompt, workDir, cfg.MCPServers)
		} else if c := client(name, p, transport); c != nil {
			m.backends[name] = modelapi.New(c, cfg.ContextWindowTokens)
		}
	}
	return m
}

// client returns the model API client of entry name, or nil for an entry
// that names no model API. An entry keyed by a native family may leave Type
// empty. A transport may supply the credentials, so the key variable may be unset.
func client(name string, p config.Provider, transport func(provider string) http.RoundTripper) provider.Provider {
	var hc *http.Client
	if transport != nil {
		if rt := transport(name); rt != nil {
			hc = &http.Client{Transport: rt}
		}
	}
	switch {
	case p.Type == config.TypeOpenAI, p.Type == "" && name == responses.Family:
		return &responses.Client{Family: name, APIKey: os.Getenv(cmp.Or(p.APIKeyEnv, "OPENAI_API_KEY")), BaseURL: p.BaseURL,
			HTTPClient: hc, ExtraHeaders: p.ExtraHeaders, ResponsesPath: p.ResponsesPath, OmitResponseParams: p.OmitResponseParams,
			SanitizeToolSchemas: p.SanitizeToolSchemas, UseWebSocketTransport: p.UseWebSocketTransport}
	case p.Type == config.TypeOpenAICompat:
		return &openaicompat.Client{Family: cmp.Or(p.Family, name), APIKey: os.Getenv(p.APIKeyEnv), BaseURL: p.BaseURL,
			HTTPClient: hc, ExtraHeaders: p.ExtraHeaders, NoPromptCacheKey: p.NoPromptCacheKey}
	case p.Type == "" && name == anthropic.Family:
		return &anthropic.Client{APIKey: os.Getenv(cmp.Or(p.APIKeyEnv, "ANTHROPIC_API_KEY")), BaseURL: p.BaseURL,
			HTTPClient: hc, CacheTTL: p.CacheTTL, ExtraHeaders: p.ExtraHeaders}
	}
	return nil
}

// checkModel reports why model cannot start a session that allows the
// names. A backend that owns the loop runs only its built-in tools and the
// tools that no model owns.
func (r *Runtime) checkModel(model string, names []string) error {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	be, err := r.models.backend(model)
	if err != nil {
		return err
	}
	if _, ok := modelmeta.ContextWindow(ref); !ok && r.models.strict {
		return fmt.Errorf("%w: modelmeta does not know %s", ErrModelUnavailable, model)
	}
	caps := be.Capabilities(model)
	if !caps.OwnsLoop {
		return nil
	}
	for _, n := range caps.Tools {
		if r.owns(n) {
			return fmt.Errorf("%w: tool %q has the name of a built-in tool of %s", ErrInvalidRequest, n, model)
		}
	}
	for _, n := range names {
		if !r.known(model, n) {
			return fmt.Errorf("%w: %s has no tool %q", ErrInvalidRequest, model, n)
		}
	}
	return nil
}

// changeModel reports why a session that allows the names cannot move to
// model to. A backend that owns its context reads the history of another
// provider through the history tool, so any two models may follow each other.
func (r *Runtime) changeModel(_, to string, names []string) error {
	return r.checkModel(to, names)
}

func (m *models) backend(model string) (turn.Backend, error) {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModelUnavailable, err)
	}
	be, ok := m.backends[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: no provider %q is configured for %s", ErrModelUnavailable, ref.Provider, model)
	}
	return be, nil
}

func (m *models) Capabilities(model string) turn.Capabilities {
	be, err := m.backend(model)
	if err != nil {
		return turn.Capabilities{}
	}
	return be.Capabilities(model)
}

func (m *models) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	be, err := m.backend(req.Model)
	if err != nil {
		return turn.Result{}, err
	}
	return be.Run(ctx, req, out)
}

// CanWarm reports whether the backend of model has a warm-up to run.
func (m *models) CanWarm(model string) bool {
	be, err := m.backend(model)
	if err != nil {
		return false
	}
	w, ok := be.(turn.Warmer)
	if !ok {
		return false
	}
	g, gated := w.(turn.WarmGate)
	return !gated || g.CanWarm(model)
}

// Warm warms the backend of req.Model, when that backend can warm.
func (m *models) Warm(ctx context.Context, req turn.Request) error {
	be, err := m.backend(req.Model)
	if err != nil {
		return err
	}
	if w, ok := be.(turn.Warmer); ok {
		return w.Warm(ctx, req)
	}
	return nil
}

// list returns the models that modelmeta knows for each configured provider.
func (m *models) list() []protocol.Model {
	out := []protocol.Model{}
	for name, be := range m.backends {
		for _, id := range modelmeta.Models(name) {
			ref := name + "/" + id
			out = append(out, protocol.Model{ID: ref, Provider: name, ContextWindow: be.Capabilities(ref).ContextWindow})
		}
	}
	slices.SortFunc(out, func(a, b protocol.Model) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Close closes the connections of every backend. Call it when no turn runs.
func (m *models) Close() {
	for _, be := range m.backends {
		if c, ok := be.(interface{ Close() }); ok {
			c.Close()
		}
	}
}
