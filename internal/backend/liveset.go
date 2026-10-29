package backend

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/claudecode"
	"github.com/majorcontext/harness/internal/codexauth"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	responses "github.com/majorcontext/harness/internal/provider/openai"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

// snapshot is the immutable live set that a reader takes once for each decision.
type snapshot struct {
	entries     map[string]*live
	unavailable map[string]string
	vendors     map[string]string
	traceNames  map[string]string
	// names holds every configured name, available or not, sorted.
	names  []string
	closed bool
}

func newSnapshot() *snapshot {
	return &snapshot{entries: map[string]*live{}, unavailable: map[string]string{}, vendors: map[string]string{}, traceNames: map[string]string{}}
}

// find returns the entry that serves ref, or the model_unavailable error.
func (s *snapshot) find(ref modelref.ModelRef) (*live, error) {
	if s.closed {
		return nil, ErrClosed
	}
	if e, ok := s.entries[ref.Provider]; ok {
		return e, nil
	}
	if reason := s.unavailable[ref.Provider]; reason != "" {
		return nil, fmt.Errorf("%w: provider %q is unavailable for %s: %s", ErrUnavailable, ref.Provider, ref, reason)
	}
	return nil, fmt.Errorf("%w: no provider %q is configured for %s", ErrUnavailable, ref.Provider, ref)
}

// live is one provider entry with a backend.
type live struct {
	name string
	cfg  config.Provider
	// cred is the API key that the entry reads, or "".
	cred string
	be   turn.Backend
	// listed holds the models that the entry names.
	listed    []modelmeta.Model
	hasListed bool
	hidden    bool
	// rev is the revision of the credentials of the entry. A pooled websocket keeps the revision that it was dialed under.
	rev atomic.Uint64
	// generation returns the login generation of an injected entry. nil: none.
	generation func() uint64
	lastGen    atomic.Uint64

	mu      sync.Mutex
	active  int
	retired bool
	drained chan struct{}
}

func newLive(name string, p config.Provider, cred string, be turn.Backend) *live {
	return &live{name: name, cfg: p, cred: cred, be: be, drained: make(chan struct{})}
}

// acquire holds the entry for one call. It is false for an entry that a swap retired.
func (e *live) acquire() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired {
		return false
	}
	e.active++
	return true
}

func (e *live) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active--
	if e.retired && e.active == 0 {
		close(e.drained)
	}
}

// checkGeneration moves the revision when the login generation of the entry moved since the last call.
func (e *live) checkGeneration() {
	if e.generation == nil {
		return
	}
	if g := e.generation(); e.lastGen.Swap(g) != g {
		e.bump()
	}
}

// bump moves the revision and closes the idle sockets of an older revision. A busy socket closes when its call ends.
func (e *live) bump() {
	e.rev.Add(1)
	if c, ok := e.be.(interface{ CloseStale() }); ok {
		c.CloseStale()
	}
}

// builder makes the backend of one entry. It holds what every entry of a runtime shares.
type builder struct {
	cfg       config.Config
	workDir   string
	transport func(provider string) http.RoundTripper
	env       Env
	codex     func() *codexauth.Source
}

// credential returns the API key that the entry reads now, or "".
func (b *builder) credential(name string, p config.Provider) string {
	var key string
	switch {
	case p.Type == config.TypeOpenAICompat:
		key = p.APIKeyEnv
	case p.Auth == "":
		key = p.KeyEnv(name)
	}
	if key == "" || b.env.Getenv == nil {
		return ""
	}
	return b.env.Getenv(key)
}

// entry builds the backend of the entry name. It makes no network call and starts no process. For an entry with no backend it returns nil and the reason.
func (b *builder) entry(name string, p config.Provider, cred string) (*live, string) {
	e := newLive(name, p, cred, nil)
	rev := e.rev.Load
	var be turn.Backend
	if p.Type == config.TypeClaudeCodeCLI {
		if b.env.NoProcess {
			return nil, "this host starts no process, so a claude-code-cli entry has no backend"
		}
		cb := claudecode.New(p, b.cfg.AppendSystemPrompt, b.workDir, b.cfg.MCPToolLoading)
		cb.RouteAccounts(b.cfg.AccountRouting, b.env.Getenv)
		be = cb
	} else {
		retries := b.cfg.RequestMaxRetriesValue()
		c := client(name, p, b.transport, b.env, b.codex, retries, rev)
		if c == nil {
			return nil, "the entry names no model API"
		}
		be = modelBackend(name, p, c, b.cfg.ContextWindowTokens, b.env, b.codex)
		if p.Vendor != "" {
			be = &accountBackend{base: be, name: name, vendor: p.Vendor, routing: b.cfg.AccountRouting, getenv: b.env.Getenv,
				transport: func() http.RoundTripper {
					if b.transport == nil {
						return nil
					}
					return b.transport(name)
				},
				build: func(tr func(string) http.RoundTripper) turn.Backend {
					accountClient := client(name, p, tr, b.env, b.codex, retries, rev)
					if base, ok := c.(*responses.Client); ok {
						if account, ok := accountClient.(*responses.Client); ok {
							account.ShareLiveCatalog(base)
						}
					}
					return modelBackend(name, p, accountClient, b.cfg.ContextWindowTokens, b.env, b.codex)
				}}
		}
		e.hidden = b.env.NoLogin && p.Auth == config.AuthChatGPT
		if lg := b.env.Logins.Of(name, p); lg != nil {
			e.generation = lg.Generation
			e.lastGen.Store(lg.Generation())
		}
	}
	e.be = be
	if len(p.Models) > 0 {
		e.listed, e.hasListed = modelmeta.Named(p.Models, modelmeta.Models(name)), true
	}
	return e, ""
}

