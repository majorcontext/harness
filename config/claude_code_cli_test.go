package config

import (
	"strings"
	"testing"
)

// TestLoadProviderClaudeCodeCLI proves a minimal, valid entry parses and
// keeps its BinaryPath/ExtraArgs/PermissionMode fields intact — the same
// shape TestLoadProviderOpenAICompat establishes for that type.
func TestLoadProviderClaudeCodeCLI(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/config.json"
	writeFile(t, p, `{
		"providers": {
			"claude-code": {
				"type": "claude-code-cli",
				"binary_path": "/usr/local/bin/claude",
				"extra_args": ["--mcp-config", "/tmp/mcp.json"],
				"permission_mode": "acceptEdits"
			}
		}
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pr, ok := c.Providers["claude-code"]
	if !ok {
		t.Fatal("providers.claude-code missing")
	}
	if pr.Type != TypeClaudeCodeCLI {
		t.Errorf("Type = %q, want %q", pr.Type, TypeClaudeCodeCLI)
	}
	if pr.BinaryPath != "/usr/local/bin/claude" {
		t.Errorf("BinaryPath = %q", pr.BinaryPath)
	}
	if len(pr.ExtraArgs) != 2 || pr.ExtraArgs[0] != "--mcp-config" {
		t.Errorf("ExtraArgs = %+v", pr.ExtraArgs)
	}
	if pr.PermissionMode != "acceptEdits" {
		t.Errorf("PermissionMode = %q", pr.PermissionMode)
	}
}

// TestClaudeCodeCLINoBaseURLRequired proves the one deliberate divergence
// from TypeOpenAICompat/TypeOpenAI: this type spawns a process rather than
// dialing an HTTP endpoint, so validateProviders must accept an entry with
// no base_url at all.
func TestClaudeCodeCLINoBaseURLRequired(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"claude-code": {Type: TypeClaudeCodeCLI},
	}}
	if _, err := mergeAndValidate(c, &Config{}); err != nil {
		t.Fatalf("mergeAndValidate: %v, want no error for a base_url-less claude-code-cli entry", err)
	}
}

// TestClaudeCodeCLIUnknownPermissionModeFails proves a typo'd
// permission_mode fails loudly at config-load time rather than reaching
// the `claude` child's own --permission-mode flag and failing there,
// mirroring TestLoadProviderOpenAICompatMissingBaseURLFails's own
// "fail close to the mistake" shape.
func TestClaudeCodeCLIUnknownPermissionModeFails(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"claude-code": {Type: TypeClaudeCodeCLI, PermissionMode: "yolo"},
	}}
	_, err := mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on unknown permission_mode")
	}
	if !strings.Contains(err.Error(), "yolo") {
		t.Errorf("error %q does not name the offending value", err)
	}
}

// TestClaudeCodeCLIEmptyPermissionModeOK proves the field is optional: an
// entry naming no permission_mode at all is valid (the CLI's own default
// applies, no flag sent).
func TestClaudeCodeCLIEmptyPermissionModeOK(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"claude-code": {Type: TypeClaudeCodeCLI},
	}}
	if _, err := mergeAndValidate(c, &Config{}); err != nil {
		t.Fatalf("mergeAndValidate: %v", err)
	}
}

// TestClaudeCodeFieldsRejectedOnOtherTypes proves BinaryPath/ExtraArgs/
// PermissionMode are type-scoped, exactly like ResponsesPath/CacheTTL are
// for their own adapters — a config author setting one of these on, say,
// an openai-compat entry gets a loud, specific error instead of a value
// that silently vanishes into a client that never reads it.
func TestClaudeCodeFieldsRejectedOnOtherTypes(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
	}{
		{"binary_path", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", BinaryPath: "/bin/claude"}},
		{"extra_args", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", ExtraArgs: []string{"--foo"}}},
		{"permission_mode", Provider{Type: TypeOpenAICompat, BaseURL: "http://x", PermissionMode: "acceptEdits"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Providers: map[string]Provider{"mycompat": tc.p}}
			_, err := mergeAndValidate(c, &Config{})
			if err == nil {
				t.Fatalf("mergeAndValidate did not fail on %s set for a non-claude-code-cli entry", tc.name)
			}
			if !strings.Contains(err.Error(), TypeClaudeCodeCLI) {
				t.Errorf("error %q does not name %q as the only valid type", err, TypeClaudeCodeCLI)
			}
		})
	}
}

// TestClaudeCodeCLIUnknownTypeErrorListsIt proves the unknown-type error
// message advertises claude-code-cli alongside the other valid types, so a
// config author typo'ing the type string is told the real option exists.
func TestClaudeCodeCLIUnknownTypeErrorListsIt(t *testing.T) {
	c := &Config{Providers: map[string]Provider{
		"mystery": {Type: "carrier-pigeon"},
	}}
	_, err := mergeAndValidate(c, &Config{})
	if err == nil {
		t.Fatal("mergeAndValidate did not fail on unknown provider type")
	}
	if !strings.Contains(err.Error(), TypeClaudeCodeCLI) {
		t.Errorf("error %q does not list %q as a valid type", err, TypeClaudeCodeCLI)
	}
}

func TestAppendSystemPromptValidation(t *testing.T) {
	for _, arg := range []string{
		"--append-system-prompt",
		"--append-system-prompt=x",
		"--append-system-prompt-file",
		"--append-system-prompt-file=x",
	} {
		t.Run(arg, func(t *testing.T) {
			cfg := &Config{
				AppendSystemPrompt: []string{"platform"},
				Providers: map[string]Provider{
					"claude-code": {Type: TypeClaudeCodeCLI, ExtraArgs: []string{arg}},
				},
			}
			if _, err := mergeAndValidate(cfg, &Config{}); err == nil {
				t.Fatal("accepted conflicting extra_args")
			}
		})
	}
}

func TestAppendSystemPromptAllowsCompatibleExtraArgs(t *testing.T) {
	cfg := &Config{
		AppendSystemPrompt: []string{"platform"},
		Providers: map[string]Provider{
			"claude-code": {Type: TypeClaudeCodeCLI, ExtraArgs: []string{"--dangerously-skip-permissions"}},
		},
	}
	if _, err := mergeAndValidate(cfg, &Config{}); err != nil {
		t.Fatal(err)
	}

	cfg.AppendSystemPrompt = nil
	cfg.Providers["claude-code"] = Provider{
		Type: TypeClaudeCodeCLI, ExtraArgs: []string{"--append-system-prompt", "legacy"},
	}
	if _, err := mergeAndValidate(cfg, &Config{}); err != nil {
		t.Fatal(err)
	}
}

// TestMergeClaudeCodeExtraArgsNotAliased mirrors
// TestMergeProviderExtraHeadersBaseOnlyKeyNotAliased for the new slice
// field: a base-only key's ExtraArgs must not alias the base config's own
// backing array once merged.
func TestMergeClaudeCodeExtraArgsNotAliased(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"claude-code": {Type: TypeClaudeCodeCLI, ExtraArgs: []string{"--a"}},
	}}
	merged := merge(base, &Config{})
	pr := merged.Providers["claude-code"]
	pr.ExtraArgs[0] = "mutated"
	if base.Providers["claude-code"].ExtraArgs[0] != "--a" {
		t.Error("merge aliased the base provider's ExtraArgs slice")
	}
}

// TestMergeClaudeCodeFieldsOverride proves a project-layer override
// replaces BinaryPath/PermissionMode and replaces ExtraArgs wholesale
// (never element-wise merges it), exactly like OmitResponseParams'
// documented merge semantics.
func TestMergeClaudeCodeFieldsOverride(t *testing.T) {
	base := &Config{Providers: map[string]Provider{
		"claude-code": {
			Type:           TypeClaudeCodeCLI,
			BinaryPath:     "/user/claude",
			ExtraArgs:      []string{"--user-flag"},
			PermissionMode: "default",
		},
	}}
	over := &Config{Providers: map[string]Provider{
		"claude-code": {
			BinaryPath:     "/project/claude",
			ExtraArgs:      []string{"--project-flag"},
			PermissionMode: "acceptEdits",
		},
	}}
	merged := merge(base, over)
	pr := merged.Providers["claude-code"]
	if pr.BinaryPath != "/project/claude" {
		t.Errorf("BinaryPath = %q, want project override", pr.BinaryPath)
	}
	if len(pr.ExtraArgs) != 1 || pr.ExtraArgs[0] != "--project-flag" {
		t.Errorf("ExtraArgs = %+v, want wholesale project override", pr.ExtraArgs)
	}
	if pr.PermissionMode != "acceptEdits" {
		t.Errorf("PermissionMode = %q, want project override", pr.PermissionMode)
	}
}
