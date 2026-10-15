package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetModelRefusesATopLevelThatIsNoObject(t *testing.T) {
	for name, body := range map[string]string{"null": "null\n", "array": "[]", "string": `"x"`} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if set, err := SetModel(path, SubscriptionCodexModel); err == nil || set {
				t.Fatalf("SetModel = %v, %v, want an error", set, err)
			}
			if got, _ := os.ReadFile(path); string(got) != body {
				t.Errorf("file = %q, want it untouched", got)
			}
		})
	}
}

func TestSetModelFindsAnEmptyModelKeyOfAnyCase(t *testing.T) {
	for _, body := range []string{`{"MODEL":""}`, `{"Model":"","web":{"fetch":true}}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if set, err := SetModel(path, SubscriptionCodexModel); err != nil || !set {
			t.Fatalf("SetModel(%s) = %v, %v, want true", body, set, err)
		}
		if cfg, err := Load(path); err != nil || cfg.Model != SubscriptionCodexModel {
			t.Errorf("%s: config = %+v, %v, want the model", body, cfg, err)
		}
	}
}

func TestProviderKeyEnvIsTheEntryVariableElseTheNativeOne(t *testing.T) {
	if got := (Provider{}).KeyEnv("anthropic"); got != "ANTHROPIC_API_KEY" {
		t.Errorf("native anthropic = %q", got)
	}
	if got := (Provider{APIKeyEnv: "WORK"}).KeyEnv("anthropic"); got != "WORK" {
		t.Errorf("entry = %q", got)
	}
	if got := (Provider{}).KeyEnv("gateway"); got != "" {
		t.Errorf("unknown provider = %q", got)
	}
}

func TestSetModelLeavesKeysOfAnotherCaseAndRepeatedObjectsAsTheyLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"model":"","max_tokens":1,"MAX_TOKENS":2,"aliases":{"a":"openai/x"},"aliases":{"b":"openai/y"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(path, SubscriptionCodexModel); err != nil || !set {
		t.Fatalf("SetModel = %v, %v, want true", set, err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Model != SubscriptionCodexModel || cfg.MaxTokens != 2 || len(cfg.Aliases) != 2 {
		t.Errorf("config = %+v, %v, want the model and the other keys as before", cfg, err)
	}
}

func TestNoEnvironmentVariableSetsTheSubscriptionDefault(t *testing.T) {
	var c Config
	if err := c.ApplyEnv(func(name string) string {
		if name == "HARNESS_-" || name == "HARNESS_SUBSCRIPTIONDEFAULT" || name == "HARNESS_SUBSCRIPTION_DEFAULT" {
			return "true"
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	if c.SubscriptionDefault {
		t.Error("an environment variable set SubscriptionDefault")
	}
}

func TestSetModelKeepsTheBytesOfAPluginConfig(t *testing.T) {
	const body = `{"model":"","plugins":[{"name":"p","command":["x"],"config":{ "a": 1,"b":[1, 2] }}]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if set, err := SetModel(path, SubscriptionCodexModel); err != nil || !set {
		t.Fatalf("SetModel = %v, %v, want true", set, err)
	}
	want := strings.Replace(body, `"model":""`, `"model":"`+SubscriptionCodexModel+`"`, 1)
	if got, _ := os.ReadFile(path); string(got) != want {
		t.Errorf("file = %s, want %s", got, want)
	}
}
