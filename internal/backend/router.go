// Package backend routes each turn to the backend of its model's provider.
package backend

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
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/provider/anthropic"
	responses "github.com/majorcontext/harness/internal/provider/openai"
	"github.com/majorcontext/harness/internal/provider/openaicompat"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const defaultOpenRouter = "openrouter"

// ErrUnavailable reports a model that no configured provider serves.
var ErrUnavailable = errors.New("harness: model unavailable")

// Router is the turn.Backend of a runtime. It routes each turn to the backend
// of its model's provider.
type Router struct {
	backends map[string]turn.Backend
}

// NewRouter returns a Router over backends, by provider name.
func NewRouter(backends map[string]turn.Backend) *Router {
	return &Router{backends: backends}
}

// New returns the Router of the providers of cfg, and of the providers that
// need no entry: the native anthropic and openai, and the default openrouter.
// The backend of a claude-code-cli provider runs in workDir.
func New(cfg config.Config, workDir string, transport func(provider string) http.RoundTripper) *Router {
	backends := map[string]turn.Backend{}
	providers := maps.Clone(cfg.Providers)
	if providers == nil {
		providers = map[string]config.Provider{}
	}
	for _, name := range []string{anthropic.Family, responses.Family, defaultOpenRouter} {
		if _, ok := providers[name]; !ok {
			providers[name] = config.Provider{}
		}
	}
	config.EnsureProviderDefaults(providers)
	for name, p := range providers {
		if p.Type == config.TypeClaudeCodeCLI {
			backends[name] = claudecode.New(p, cfg.AppendSystemPrompt, workDir, cfg.MCPServers)
		} else if c := client(name, p, transport); c != nil {
			backends[name] = modelapi.New(c, cfg.ContextWindowTokens)
		}
	}
	return NewRouter(backends)
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

// Check returns the backend of the provider of ref, or reports that no
// configured provider serves it.
func (m *Router) Check(ref message.ModelRef) (turn.Backend, error) {
	be, ok := m.backends[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: no provider %q is configured for %s", ErrUnavailable, ref.Provider, ref)
	}
	return be, nil
}

// lookup returns the backend of the provider of model.
func (m *Router) lookup(model string) (turn.Backend, error) {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return m.Check(ref)
}

// Providers returns the names of the configured providers, sorted.
func (m *Router) Providers() []string { return slices.Sorted(maps.Keys(m.backends)) }

// Capabilities returns the capabilities of the backend of model, or none for a model with no backend.
func (m *Router) Capabilities(model string) turn.Capabilities {
	be, err := m.lookup(model)
	if err != nil {
		return turn.Capabilities{}
	}
	return be.Capabilities(model)
}

// Run runs the turn on the backend of req.Model.
func (m *Router) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	be, err := m.lookup(req.Model)
	if err != nil {
		return turn.Result{}, err
	}
	return be.Run(ctx, req, out)
}

// CanWarm reports whether the backend of model has a warm-up to run.
func (m *Router) CanWarm(model string) bool {
	be, err := m.lookup(model)
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
func (m *Router) Warm(ctx context.Context, req turn.Request) error {
	be, err := m.lookup(req.Model)
	if err != nil {
		return err
	}
	if w, ok := be.(turn.Warmer); ok {
		return w.Warm(ctx, req)
	}
	return nil
}

// List returns the models that modelmeta knows for each configured provider.
func (m *Router) List() []protocol.Model {
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
func (m *Router) Close() {
	for _, be := range m.backends {
		if c, ok := be.(interface{ Close() }); ok {
			c.Close()
		}
	}
}
