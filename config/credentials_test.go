package config

import "testing"

func TestProviderCredentialsUseTheSelectedSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider Provider
		env      map[string]string
		want     bool
	}{
		{"absent", Provider{}, nil, false},
		{"native key", Provider{}, map[string]string{"ANTHROPIC_API_KEY": "placeholder"}, true},
		{"configured key", Provider{APIKeyEnv: "WORK_KEY"}, map[string]string{"WORK_KEY": "placeholder"}, true},
		{"configured key does not fall back", Provider{APIKeyEnv: "WORK_KEY"}, map[string]string{"ANTHROPIC_API_KEY": "placeholder"}, false},
		{"base URL alone", Provider{BaseURL: "https://proxy.example"}, nil, false},
		{"injected auth", Provider{Auth: AuthInjected}, nil, true},
		{"key header", Provider{ExtraHeaders: map[string]string{"X-API-Key": "placeholder"}}, nil, true},
		{"bearer header", Provider{ExtraHeaders: map[string]string{"authorization": "Bearer placeholder"}}, nil, true},
		{"attribution is not auth", Provider{ExtraHeaders: map[string]string{"X-User": "owner"}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.provider.HasCredentials("anthropic", func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("HasCredentials = %v, want %v", got, tc.want)
			}
		})
	}
}
