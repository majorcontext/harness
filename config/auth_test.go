package config

import (
	"strings"
	"testing"
)

func TestAuthValidation(t *testing.T) {
	const codex = "https://chatgpt.com/backend-api/codex"
	for _, tc := range []struct {
		name string
		cfg  Config
		want string // empty: valid
	}{
		{"type openai with the subscription login", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, Auth: AuthChatGPT, BaseURL: codex}}}, ""},
		{"loopback http base url", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, Auth: AuthChatGPT, BaseURL: "http://127.0.0.1:8080"}}}, ""},
		{"native openai key without a base url", Config{Providers: map[string]Provider{"openai": {Auth: AuthChatGPT}}}, ""},
		{"unknown auth", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, Auth: "token", BaseURL: codex}}}, "unknown auth"},
		{"with api_key_env", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, Auth: AuthChatGPT, APIKeyEnv: "K", BaseURL: codex}}}, "mutually exclusive"},
		{"on openai-compat", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAICompat, Auth: AuthChatGPT, BaseURL: codex}}}, "only valid on an entry that builds the OpenAI Responses adapter"},
		{"plain http to a remote host", Config{Providers: map[string]Provider{"codex": {Type: TypeOpenAI, Auth: AuthChatGPT, BaseURL: "http://gateway.example"}}}, "https"},
		{"codex_issuer over plain http to a remote host", Config{CodexIssuer: "http://issuer.example"}, "codex_issuer"},
		{"codex_issuer on loopback", Config{CodexIssuer: "http://127.0.0.1:9"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := mergeAndValidate(&tc.cfg, &Config{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("Validate = %v, want an error with %q", err, tc.want)
			case tc.want == "" && tc.cfg.Providers != nil:
				for name, p := range tc.cfg.Providers {
					if merged.Providers[name].Auth != p.Auth {
						t.Errorf("merged auth = %q, want %q", merged.Providers[name].Auth, p.Auth)
					}
				}
			}
		})
	}
}

func TestProjectConfigCannotSetTheCodexLoginFiles(t *testing.T) {
	user := &Config{CodexAuthFile: "/user/codex.json", CodexIssuer: "https://issuer.example"}
	merged, err := mergeAndValidate(user, &Config{CodexAuthFile: "/project/steal.json", CodexIssuer: "https://evil.example"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.CodexAuthFile != user.CodexAuthFile || merged.CodexIssuer != user.CodexIssuer {
		t.Errorf("merged = %q %q, want the user values", merged.CodexAuthFile, merged.CodexIssuer)
	}
}

func TestCodexIssuerHasNoEnvironmentVariable(t *testing.T) {
	c := &Config{}
	env := map[string]string{"HARNESS_CODEX_ISSUER": "http://remote.example", "HARNESS_CODEX_AUTH_FILE": "/tmp/codex.json"}
	if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if c.CodexIssuer != "" || c.CodexAuthFile != "/tmp/codex.json" {
		t.Errorf("issuer %q, auth file %q, want no issuer from the environment and the file from it", c.CodexIssuer, c.CodexAuthFile)
	}
}
