// Package backend routes each turn to the backend of its model's provider.
package backend

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/claudecode"
	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/provider/anthropic"
	responses "github.com/majorcontext/harness/internal/provider/openai"
	"github.com/majorcontext/harness/internal/provider/openaicompat"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// ErrUnavailable reports a model that no configured provider serves.
var ErrUnavailable = &protocol.Error{Code: protocol.CodeModelUnavailable, Message: "harness: model unavailable"}

// Router is the turn.Backend of a runtime. It routes each turn to the backend
// of its model's provider.
type Router struct {
	backends map[string]turn.Backend
}

// NewRouter returns a Router over backends, by provider name.
func NewRouter(backends map[string]turn.Backend) *Router {
	return &Router{backends: backends}
}

// Env is what the backends read from their process: the variables that hold API keys, and the log.
type Env struct {
	// Getenv reads an API-key variable. nil: no variable is read.
	Getenv func(key string) string
	// NoGetenv says why Getenv is nil, in the error of an unread key. Empty: "no Getenv is set".
	NoGetenv string
	// Log receives the log lines of the backends. nil: slog.Default().
	Log *slog.Logger
}

// New returns the Router of the providers of cfg, and of the providers that
// need no entry: the native anthropic and openai.
// The backend of a claude-code-cli provider runs in workDir.
func New(cfg config.Config, workDir string, transport func(provider string) http.RoundTripper, env Env) *Router {
	env.Log = cmp.Or(env.Log, slog.Default())
	backends := map[string]turn.Backend{}
	cfg.Providers = maps.Clone(cfg.Providers)
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	for _, name := range []string{anthropic.Family, responses.Family} {
		if _, ok := cfg.Providers[name]; !ok {
			cfg.Providers[name] = config.Provider{}
		}
	}
	codex := sync.OnceValue(func() *codexauth.Source {
		return codexauth.NewSource(codexauth.SourceOptions{Path: cfg.CodexAuthFile, Issuer: cfg.CodexIssuer, Log: env.Log})
	})
	for name, p := range cfg.Providers {
		if p.Type == config.TypeClaudeCodeCLI {
			backends[name] = claudecode.New(p, cfg.AppendSystemPrompt, workDir, cfg.MCPServers)
		} else if c := client(name, p, transport, env, codex, cfg.RequestMaxRetriesValue()); c != nil {
			be := modelapi.New(c, cfg.ContextWindowTokens, env.Log)
			if rc, ok := c.(*responses.Client); ok && p.Auth == config.AuthChatGPT {
				backends[name] = &catalogBackend{Backend: be, name: name, fetch: rc.ListModels, static: modelmeta.CodexModels(), log: env.Log, credential: func() string { return codex().AccessToken() }}
			} else {
				backends[name] = be
			}
		}
	}
	return NewRouter(backends)
}

