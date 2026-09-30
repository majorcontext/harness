package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// stubDelegatedBackend is a minimal DelegatedBackend for registry and
// dispatch tests — it never spawns a process.
type stubDelegatedBackend struct {
	name string
}

func (b *stubDelegatedBackend) SpecificationVersion() string { return "v1" }

func (b *stubDelegatedBackend) RunTurn(context.Context, *Session, message.ModelRef) (*message.Message, error) {
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

func (b *recordingDelegatedBackend) RunTurn(_ context.Context, s *Session, _ message.ModelRef) (*message.Message, error) {
	b.calls++
	b.lastS = s
	return b.result, nil
}

type modelSwitchDelegatedBackend struct {
	started chan struct{}
	release chan struct{}
	model   message.ModelRef
	proxy   string
}

func (b *modelSwitchDelegatedBackend) SpecificationVersion() string { return "v1" }

func (b *modelSwitchDelegatedBackend) RunTurn(ctx context.Context, s *Session, model message.ModelRef) (*message.Message, error) {
	close(b.started)
	<-b.release
	b.model = model
	routeCtx, err := s.accountRoutingContext(ctx, model.Provider)
	if err != nil {
		return nil, err
	}
	if route, ok := provider.AccountRoutingFromContext(routeCtx); ok {
		b.proxy = route.ProxyURL
	}
	msg := &message.Message{ID: "msg_delegated", Role: message.RoleAssistant, Parts: message.Parts{&message.Text{Text: "claude done"}}}
	s.append(*msg)
	return msg, nil
}

type modelSwitchNativeProvider struct {
	calls int
	proxy string
}

func (*modelSwitchNativeProvider) Name() string { return "codex" }

func (p *modelSwitchNativeProvider) Stream(ctx context.Context, _ *provider.Request) (provider.Stream, error) {
	p.calls++
	if route, ok := provider.AccountRoutingFromContext(ctx); ok {
		p.proxy = route.ProxyURL
	}
	return &scriptedStream{events: doneTurn("codex done")[0]}, nil
}

func TestDelegatedDispatchKeepsCommittedModelAcrossConcurrentSwitch(t *testing.T) {
	backend := &modelSwitchDelegatedBackend{started: make(chan struct{}), release: make(chan struct{})}
	original := delegatedBackends[ClaudeCodeProviderFamily]
	delegatedBackends[ClaudeCodeProviderFamily] = backend
	t.Cleanup(func() { delegatedBackends[ClaudeCodeProviderFamily] = original })
	codex := &modelSwitchNativeProvider{}
	claudeProxy := "http://subject%7Cbox:claude-password@claude-proxy.example"
	codexProxy := "http://subject%7Cbox:codex-password@codex-proxy.example"
	selection := map[string]string{"claude": "acct_claude", "codex": "acct_codex"}
	s := NewSession(Config{
		Providers: provider.Registry{"codex": codex},
		Model:     message.ModelRef{Provider: ClaudeCodeProviderFamily, Model: "sonnet"},
		AccountRouting: map[string]AccountRoutingConfig{
			ClaudeCodeProviderFamily: {Vendor: "claude", Protocol: "boxes-v1", ProxyURL: claudeProxy},
			"codex":                  {Vendor: "codex", Protocol: "boxes-v1", ProxyURL: codexProxy},
		},
	})
	s.accountSelection = selection
	claudeModel := s.Model()
	wantClaudeProxy, err := accountProxyURL(claudeProxy, selection)
	if err != nil {
		t.Fatal(err)
	}
	wantCodexProxy, err := accountProxyURL(codexProxy, selection)
	if err != nil {
		t.Fatal(err)
	}

	promptDone := make(chan error, 1)
	go func() {
		_, err := s.Prompt(context.Background(), "claude request")
		promptDone <- err
	}()
	<-backend.started
	s.SetModel(modelFor("codex"))
	close(backend.release)
	if err := <-promptDone; err != nil {
		t.Fatal(err)
	}
	if backend.model != claudeModel || backend.proxy != wantClaudeProxy {
		t.Fatalf("committed delegated model=%s proxy=%s, want model=%s and Claude account route", backend.model, backend.proxy, claudeModel)
	}

	if _, err := s.Prompt(context.Background(), "codex request"); err != nil {
		t.Fatal(err)
	}
	if codex.calls != 1 || codex.proxy != wantCodexProxy {
		t.Fatalf("next native turn calls=%d proxy=%s, want one Codex-account request", codex.calls, codex.proxy)
	}
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
