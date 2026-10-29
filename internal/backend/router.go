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
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/modelapi"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/injectedauth"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/provider"
	"github.com/majorcontext/harness/internal/provider/anthropic"
	responses "github.com/majorcontext/harness/internal/provider/openai"
	"github.com/majorcontext/harness/internal/provider/openaicompat"
	"github.com/majorcontext/harness/internal/tracing"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// ErrUnavailable reports a model that no configured provider serves.
var ErrUnavailable = &protocol.Error{Code: protocol.CodeModelUnavailable, Message: "harness: model unavailable"}

// Router is the turn.Backend of a runtime. It routes each turn to the backend
// of its model's provider, and owns the live set of provider entries.
type Router struct {
	build *builder
	snap  atomic.Pointer[snapshot]
	// setMu serializes Set, Reconnect, and Close.
	setMu   sync.Mutex
	closed  bool
	retired sync.WaitGroup
}

// NewRouter returns a Router over backends, by provider name. It has no entry config, so Set fails.
func NewRouter(backends map[string]turn.Backend) *Router {
	s := newSnapshot()
	for name, be := range backends {
		s.entries[name] = newLive(name, config.Provider{}, "", be)
		s.names = append(s.names, name)
	}
	r := &Router{}
	r.setProviderSet(s)
	return r
}

// Env is what the backends read from their process: the variables that hold API keys, and the log.
type Env struct {
	// Getenv reads an API-key variable. nil: no variable is read.
	Getenv func(key string) string
	// NoGetenv says why Getenv is nil, in the error of an unread key. Empty: "no Getenv is set".
	NoGetenv string
	// Log receives the log lines of the backends. nil: slog.Default().
	Log *slog.Logger
	// Logins owns the login state of the injected provider entries, and is shared with the ACP logins of the connection. nil: the backend makes one over its own model transport.
	Logins *injectedauth.Logins
	// NoProcess says that the host starts no process, so a provider that runs a child process has no backend.
	NoProcess bool
	// NoLogin says that the host has no file login: a provider that uses the ChatGPT login is not listed, reads no login file, and fails a request before it is sent.
	NoLogin bool
}

// New returns the Router of the providers of cfg, and of the providers that
// need no entry: codex, claude-code, and anthropic and openai when credentialed.
// The backend of a claude-code-cli provider runs in workDir.
func New(cfg config.Config, workDir string, transport func(provider string) http.RoundTripper, env Env) *Router {
	env.Log = cmp.Or(env.Log, slog.Default())
	if env.Logins == nil {
		env.Logins = injectedauth.New(injectedauth.Options{Transport: transport, Probe: ProbeOf})
	}
	cfg.Providers = cloneProviders(cfg.Providers)
	b := &builder{cfg: cfg, workDir: workDir, transport: transport, env: env,
		codex: sync.OnceValue(func() *codexauth.Source {
			return codexauth.NewSource(codexauth.SourceOptions{Path: cfg.CodexAuthFile, Issuer: cfg.CodexIssuer, Log: env.Log})
		})}
	r := &Router{build: b}
	r.setProviderSet(b.snapshot(&cfg, newSnapshot(), nil))
	return r
}

// modelBackend returns the backend of client c of entry name. An entry that reads its model list from the ChatGPT backend lists it through c.
func modelBackend(name string, p config.Provider, c provider.Provider, contextWindow int, env Env, codex func() *codexauth.Source) turn.Backend {
	be := modelapi.New(c, contextWindow, env.Log)
	rc, ok := c.(*responses.Client)
	if !ok || p.ModelsFrom != config.ModelsFromChatGPT {
		return be
	}
	cb := &catalogBackend{Backend: be, name: name, fetch: rc.ListModels, static: modelmeta.CodexModels(), log: env.Log, credential: func() string { return "" }}
	cb.base, cb.cancel = context.WithCancel(context.Background())
	switch e := env.Logins.Of(name, p); {
	case p.Auth == config.AuthChatGPT:
		cb.credential = func() string { return codex().AccessToken() }
	case e != nil:
		cb.credential, cb.refusedRetry = func() string { return strconv.FormatUint(e.Generation(), 10) }, catalogRetry
	}
	return cb
}