// client returns the model API client of entry name, or nil for an entry
// that names no model API. An entry keyed by a native family may leave Type
// empty. A transport may supply the credentials, so the key variable may be unset.
func client(name string, p config.Provider, transport func(provider string) http.RoundTripper, env Env, codex func() *codexauth.Source, retries int) provider.Provider {
	var hc *http.Client
	if transport != nil {
		if rt := transport(name); rt != nil {
			hc = &http.Client{Transport: rt}
		}
	}
	if p.Auth == config.AuthChatGPT {
		hc = codexClient(hc, codex(), responses.ResponsesURL(p.BaseURL, p.ResponsesPath), responses.ModelsURL(p.BaseURL))
	}
	switch {
	case p.Type == config.TypeOpenAI, p.Type == "" && name == responses.Family:
		key, reason := "", "the entry uses the "+p.Auth+" login"
		if p.Auth == "" {
			key, reason = env.key(cmp.Or(p.APIKeyEnv, "OPENAI_API_KEY"))
		}
		return &responses.Client{Family: name, APIKey: key, NoKeyReason: reason, BaseURL: p.BaseURL, Log: env.Log,
			HTTPClient: hc, ExtraHeaders: p.ExtraHeaders, ResponsesPath: p.ResponsesPath, OmitResponseParams: p.OmitResponseParams,
			SanitizeToolSchemas: p.SanitizeToolSchemas, UseWebSocketTransport: p.UseWebSocketTransport, RequestRetries: retries}
	case p.Type == config.TypeOpenAICompat:
		key, reason := env.key(p.APIKeyEnv)
		return &openaicompat.Client{Family: cmp.Or(p.Family, name), APIKey: key, NoKeyReason: reason, BaseURL: p.BaseURL,
			HTTPClient: hc, ExtraHeaders: p.ExtraHeaders, NoPromptCacheKey: p.NoPromptCacheKey, RequestRetries: retries}
	case p.Type == "" && name == anthropic.Family:
		key, reason := env.key(cmp.Or(p.APIKeyEnv, "ANTHROPIC_API_KEY"))
		return &anthropic.Client{APIKey: key, NoKeyReason: reason, BaseURL: p.BaseURL,
			HTTPClient: hc, CacheTTL: p.CacheTTL, ExtraHeaders: p.ExtraHeaders, RequestRetries: retries}
	}
	return nil
}

// codexClient wraps hc so that the Codex login reaches the endpoints of the entry and no other URL.
// The client follows no redirect, so a 3xx answer ends the request and no header goes to its target.
func codexClient(hc *http.Client, src *codexauth.Source, endpoints ...string) *http.Client {
	out := &http.Client{}
	if hc != nil {
		out = &http.Client{Transport: hc.Transport, Jar: hc.Jar, Timeout: hc.Timeout}
	}
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	out.Transport = codexauth.NewTransport(src, out.Transport, endpoints...)
	return out
}

// key returns the API key in variable name, and why it is empty for the error of an empty key.
func (e Env) key(name string) (key, reason string) {
	switch {
	case name == "":
		return "", "the providers entry sets no api_key_env"
	case e.Getenv == nil:
		return "", cmp.Or(e.NoGetenv, "no Getenv is set") + ", so " + name + " is unread"
	}
	return e.Getenv(name), name + " is empty"
}

// Check returns the backend of the provider of ref, or reports that no
// configured provider serves it.
func (m *Router) Check(ref modelref.ModelRef) (turn.Backend, error) {
	be, ok := m.backends[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: no provider %q is configured for %s", ErrUnavailable, ref.Provider, ref)
	}
	return be, nil
}

// lookup returns the backend of the provider of model.
func (m *Router) lookup(model string) (turn.Backend, error) {
	ref, err := modelref.ParseModelRef(model)
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
func (m *Router) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	be, err := m.lookup(req.Model)
	if err != nil {
		return err
	}
	return be.Run(ctx, req, out)
}

// CanWarm reports whether the backend of model has a warm-up to run.
func (m *Router) CanWarm(model string) bool {
	be, err := m.lookup(model)
	return err == nil && turn.CanWarm(be, model)
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

// List returns the models of each configured provider, by provider name and then in the order of the provider list. A backend that
// fetches its list is asked for it; ctx bounds that wait. The other providers list what modelmeta knows.
func (m *Router) List(ctx context.Context) []protocol.Model {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	names := make([]string, 0, len(m.backends))
	for name := range m.backends {
		names = append(names, name)
	}
	slices.Sort(names)
	out := []protocol.Model{}
	for _, name := range names {
		be := m.backends[name]
		models := modelmeta.Models(name)
		if lister, ok := be.(interface {
			Models(ctx context.Context) []modelmeta.Model
		}); ok {
			models = lister.Models(ctx)
		}
		for _, mm := range models {
			ref := name + "/" + mm.ID
			out = append(out, protocol.Model{ID: ref, Provider: name, ProviderName: modelmeta.ProviderName(name), Name: mm.Name, Description: mm.Description, Efforts: mm.Efforts, ContextWindow: be.Capabilities(ref).ContextWindow})
		}
	}
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