// snapshot reads the candidate entries of cfg. An entry that old holds with the same config and credential keeps its backend.
func (b *builder) snapshot(cfg *config.Config, s *snapshot, old *snapshot) *snapshot {
	for name, p := range cfg.ProviderEntries() {
		s.names = append(s.names, name)
		s.vendors[name] = p.Vendor
		s.traceNames[name] = config.TraceName(name, p)
		if config.NativeKeyEnv(name) != "" && !p.HasCredentials(name, b.env.Getenv) {
			_, s.unavailable[name] = b.env.key(p.KeyEnv(name))
			continue
		}
		b.env.Logins.Publish(name, p)
		cred := b.credential(name, p)
		if old != nil {
			if o := old.entries[name]; o != nil && o.cred == cred && reflect.DeepEqual(o.cfg, p) {
				s.entries[name] = o
				continue
			}
		}
		if e, reason := b.entry(name, p, cred); e != nil {
			s.entries[name] = e
		} else {
			s.unavailable[name] = reason
		}
	}
	slices.Sort(s.names)
	return s
}

// ErrClosed reports a call on a router that Close has closed.
var ErrClosed = errors.New("backend: the router is closed")

// Set makes providers the live set of configured entries. It validates cfg with a copy of providers, then builds a backend for each
// new or changed entry. An entry whose config and credential are the same keeps its backend. Every other old entry retires: it takes
// no new call, and it closes when its last call ends. A Validate error changes nothing. It returns the status of the new set.
func (m *Router) Set(providers map[string]config.Provider) ([]protocol.ProviderStatus, error) {
	m.setMu.Lock()
	defer m.setMu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if m.build == nil {
		return nil, errors.New("backend: the router has no entry config")
	}
	cfg := m.build.cfg
	cfg.Providers = cloneProviders(providers)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	old := m.snap.Load()
	next := m.build.snapshot(&cfg, newSnapshot(), old)
	m.setProviderSet(next)
	for name, e := range old.entries {
		if next.entries[name] != e {
			m.retire(e)
		}
	}
	return next.status(), nil
}

// cloneProviders returns a copy of providers that shares no map or slice with it. nil stays nil.
func cloneProviders(providers map[string]config.Provider) map[string]config.Provider {
	if providers == nil {
		return nil
	}
	out := make(map[string]config.Provider, len(providers))
	for name, p := range providers {
		out[name] = p.Clone()
	}
	return out
}

// setProviderSet publishes next as the live set of provider entries.
func (m *Router) setProviderSet(next *snapshot) { m.snap.Store(next) }

// Reconnect moves the revision of the entry name, so that the next connection of its websocket pool authenticates again.
func (m *Router) Reconnect(name string) error {
	m.setMu.Lock()
	defer m.setMu.Unlock()
	if m.closed {
		return ErrClosed
	}
	e := m.snap.Load().entries[name]
	if e == nil {
		return fmt.Errorf("provider %q has no backend", name)
	}
	e.bump()
	return nil
}

// status returns the configured names in name order with whether each has a backend.
func (s *snapshot) status() []protocol.ProviderStatus {
	out := make([]protocol.ProviderStatus, 0, len(s.names))
	for _, name := range s.names {
		_, ok := s.entries[name]
		out = append(out, protocol.ProviderStatus{Name: name, Available: ok, Reason: s.unavailable[name]})
	}
	return out
}

// retire closes e after its last call ends.
func (m *Router) retire(e *live) {
	e.mu.Lock()
	e.retired = true
	if e.active == 0 {
		close(e.drained)
	}
	e.mu.Unlock()
	m.retired.Add(1)
	go func() {
		defer m.retired.Done()
		<-e.drained
		if c, ok := e.be.(interface{ Close() }); ok {
			c.Close()
		}
	}()
}

// Close retires every entry and waits until each has closed. Call it when no turn runs.
func (m *Router) Close() {
	m.setMu.Lock()
	if !m.closed {
		m.closed = true
		old := m.snap.Load()
		m.setProviderSet(&snapshot{closed: true, entries: map[string]*live{}, unavailable: map[string]string{}, vendors: map[string]string{}, traceNames: map[string]string{}})
		for _, e := range old.entries {
			m.retire(e)
		}
	}
	m.setMu.Unlock()
	m.retired.Wait()
}
