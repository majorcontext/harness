package e2e

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// The anthropic entry names a variable that no process sets, so the API key rule cannot keep the default: only the missing flag does.
// loginFileWithContent is a non-empty Codex login file, which a default model that followed the subscription would pick.
var loginFileWithContent = sync.OnceValue(func() string {
	dir, err := os.MkdirTemp("", "e2e-codex-login")
	if err != nil {
		panic(err)
	}
	p := filepath.Join(dir, "codex.json")
	if err := os.WriteFile(p, []byte(`{"access_token":"x","account_id":"a"}`), 0o600); err != nil {
		panic(err)
	}
	return p
})

func TestContractModelsDefault(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "create_without_a_model_ignores_claude_on_path_and_a_codex_login",
			config:  map[string]any{"model": "", "codex_auth_file": loginFileWithContent(), "providers": map[string]any{"anthropic": map[string]any{"api_key_env": "HARNESS_E2E_NEVER_SET"}}},
			actions: []action{tryCreate{model: ""}},
		},
	})
}