// client returns the model API client of entry name, or nil for an entry
// that names no model API. An entry keyed by a native family may leave Type
// empty. A transport that supplies native API credentials declares auth injected.
func client(name string, p config.Provider, transport func(provider string) http.RoundTripper, env Env, codex func() *codexauth.Source, retries int, rev func() uint64) provider.Provider {
	var hc *http.Client
	if transport != nil {
		if rt := transport(name); rt != nil {
			hc = &http.Client{Transport: rt}
		}
	}
	if p.Auth == config.AuthChatGPT && env.NoLogin {
		hc = &http.Client{Transport: noLoginTransport{name: name}}
	} else if p.Auth == config.AuthChatGPT {
		hc = codexClient(hc, codex(), responses.ResponsesURL(p.BaseURL, p.ResponsesPath), responses.ModelsURL(p.BaseURL))
	} else if e := env.Logins.Of(name, p); e != nil {
		hc = e.Client(hc)
	}
	traced := func(hasCredential bool) *http.Client {
		if hc == nil && !hasCredential {
			return nil
		}
		return tracing.Client(hc)
	}
	var cacheTTL time.Duration
	if p.UsesChatGPTLogin() || p.ModelsFrom == config.ModelsFromChatGPT {
		cacheTTL = responses.ChatGPTCacheTTL
	}
	switch {
	case p.Type == config.TypeOpenAI, p.Type == "" && name == responses.Family:
		key, reason := "", "the entry uses the "+p.Auth+" login"
		if p.Auth == "" {
			key, reason = env.key(p.KeyEnv(name))
		}
		return &responses.Client{Family: name, APIKey: key, NoKeyReason: reason, NoKey: p.Keyless() || p.HasCredentialHeader(), BaseURL: p.BaseURL, Log: env.Log,
			HTTPClient: traced(key != "" || p.Keyless() || p.HasCredentialHeader()), ExtraHeaders: p.ExtraHeaders, ResponsesPath: p.ResponsesPath, OmitResponseParams: p.OmitResponseParams,
			SanitizeToolSchemas: p.SanitizeToolSchemas, UseWebSocketTransport: p.UseWebSocketTransport, HashCacheKey: p.UsesInjectedAuth(), SessionHeaders: p.UsesChatGPTLogin() || p.UsesInjectedAuth() && p.ModelsFrom == config.ModelsFromChatGPT, CacheTTL: cacheTTL, LiteUntilListed: p.ModelsFrom == config.ModelsFromChatGPT, RequestRetries: retries, Revision: rev}
	case p.Type == config.TypeOpenAICompat:
		key, reason := env.key(p.APIKeyEnv)
		return &openaicompat.Client{Family: cmp.Or(p.Family, name), APIKey: key, NoKeyReason: reason, NoKey: p.Keyless() || p.HasCredentialHeader(), BaseURL: p.BaseURL,
			HTTPClient: traced(key != "" || p.Keyless() || p.HasCredentialHeader()), ExtraHeaders: p.ExtraHeaders, NoPromptCacheKey: p.NoPromptCacheKey, RequestRetries: retries}
	case p.Type == "" && name == anthropic.Family:
		key, reason := "", "the entry uses the "+p.Auth+" login"
		if p.Auth == "" {
			key, reason = env.key(p.KeyEnv(name))
		}
		return &anthropic.Client{APIKey: key, NoKeyReason: reason, NoKey: p.HasCredentialHeader(), BaseURL: p.BaseURL,
			HTTPClient: traced(key != "" || p.HasCredentialHeader()), CacheTTL: p.CacheTTL, ExtraHeaders: p.ExtraHeaders, RequestRetries: retries}
	}
	return nil
}

