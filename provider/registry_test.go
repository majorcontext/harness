package provider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// stubProvider is a minimal provider.Provider for registry tests.
type stubProvider struct{ name string }

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) Stream(context.Context, *provider.Request) (provider.Stream, error) {
	return nil, nil
}

func TestRegistryFor_KnownProvider(t *testing.T) {
	anthropic := &stubProvider{name: "anthropic"}
	reg := provider.Registry{"anthropic": anthropic}

	got, err := reg.For(message.ModelRef{Provider: "anthropic", Model: "claude-x"})
	if err != nil {
		t.Fatalf("For returned error for a registered provider: %v", err)
	}
	if got != anthropic {
		t.Fatalf("For returned %v, want the registered stub", got)
	}
}

func TestRegistryFor_UnknownProvider(t *testing.T) {
	reg := provider.Registry{"anthropic": &stubProvider{name: "anthropic"}}

	ref := message.ModelRef{Provider: "does-not-exist", Model: "some-model"}
	got, err := reg.For(ref)
	if err == nil {
		t.Fatalf("For returned nil error for an unregistered provider %q", ref.Provider)
	}
	if got != nil {
		t.Fatalf("For returned a non-nil provider %v alongside an error", got)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error %q does not name the unregistered provider", err.Error())
	}
	if !strings.Contains(err.Error(), ref.String()) {
		t.Errorf("error %q does not name the requested model %q", err.Error(), ref.String())
	}
}

func TestRegistryFor_EmptyRegistry(t *testing.T) {
	var reg provider.Registry // nil map, the zero value

	_, err := reg.For(message.ModelRef{Provider: "anthropic", Model: "claude-x"})
	if err == nil {
		t.Fatal("For on a nil Registry should error, not panic or succeed")
	}
}
