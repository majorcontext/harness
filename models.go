package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/claudecode"
	"github.com/majorcontext/harness/internal/backend/openai"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/modelmeta"
	responses "github.com/majorcontext/harness/provider/openai"
)

// ErrModelUnavailable reports a model that no configured provider serves.
var ErrModelUnavailable = errors.New("harness: model unavailable")

// models routes each turn to the backend of its model's provider.
type models struct {
	backends map[string]turn.Backend
	// strict refuses a model that modelmeta does not know.
	strict bool
}

func newModels(cfg config.Config, transport func(provider string) http.RoundTripper) *models {
	m := &models{backends: map[string]turn.Backend{},
		strict: cfg.ContextWindowRequiredValue() && cfg.ContextWindowTokens == 0}
	for name, p := range cfg.Providers {
		if p.Type == config.TypeClaudeCodeCLI {
			m.backends[name] = claudecode.New(p)
			continue
		}
		// The entry keyed by the native family may leave Type empty.
		if p.Type != config.TypeOpenAI && (p.Type != "" || name != responses.Family) {
			continue
		}
		var rt http.RoundTripper
		if transport != nil {
			rt = transport(name)
		}
		m.backends[name] = openai.New(name, p, rt)
	}
	return m
}

// check reports why model cannot start a session that allows the names. A
// backend that owns the loop runs only its built-in tools and the embedder tools.
func (m *models) check(model string, names []string, tools []turn.Tool) error {
	ref, err := message.ParseModelRef(model)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	be, err := m.backend(model)
	if err != nil {
		return err
	}
	if _, ok := modelmeta.ContextWindow(ref); !ok && m.strict {
		return fmt.Errorf("%w: modelmeta does not know %s", ErrModelUnavailable, model)
	}
	if caps := be.Capabilities(model); caps.OwnsLoop {
		for _, n := range names {
			if !slices.Contains(caps.Tools, n) && !slices.ContainsFunc(tools, func(t turn.Tool) bool { return t.Spec().Name == n }) {
				return fmt.Errorf("%w: %s has no tool %q", ErrInvalidRequest, model, n)
			}
		}
	}
	return nil
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

// Close closes the connections of every backend. Call it when no turn runs.
func (m *models) Close() {
	for _, be := range m.backends {
		if c, ok := be.(interface{ Close() }); ok {
			c.Close()
		}
	}
}
