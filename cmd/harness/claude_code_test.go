package main

import (
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider/claudecode"
)

// TestRegistryClaudeCodeCLIRegistersStubUnderKey proves a claude-code-cli
// entry registers a claudecode.Client under its providers-map key, exactly
// like registerOpenAICompatProviders does for its own type — this is what
// makes Session.ModelSupported accept a swap to a claude-code model ref
// (see registerClaudeCodeProviders' own doc comment).
func TestRegistryClaudeCodeCLIRegistersStubUnderKey(t *testing.T) {
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"claude-code": {Type: config.TypeClaudeCodeCLI, BinaryPath: "/usr/local/bin/claude"},
	}})
	if _, ok := reg["claude-code"].(claudecode.Client); !ok {
		t.Fatalf("claude-code provider is %T, want claudecode.Client", reg["claude-code"])
	}
	ref, err := message.ParseModelRef("claude-code/sonnet")
	if err != nil {
		t.Fatalf("ParseModelRef: %v", err)
	}
	if _, err := reg.For(ref); err != nil {
		t.Errorf("reg.For(%s): %v, want a registered adapter", ref, err)
	}
}

// TestClaudeCodeConfigForTranslatesProviderFields proves
// claudeCodeConfigFor carries BinaryPath/ExtraArgs/PermissionMode from a
// config.Provider entry into engine.ClaudeCodeConfig — the one translation
// point between the file-config Provider type and the engine's own
// backend-agnostic Config (engine deliberately does not import config; see
// claudeCodeConfigFor's own doc comment).
func TestClaudeCodeConfigForTranslatesProviderFields(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.Provider{
		"claude-code": {
			Type:           config.TypeClaudeCodeCLI,
			BinaryPath:     "/opt/claude/bin/claude",
			ExtraArgs:      []string{"--mcp-config", "/tmp/mcp.json"},
			PermissionMode: "acceptEdits",
		},
	}}
	got := claudeCodeConfigFor(cfg, claudecode.Family)
	want := engine.ClaudeCodeConfig{
		BinaryPath:     "/opt/claude/bin/claude",
		ExtraArgs:      []string{"--mcp-config", "/tmp/mcp.json"},
		PermissionMode: "acceptEdits",
	}
	if got.BinaryPath != want.BinaryPath || got.PermissionMode != want.PermissionMode {
		t.Errorf("claudeCodeConfigFor = %+v, want %+v", got, want)
	}
	if len(got.ExtraArgs) != len(want.ExtraArgs) {
		t.Fatalf("ExtraArgs = %+v, want %+v", got.ExtraArgs, want.ExtraArgs)
	}
	for i := range want.ExtraArgs {
		if got.ExtraArgs[i] != want.ExtraArgs[i] {
			t.Errorf("ExtraArgs[%d] = %q, want %q", i, got.ExtraArgs[i], want.ExtraArgs[i])
		}
	}
}

// TestClaudeCodeConfigForAbsentEntryYieldsZeroValue proves a config with no
// matching entry (or a nil *config.Config, the same defensive shape every
// other cmd/harness helper here handles) yields the zero ClaudeCodeConfig
// rather than panicking — engine.newSession's own BinaryPath default
// ("claude") then applies.
func TestClaudeCodeConfigForAbsentEntryYieldsZeroValue(t *testing.T) {
	assertZero := func(t *testing.T, got engine.ClaudeCodeConfig) {
		t.Helper()
		if got.BinaryPath != "" || got.PermissionMode != "" || len(got.ExtraArgs) != 0 {
			t.Errorf("claudeCodeConfigFor = %+v, want the zero value", got)
		}
	}
	assertZero(t, claudeCodeConfigFor(nil, claudecode.Family))
	cfg := &config.Config{Providers: map[string]config.Provider{
		"anthropic": {APIKeyEnv: "X"},
	}}
	assertZero(t, claudeCodeConfigFor(cfg, claudecode.Family))
}
