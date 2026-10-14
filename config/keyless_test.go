package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMergeProjectMarksOnlyAProviderThatTheProjectFileAloneDefines(t *testing.T) {
	user := &Config{Providers: map[string]Provider{"shared": {Type: TypeOpenAICompat, BaseURL: "http://u"}}}
	for name, c := range map[string]struct {
		entry string
		want  bool
	}{"mine": {`{"type":"openai-compat","base_url":"http://p"}`, true}, "shared": {`{"no_prompt_cache_key":true}`, false}, "openai": {`{}`, false}, "anthropic": {`{}`, false}} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".harness.json"), []byte(`{"providers":{"`+name+`":`+c.entry+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := MergeProject(user, "user", dir, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Providers[name].Keyless(); got != c.want {
			t.Errorf("providers.%s.Keyless() = %v, want %v", name, got, c.want)
		}
	}
}
