package config

import (
	"reflect"
	"testing"
)

func TestNativeProviderCodexIsTheCodexLaneEntry(t *testing.T) {
	got, ok := NativeProvider("codex")
	if !ok {
		t.Fatal("codex is not native")
	}
	want := Provider{
		Type:                TypeOpenAI,
		Auth:                AuthChatGPT,
		BaseURL:             "https://chatgpt.com/backend-api/codex",
		ResponsesPath:       "/responses",
		OmitResponseParams:  []string{OmitResponseParamMaxOutputTokens, OmitResponseParamTemperature, OmitResponseParamTopP, OmitResponseParamMetadata},
		SanitizeToolSchemas: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("codex = %+v, want %+v", got, want)
	}
	if err := validateProviders(map[string]Provider{"codex": got}); err != nil {
		t.Errorf("the native codex entry fails validation: %v", err)
	}
}

func TestNativeProvidersNameEachBuiltInProvider(t *testing.T) {
	want := []string{"anthropic", "claude-code", "codex", "openai"}
	if got := NativeProviders(); !reflect.DeepEqual(got, want) {
		t.Errorf("NativeProviders = %q, want %q", got, want)
	}
}

func TestProviderEntriesAddNativeProvidersAndKeepConfiguredOnes(t *testing.T) {
	own := Provider{Type: TypeOpenAI, BaseURL: "https://example.com", APIKeyEnv: "K"}
	c := &Config{Providers: map[string]Provider{"codex": own, "work": {Type: TypeClaudeCodeCLI, BinaryPath: "/bin/c"}}}
	got := c.ProviderEntries()
	if !reflect.DeepEqual(got["codex"], own) {
		t.Errorf("codex = %+v, want the configured entry alone", got["codex"])
	}
	if got["work"].BinaryPath != "/bin/c" {
		t.Errorf("work = %+v, want the configured entry", got["work"])
	}
	for _, name := range []string{"anthropic", "openai", "claude-code"} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s is missing", name)
		}
	}
	if got["claude-code"].Type != TypeClaudeCodeCLI || len(c.Providers) != 2 {
		t.Errorf("ProviderEntries changed Providers: %+v", c.Providers)
	}
}

func TestClaudeCLIPrefersAConfiguredEntryToTheNativeProgram(t *testing.T) {
	if bin, ok := (&Config{}).ClaudeCLI(); !ok || bin != "claude" {
		t.Errorf("ClaudeCLI = %q, %v, want claude", bin, ok)
	}
	c := &Config{Providers: map[string]Provider{"work": {Type: TypeClaudeCodeCLI, BinaryPath: "/bin/work"}}}
	if bin, _ := c.ClaudeCLI(); bin != "/bin/work" {
		t.Errorf("ClaudeCLI = %q, want /bin/work", bin)
	}
	c.Providers["claude-code"] = Provider{Type: TypeClaudeCodeCLI, BinaryPath: "/bin/own"}
	if bin, _ := c.ClaudeCLI(); bin != "/bin/own" {
		t.Errorf("ClaudeCLI = %q, want /bin/own", bin)
	}
}