// noLoginTransport fails every request before it is sent, because the host has no file login.
type noLoginTransport struct{ name string }

func (t noLoginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	return nil, &injectedauth.LoginRequiredError{Text: "not logged in to " + t.name + "; this host has no ChatGPT login, so set auth to injected on the provider entry"}
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
	e, err := m.snap.Load().find(ref)
	if err != nil {
		return nil, err
	}
	return e.be, nil
}

// lease returns the entry of the provider of model, held until its release.
func (m *Router) lease(ctx context.Context, model string) (*live, error) {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, context.Cause(ctx)
		}
		e, err := m.snap.Load().find(ref)
		if err != nil {
			return nil, err
		}
		if e.acquire() {
			e.checkGeneration()
			return e, nil
		}
	}
}

// lookup returns the backend of the provider of model.
func (m *Router) lookup(model string) (turn.Backend, error) {
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return m.Check(ref)
}

// Providers returns the names of the configured providers that have a backend, sorted.
func (m *Router) Providers() []string { return slices.Sorted(maps.Keys(m.snap.Load().entries)) }

// Unavailable returns why the configured provider name has no backend, or "".
func (m *Router) Unavailable(name string) string { return m.snap.Load().unavailable[name] }

// Vendor returns the vendor of the entry name, or "".
func (m *Router) Vendor(name string) string { return m.snap.Load().vendors[name] }

// Capabilities returns the capabilities of the backend of model, or none for a model with no backend.
func (m *Router) Capabilities(model string) turn.Capabilities {
	s := m.snap.Load()
	ref, err := modelref.ParseModelRef(model)
	if err != nil {
		return turn.Capabilities{}
	}
	e, err := s.find(ref)
	if err != nil {
		return turn.Capabilities{}
	}
	caps := e.be.Capabilities(model)
	caps.Provider = s.traceNames[ref.Provider]
	return caps
}

// Run runs the turn on the backend of req.Model.
func (m *Router) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	e, err := m.lease(ctx, req.Model)
	if err != nil {
		return err
	}
	defer e.release()
	return e.be.Run(ctx, req, out)
}

// CanWarm reports whether the backend of model has a warm-up to run.
func (m *Router) CanWarm(model string) bool {
	be, err := m.lookup(model)
	return err == nil && turn.CanWarm(be, model)
}

// Warm warms the backend of req.Model, when that backend can warm.
func (m *Router) Warm(ctx context.Context, req turn.Request) error {
	e, err := m.lease(ctx, req.Model)
	if err != nil {
		return err
	}
	defer e.release()
	if w, ok := e.be.(turn.Warmer); ok {
		return w.Warm(ctx, req)
	}
	return nil
}

// List returns the models of each configured provider, by provider name and then in the order of the provider list. A backend that
// fetches its list is asked for it; ctx bounds that wait. The other providers list what modelmeta knows.
func (m *Router) List(ctx context.Context) []protocol.Model {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	s := m.snap.Load()
	out := []protocol.Model{}
	for _, name := range slices.Sorted(maps.Keys(s.entries)) {
		e := s.entries[name]
		if e.hidden || !e.acquire() {
			continue
		}
		models := e.listed
		if !e.hasListed {
			models = modelmeta.Models(name)
			if lister, ok := e.be.(interface {
				Models(ctx context.Context) []modelmeta.Model
			}); ok {
				models = lister.Models(ctx)
			}
		}
		for _, mm := range models {
			ref := name + "/" + mm.ID
			out = append(out, protocol.Model{ID: ref, Provider: name, ProviderName: modelmeta.ProviderName(name), Name: mm.Name, Description: mm.Description, Efforts: mm.Efforts, ContextWindow: e.be.Capabilities(ref).ContextWindow})
		}
		e.release()
	}
	return out
}
