package engine

import (
	"context"
	"fmt"

	"github.com/majorcontext/harness/message"
)

// DelegatedBackend runs one turn of a delegated agent CLI end to end. What
// varies between backends is the wire protocol, session resume, and
// mid-turn steering; what does not is that every backend applies canonical
// message.Message values to the same *Session via append/emit. Claude Code
// (claudeCodeBackend) is the only implementation.
type DelegatedBackend interface {
	// SpecificationVersion mirrors the AI SDK's specificationVersion field.
	SpecificationVersion() string
	RunTurn(ctx context.Context, s *Session) (*message.Message, error)
}

// DelegatedBackendRegistry maps a provider to its backend, mirroring provider.Registry.
type DelegatedBackendRegistry map[string]DelegatedBackend

func (r DelegatedBackendRegistry) For(ref message.ModelRef) (DelegatedBackend, error) {
	b, ok := r[ref.Provider]
	if !ok {
		return nil, fmt.Errorf("engine: no delegated backend for %q (model %s)", ref.Provider, ref)
	}
	return b, nil
}

// delegatedBackends is a var so a test can substitute a fake backend.
var delegatedBackends = DelegatedBackendRegistry{
	ClaudeCodeProviderFamily: claudeCodeBackend{},
}

type claudeCodeBackend struct{}

var _ DelegatedBackend = claudeCodeBackend{}

func (claudeCodeBackend) SpecificationVersion() string { return "v1" }

func (claudeCodeBackend) RunTurn(ctx context.Context, s *Session) (*message.Message, error) {
	return s.runClaudeCodeTurn(ctx)
}
