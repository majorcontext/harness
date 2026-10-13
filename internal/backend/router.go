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
	"strings"
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
	// CodexAuthFile is the file of the Codex login, read at the first request of a provider with auth "chatgpt".
	CodexAuthFile string
	// CodexIssuer is the OAuth issuer of that login. Empty: the issuer of the Codex CLI.
	CodexIssuer string
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
		return codexauth.NewSource(codexauth.SourceOptions{Path: env.CodexAuthFile, Issuer: env.CodexIssuer, Log: env.Log})
	})
	for name, p := range cfg.Providers {
		if p.Type == config.TypeClaudeCodeCLI {
			backends[name] = claudecode.New(p, cfg.AppendSystemPrompt, workDir, cfg.MCPServers)
		} else if c := client(name, p, transport, env, codex, cfg.RequestMaxRetriesValue()); c != nil {
			backends[name] = modelapi.New(c, cfg.ContextWindowTokens, env.Log)
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
		hc = codexClient(hc, codex(), responses.ResponsesURL(p.BaseURL, p.ResponsesPath))
	}
	switch {
	case p.Type == config.TypeOpenAI, p.Type == "" && name == responses.Family:
		key, reason := env.key(cmp.Or(p.APIKeyEnv, "OPENAI_API_KEY"))
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

// codexClient wraps hc so that the Codex login reaches the Responses endpoint of the entry and no other URL.
func codexClient(hc *http.Client, src *codexauth.Source, endpoint string) *http.Client {
	out := &http.Client{}
	if hc != nil {
		out = &http.Client{Transport: hc.Transport, CheckRedirect: hc.CheckRedirect, Jar: hc.Jar, Timeout: hc.Timeout}
	}
	out.Transport = codexauth.NewTransport(src, out.Transport, endpoint)
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
