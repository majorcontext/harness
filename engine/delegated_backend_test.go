package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// stubDelegatedBackend is a minimal DelegatedBackend for registry and
// dispatch tests — it never spawns a process.
type stubDelegatedBackend struct {
	name string
}

func (b *stubDelegatedBackend) SpecificationVersion() string { return "v1" }

func (b *stubDelegatedBackend) RunTurn(context.Context, *Session) (*message.Message, error) {
	return nil, nil
}

func TestDelegatedBackendRegistryFor_KnownProvider(t *testing.T) {
	backend := &stubDelegatedBackend{name: "claude-code"}
	reg := DelegatedBackendRegistry{ClaudeCodeProviderFamily: backend}

	got, err := reg.For(message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"})
	if err != nil {
		t.Fatalf("For returned error for a registered provider: %v", err)
	}
	if got != backend {
		t.Fatalf("For returned %v, want the registered stub", got)
	}
}

func TestDelegatedBackendRegistryFor_UnknownProvider(t *testing.T) {
	reg := DelegatedBackendRegistry{ClaudeCodeProviderFamily: &stubDelegatedBackend{}}

	ref := message.ModelRef{Provider: "codex", Model: "some-model"}
	got, err := reg.For(ref)
	if err == nil {
		t.Fatalf("For returned nil error for an unregistered provider %q", ref.Provider)
	}
	if got != nil {
		t.Fatalf("For returned a non-nil backend %v alongside an error", got)
	}
	if !strings.Contains(err.Error(), "codex") {
		t.Errorf("error %q does not name the unregistered provider", err.Error())
	}
}

func TestDelegatedBackendRegistryFor_EmptyRegistry(t *testing.T) {
	var reg DelegatedBackendRegistry // nil map, the zero value

	_, err := reg.For(message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"})
	if err == nil {
		t.Fatal("For on a nil DelegatedBackendRegistry should error, not panic or succeed")
	}
}

// recordingDelegatedBackend proves the engine dispatches a delegated turn
// through the DelegatedBackend interface, not by calling
// Session.runClaudeCodeTurn directly: it never spawns `claude` and reports
// how many times, and with which session, it was invoked.
type recordingDelegatedBackend struct {
	calls  int
	lastS  *Session
	result *message.Message
}

func (b *recordingDelegatedBackend) SpecificationVersion() string { return "v1" }

func (b *recordingDelegatedBackend) RunTurn(_ context.Context, s *Session) (*message.Message, error) {
	b.calls++
	b.lastS = s
	return b.result, nil
}

func TestRunDelegatedTurnDispatchesThroughRegistry(t *testing.T) {
	backend := &recordingDelegatedBackend{
		result: &message.Message{ID: "msg_stub", Role: message.RoleAssistant},
	}
	orig := delegatedBackends[ClaudeCodeProviderFamily]
	delegatedBackends[ClaudeCodeProviderFamily] = backend
	t.Cleanup(func() { delegatedBackends[ClaudeCodeProviderFamily] = orig })

	s := NewSession(Config{
		SessionDir: t.TempDir(),
		Model:      message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
	})

	got, err := s.Prompt(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if backend.calls != 1 {
		t.Fatalf("recordingDelegatedBackend.calls = %d, want 1", backend.calls)
	}
	if backend.lastS != s {
		t.Fatal("RunTurn did not receive the prompting session")
	}
	if got != backend.result {
		t.Fatalf("Prompt returned %v, want the backend's own result %v", got, backend.result)
	}
}
