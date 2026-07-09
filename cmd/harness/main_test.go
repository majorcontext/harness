package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/anthropic"
	"github.com/majorcontext/harness/provider/openai"
	"github.com/majorcontext/harness/provider/openaicompat"
)

func TestSessionDir(t *testing.T) {
	t.Run("no-save disables persistence", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/somewhere")
		dir, err := sessionDir(true, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "" {
			t.Errorf("sessionDir(noSave) = %q, want empty", dir)
		}
	})
	t.Run("env var wins", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "/custom/sessions")
		dir, err := sessionDir(false, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "/custom/sessions" {
			t.Errorf("sessionDir = %q, want /custom/sessions", dir)
		}
	})
	t.Run("config dir beats default when env unset", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "")
		dir, err := sessionDir(false, "/config/sessions")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		if dir != "/config/sessions" {
			t.Errorf("sessionDir = %q, want /config/sessions", dir)
		}
	})
	t.Run("defaults to HOME/.harness/sessions", func(t *testing.T) {
		t.Setenv("HARNESS_SESSION_DIR", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir, err := sessionDir(false, "")
		if err != nil {
			t.Fatalf("sessionDir: %v", err)
		}
		want := filepath.Join(home, ".harness", "sessions")
		if dir != want {
			t.Errorf("sessionDir = %q, want %q", dir, want)
		}
	})
	t.Run("full precedence chain", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// -no-save beats everything.
		t.Setenv("HARNESS_SESSION_DIR", "/env")
		if dir, _ := sessionDir(true, "/config"); dir != "" {
			t.Errorf("no-save: got %q, want empty", dir)
		}
		// env beats config.
		if dir, _ := sessionDir(false, "/config"); dir != "/env" {
			t.Errorf("env: got %q, want /env", dir)
		}
		// config beats default.
		t.Setenv("HARNESS_SESSION_DIR", "")
		if dir, _ := sessionDir(false, "/config"); dir != "/config" {
			t.Errorf("config: got %q, want /config", dir)
		}
		// default when nothing set.
		want := filepath.Join(home, ".harness", "sessions")
		if dir, _ := sessionDir(false, ""); dir != want {
			t.Errorf("default: got %q, want %q", dir, want)
		}
	})
}

// writeSessionFile writes a session log in the JSONL format documented in
// engine/store.go: a session header, a model record, then message records.
func writeSessionFile(t *testing.T, dir, id string, createdAt time.Time, messages int) {
	t.Helper()
	f := fmt.Sprintf("{\"type\":\"session\",\"id\":%q,\"created_at\":%q}\n",
		id, createdAt.Format(time.RFC3339Nano))
	f += "{\"type\":\"model\",\"model\":\"anthropic/persisted-model\"}\n"
	for i := 0; i < messages; i++ {
		f += fmt.Sprintf("{\"type\":\"message\",\"message\":{\"id\":\"msg_%d\",\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"text\":\"hello %d\"}],\"created_at\":%q}}\n",
			i, i, createdAt.Format(time.RFC3339Nano))
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(f), 0o644); err != nil {
		t.Fatalf("writing session file: %v", err)
	}
}

func TestResolveSession(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	writeSessionFile(t, dir, "ses_1111111111111111", base, 2)
	writeSessionFile(t, dir, "ses_2222222222222222", base.Add(time.Hour), 4)
	cfg := engine.Config{SessionDir: dir}

	t.Run("new session by default", func(t *testing.T) {
		s, err := resolveSession(cfg, "", false, false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID == "ses_1111111111111111" || s.ID == "ses_2222222222222222" {
			t.Errorf("expected fresh session, got existing ID %q", s.ID)
		}
		if len(s.History()) != 0 {
			t.Errorf("fresh session has %d messages, want 0", len(s.History()))
		}
	})
	t.Run("resume by id", func(t *testing.T) {
		s, err := resolveSession(cfg, "ses_1111111111111111", false, false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID != "ses_1111111111111111" {
			t.Errorf("s.ID = %q, want ses_old", s.ID)
		}
		if got := len(s.History()); got != 2 {
			t.Errorf("history length = %d, want 2", got)
		}
	})
	t.Run("continue picks most recent", func(t *testing.T) {
		s, err := resolveSession(cfg, "", true, false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if s.ID != "ses_2222222222222222" {
			t.Errorf("s.ID = %q, want ses_new", s.ID)
		}
		if got := len(s.History()); got != 4 {
			t.Errorf("history length = %d, want 4", got)
		}
	})
	t.Run("resume and continue are mutually exclusive", func(t *testing.T) {
		if _, err := resolveSession(cfg, "ses_1111111111111111", true, false); err == nil {
			t.Error("expected error for -r with -c")
		}
	})
	t.Run("continue with no sessions errors", func(t *testing.T) {
		empty := engine.Config{SessionDir: t.TempDir()}
		if _, err := resolveSession(empty, "", true, false); err == nil {
			t.Error("expected error when no sessions exist")
		}
	})
	t.Run("resume unknown id errors", func(t *testing.T) {
		if _, err := resolveSession(cfg, "ses_9999999999999999", false, false); err == nil {
			t.Error("expected error for unknown session id")
		}
	})
}

func TestFormatSessions(t *testing.T) {
	t.Run("empty list yields no output", func(t *testing.T) {
		if got := formatSessions(nil); got != "" {
			t.Errorf("formatSessions(nil) = %q, want empty", got)
		}
	})
	t.Run("one line per session, tab-separated", func(t *testing.T) {
		infos := []engine.SessionInfo{
			{ID: "ses_a", CreatedAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC), Messages: 2},
			{ID: "ses_b", CreatedAt: time.Date(2024, 6, 1, 13, 30, 0, 0, time.UTC), Messages: 5},
		}
		want := "ses_a\t2024-06-01T12:00:00Z\t2\nses_b\t2024-06-01T13:30:00Z\t5\n"
		if got := formatSessions(infos); got != want {
			t.Errorf("formatSessions = %q, want %q", got, want)
		}
	})
}

func TestFormatSessionsJSON(t *testing.T) {
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		infos []engine.SessionInfo
		want  []sessionJSON
	}{
		{
			name:  "empty list yields empty JSON array",
			infos: nil,
			want:  []sessionJSON{},
		},
		{
			name: "multiple sessions marshal in order",
			infos: []engine.SessionInfo{
				{ID: "ses_a", CreatedAt: base, Messages: 2},
				{ID: "ses_b", CreatedAt: base.Add(90 * time.Minute), Messages: 5},
			},
			want: []sessionJSON{
				{ID: "ses_a", CreatedAt: base, Messages: 2},
				{ID: "ses_b", CreatedAt: base.Add(90 * time.Minute), Messages: 5},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := formatSessionsJSON(tt.infos)
			if err != nil {
				t.Fatalf("formatSessionsJSON: %v", err)
			}
			// Empty list must print "[]", not "null" or nothing.
			if len(tt.infos) == 0 && !strings.HasPrefix(strings.TrimSpace(out), "[]") {
				t.Errorf("empty list = %q, want %q", out, "[]")
			}
			var got []sessionJSON
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not valid JSON: %v\noutput: %q", err, out)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d sessions, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i].ID != tt.want[i].ID || got[i].Messages != tt.want[i].Messages ||
					!got[i].CreatedAt.Equal(tt.want[i].CreatedAt) {
					t.Errorf("session[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolveSessionNoSave(t *testing.T) {
	// -no-save yields an empty SessionDir; resuming must fail with a
	// clear error before touching the engine.
	cfg := engine.Config{SessionDir: ""}
	t.Run("resume with no-save errors clearly", func(t *testing.T) {
		_, err := resolveSession(cfg, "ses_x", false, false)
		if err == nil {
			t.Fatal("expected error for -r with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("continue with no-save errors clearly", func(t *testing.T) {
		_, err := resolveSession(cfg, "", true, false)
		if err == nil {
			t.Fatal("expected error for -c with -no-save")
		}
		if !strings.Contains(err.Error(), "-no-save") {
			t.Errorf("error = %q, want mention of -no-save", err)
		}
	})
	t.Run("new session with no-save is fine", func(t *testing.T) {
		if _, err := resolveSession(cfg, "", false, false); err != nil {
			t.Errorf("resolveSession: %v", err)
		}
	})
}

func TestResolveSessionModelFlag(t *testing.T) {
	persisted := message.ModelRef{Provider: "anthropic", Model: "persisted-model"}
	flagModel := message.ModelRef{Provider: "anthropic", Model: "flag-model"}
	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	newCfg := func(t *testing.T) engine.Config {
		t.Helper()
		dir := t.TempDir()
		writeSessionFile(t, dir, "ses_3333333333333333", base, 2)
		return engine.Config{SessionDir: dir, Model: flagModel}
	}

	t.Run("explicit -model wins on resume", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "ses_3333333333333333", false, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v (explicit flag must win)", got, flagModel)
		}
		// SetModel persists a model record, so a subsequent load sees the
		// override too.
		s2, err := engine.LoadSession(cfg, "ses_3333333333333333")
		if err != nil {
			t.Fatalf("LoadSession after override: %v", err)
		}
		if got := s2.Model(); got != flagModel {
			t.Errorf("reloaded s.Model() = %v, want %v (override must persist)", got, flagModel)
		}
	})
	t.Run("persisted model retained without explicit -model", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "ses_3333333333333333", false, false)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != persisted {
			t.Errorf("s.Model() = %v, want %v (persisted model must be retained)", got, persisted)
		}
	})
	t.Run("explicit -model on continue wins too", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "", true, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v (explicit flag must win)", got, flagModel)
		}
	})
	t.Run("fresh session uses flag model regardless", func(t *testing.T) {
		cfg := newCfg(t)
		s, err := resolveSession(cfg, "", false, true)
		if err != nil {
			t.Fatalf("resolveSession: %v", err)
		}
		if got := s.Model(); got != flagModel {
			t.Errorf("s.Model() = %v, want %v", got, flagModel)
		}
	})
}

func TestInstructionsConfig(t *testing.T) {
	trueV, falseV := true, false
	t.Run("no-instructions flag disables", func(t *testing.T) {
		ic := instructionsConfig(&config.Config{}, true)
		if ic == nil || !ic.Disabled {
			t.Errorf("ic = %+v, want disabled", ic)
		}
	})
	t.Run("config instructions:false disables", func(t *testing.T) {
		ic := instructionsConfig(&config.Config{Instructions: &falseV}, false)
		if ic == nil || !ic.Disabled {
			t.Errorf("ic = %+v, want disabled", ic)
		}
	})
	t.Run("flag wins over config instructions:true", func(t *testing.T) {
		ic := instructionsConfig(&config.Config{Instructions: &trueV}, true)
		if ic == nil || !ic.Disabled {
			t.Errorf("ic = %+v, want disabled (flag wins)", ic)
		}
	})
	t.Run("config path override", func(t *testing.T) {
		ic := instructionsConfig(&config.Config{InstructionsPath: "x/AGENTS.md"}, false)
		if ic == nil || ic.Disabled || ic.Path != "x/AGENTS.md" {
			t.Errorf("ic = %+v, want path override", ic)
		}
	})
	t.Run("default nil enables auto-discovery", func(t *testing.T) {
		if ic := instructionsConfig(&config.Config{}, false); ic != nil {
			t.Errorf("ic = %+v, want nil (default enabled)", ic)
		}
	})
	t.Run("instructions:true without path stays nil", func(t *testing.T) {
		if ic := instructionsConfig(&config.Config{Instructions: &trueV}, false); ic != nil {
			t.Errorf("ic = %+v, want nil", ic)
		}
	})
	t.Run("nil config is safe", func(t *testing.T) {
		if ic := instructionsConfig(nil, false); ic != nil {
			t.Errorf("ic = %+v, want nil", ic)
		}
	})
}

func TestSkillsDirsExplicitEmptyDisables(t *testing.T) {
	// A config file with "skills_dirs": [] is an explicit opt-out and must
	// reach the engine as a non-nil empty slice (disable), not nil
	// (default-on). Review finding on #21.
	got := skillsDirs(&config.Config{SkillsDirs: []string{}}, nil, "/w")
	if got == nil {
		t.Fatal("explicit empty skills_dirs collapsed to nil (re-enables default)")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
	// Absent config stays nil → engine default applies.
	if got := skillsDirs(&config.Config{}, nil, "/w"); got != nil {
		t.Fatalf("absent skills_dirs = %v, want nil", got)
	}
}

func TestSkillsDirs(t *testing.T) {
	t.Run("default nil when nothing configured", func(t *testing.T) {
		if dirs := skillsDirs(&config.Config{}, nil, "/work"); dirs != nil {
			t.Errorf("dirs = %v, want nil (engine default)", dirs)
		}
	})
	t.Run("config dirs resolved against workDir", func(t *testing.T) {
		dirs := skillsDirs(&config.Config{SkillsDirs: []string{"a/skills", "/abs/skills"}}, nil, "/work")
		want := []string{filepath.Join("/work", "a/skills"), "/abs/skills"}
		if len(dirs) != 2 || dirs[0] != want[0] || dirs[1] != want[1] {
			t.Errorf("dirs = %v, want %v", dirs, want)
		}
	})
	t.Run("flag overrides config entirely", func(t *testing.T) {
		dirs := skillsDirs(&config.Config{SkillsDirs: []string{"cfg/skills"}}, []string{"flag/skills"}, "/work")
		if len(dirs) != 1 || dirs[0] != filepath.Join("/work", "flag/skills") {
			t.Errorf("dirs = %v, want flag override", dirs)
		}
	})
	t.Run("nil config is safe", func(t *testing.T) {
		if dirs := skillsDirs(nil, nil, "/work"); dirs != nil {
			t.Errorf("dirs = %v, want nil", dirs)
		}
	})
}

func TestRegistry(t *testing.T) {
	t.Run("defaults to ANTHROPIC_API_KEY and empty base url", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-default")
		reg := registry(&config.Config{})
		c, ok := reg[anthropic.Family].(*anthropic.Client)
		if !ok {
			t.Fatalf("anthropic provider is %T, want *anthropic.Client", reg[anthropic.Family])
		}
		if c.APIKey != "sk-default" {
			t.Errorf("APIKey = %q, want sk-default", c.APIKey)
		}
		if c.BaseURL != "" {
			t.Errorf("BaseURL = %q, want empty", c.BaseURL)
		}
	})
	t.Run("openai wired with OPENAI_API_KEY default", func(t *testing.T) {
		t.Setenv("OPENAI_API_KEY", "sk-oai")
		reg := registry(&config.Config{})
		c, ok := reg[openai.Family].(*openai.Client)
		if !ok {
			t.Fatalf("openai provider is %T, want *openai.Client", reg[openai.Family])
		}
		if c.APIKey != "sk-oai" {
			t.Errorf("APIKey = %q, want sk-oai", c.APIKey)
		}
	})
	t.Run("config api_key_env and base_url are honored", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "ignored")
		t.Setenv("MY_ANTHROPIC_KEY", "sk-custom")
		reg := registry(&config.Config{Providers: map[string]config.Provider{
			"anthropic": {APIKeyEnv: "MY_ANTHROPIC_KEY", BaseURL: "http://proxy"},
		}})
		c := reg[anthropic.Family].(*anthropic.Client)
		if c.APIKey != "sk-custom" {
			t.Errorf("APIKey = %q, want sk-custom", c.APIKey)
		}
		if c.BaseURL != "http://proxy" {
			t.Errorf("BaseURL = %q, want http://proxy", c.BaseURL)
		}
	})
	t.Run("nil config is safe", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "sk-nil")
		reg := registry(nil)
		if c := reg[anthropic.Family].(*anthropic.Client); c.APIKey != "sk-nil" {
			t.Errorf("APIKey = %q, want sk-nil", c.APIKey)
		}
	})
	t.Run("openai-compat entry builds a named provider", func(t *testing.T) {
		t.Setenv("MY_COMPAT_KEY", "sk-compat")
		reg := registry(&config.Config{Providers: map[string]config.Provider{
			"mycompat": {
				Type:         config.TypeOpenAICompat,
				BaseURL:      "http://compat.example",
				APIKeyEnv:    "MY_COMPAT_KEY",
				Family:       "mycompat-quirks",
				ExtraHeaders: map[string]string{"X-Extra": "yes"},
			},
		}})
		c, ok := reg["mycompat"].(*openaicompat.Client)
		if !ok {
			t.Fatalf("mycompat provider is %T, want *openaicompat.Client", reg["mycompat"])
		}
		if c.Family != "mycompat-quirks" {
			t.Errorf("Family = %q, want mycompat-quirks", c.Family)
		}
		if c.APIKey != "sk-compat" {
			t.Errorf("APIKey = %q, want sk-compat", c.APIKey)
		}
		if c.BaseURL != "http://compat.example" {
			t.Errorf("BaseURL = %q, want http://compat.example", c.BaseURL)
		}
		if c.ExtraHeaders["X-Extra"] != "yes" {
			t.Errorf("ExtraHeaders = %+v, want X-Extra=yes", c.ExtraHeaders)
		}
	})
	t.Run("default openrouter registered when absent from config", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-or-default")
		reg := registry(&config.Config{})
		c, ok := reg["openrouter"].(*openaicompat.Client)
		if !ok {
			t.Fatalf("openrouter provider is %T, want *openaicompat.Client", reg["openrouter"])
		}
		if c.Family != "openrouter" {
			t.Errorf("Family = %q, want openrouter", c.Family)
		}
		if c.BaseURL != "https://openrouter.ai/api/v1" {
			t.Errorf("BaseURL = %q, want https://openrouter.ai/api/v1", c.BaseURL)
		}
		if c.APIKey != "sk-or-default" {
			t.Errorf("APIKey = %q, want sk-or-default", c.APIKey)
		}
	})
	t.Run("default openrouter registered for nil config", func(t *testing.T) {
		reg := registry(nil)
		if _, ok := reg["openrouter"].(*openaicompat.Client); !ok {
			t.Fatalf("openrouter provider is %T, want *openaicompat.Client", reg["openrouter"])
		}
	})
	t.Run("config overrides the default openrouter entry", func(t *testing.T) {
		t.Setenv("CUSTOM_OR_KEY", "sk-custom-or")
		reg := registry(&config.Config{Providers: map[string]config.Provider{
			"openrouter": {
				Type:      config.TypeOpenAICompat,
				BaseURL:   "http://openrouter.proxy.internal",
				APIKeyEnv: "CUSTOM_OR_KEY",
			},
		}})
		c := reg["openrouter"].(*openaicompat.Client)
		if c.BaseURL != "http://openrouter.proxy.internal" {
			t.Errorf("BaseURL = %q, want overridden value", c.BaseURL)
		}
		if c.APIKey != "sk-custom-or" {
			t.Errorf("APIKey = %q, want sk-custom-or", c.APIKey)
		}
	})
	t.Run("minimal openrouter entry resolves identically without pre-applied defaults", func(t *testing.T) {
		// registry() must not silently depend on its caller (e.g.
		// config.LoadProject) already having run applyProviderDefaults on
		// cfg — a *config.Config built by hand, with a minimal openrouter
		// entry that leaves Type/BaseURL empty, must wire the exact same
		// adapter registry() would build from the fully-defaulted
		// equivalent, never a silently missing "openrouter" key.
		t.Setenv("CUSTOM_OR_KEY", "sk-custom-or")
		raw := &config.Config{Providers: map[string]config.Provider{
			"openrouter": {APIKeyEnv: "CUSTOM_OR_KEY"},
		}}
		reg := registry(raw)
		c, ok := reg["openrouter"].(*openaicompat.Client)
		if !ok {
			t.Fatalf("openrouter provider is %T, want *openaicompat.Client (silently misregistered)", reg["openrouter"])
		}
		if c.BaseURL != "https://openrouter.ai/api/v1" {
			t.Errorf("BaseURL = %q, want the default OpenRouter base URL", c.BaseURL)
		}
		if c.Family != "openrouter" {
			t.Errorf("Family = %q, want openrouter", c.Family)
		}
		if c.APIKey != "sk-custom-or" {
			t.Errorf("APIKey = %q, want sk-custom-or", c.APIKey)
		}

		// And confirm it's identical to the pre-defaulted case, not a
		// coincidentally-equal one-off.
		defaulted := &config.Config{Providers: map[string]config.Provider{
			"openrouter": {APIKeyEnv: "CUSTOM_OR_KEY"},
		}}
		config.EnsureProviderDefaults(defaulted.Providers)
		want := registry(defaulted)["openrouter"].(*openaicompat.Client)
		want.HTTPClient, c.HTTPClient = nil, nil // both default to http.DefaultClient; not comparable/meaningful here
		if !reflect.DeepEqual(c, want) {
			t.Errorf("registry(raw) openrouter = %+v, want %+v (identical to pre-defaulted)", c, want)
		}
	})
	t.Run("a valid non-openrouter key does not suppress the openrouter default", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-or-still-default")
		reg := registry(&config.Config{Providers: map[string]config.Provider{
			"mycompat": {Type: config.TypeOpenAICompat, BaseURL: "http://compat.example"},
		}})
		c, ok := reg["openrouter"].(*openaicompat.Client)
		if !ok {
			t.Fatalf("openrouter provider is %T, want *openaicompat.Client", reg["openrouter"])
		}
		if c.BaseURL != "https://openrouter.ai/api/v1" {
			t.Errorf("BaseURL = %q, want the default OpenRouter base URL", c.BaseURL)
		}
		if c.APIKey != "sk-or-still-default" {
			t.Errorf("APIKey = %q, want sk-or-still-default", c.APIKey)
		}
	})
}

// TestRegistryOpenAICompatHitsConfiguredBaseURL proves a config-declared
// openai-compat entry resolves a ModelRef through the registry and actually
// talks to its configured base URL — with the auth header derived from the
// configured env var and any extra_headers attached.
func TestRegistryOpenAICompatHitsConfiguredBaseURL(t *testing.T) {
	var gotAuth, gotReferer, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	t.Setenv("FAKE_PROVIDER_KEY", "sk-fake-123")
	cfg := &config.Config{Providers: map[string]config.Provider{
		"fakecompat": {
			Type:         config.TypeOpenAICompat,
			BaseURL:      srv.URL,
			APIKeyEnv:    "FAKE_PROVIDER_KEY",
			ExtraHeaders: map[string]string{"HTTP-Referer": "https://harness.example"},
		},
	}}
	reg := registry(cfg)

	ref, err := message.ParseModelRef("fakecompat/some-model")
	if err != nil {
		t.Fatalf("ParseModelRef: %v", err)
	}
	p, err := reg.For(ref)
	if err != nil {
		t.Fatalf("reg.For: %v", err)
	}
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model: ref,
		Messages: []message.Message{
			{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hello"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	if gotPath != "/chat/completions" {
		t.Errorf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-fake-123" {
		t.Errorf("Authorization = %q, want Bearer sk-fake-123", gotAuth)
	}
	if gotReferer != "https://harness.example" {
		t.Errorf("HTTP-Referer = %q, want https://harness.example", gotReferer)
	}
}

// scriptedProvider returns one pre-built assistant turn per call and records
// the request it was given, mirroring the request-inspection pattern used in
// engine and server tests.
type scriptedProvider struct {
	name     string
	requests []*provider.Request
}

func (p *scriptedProvider) Name() string { return p.name }

func (p *scriptedProvider) Stream(_ context.Context, req *provider.Request) (provider.Stream, error) {
	p.requests = append(p.requests, req)
	return &scriptedStream{msg: &message.Message{
		ID: "msg_a", Role: message.RoleAssistant,
		Parts: message.Parts{&message.Text{Text: "done"}},
	}}, nil
}

type scriptedStream struct {
	msg  *message.Message
	sent bool
}

func (s *scriptedStream) Next() (provider.Event, error) {
	if s.sent {
		return provider.Event{}, io.EOF
	}
	s.sent = true
	return provider.Event{Type: provider.EventDone, Message: s.msg, StopReason: provider.StopEndTurn}, nil
}

func (s *scriptedStream) Close() error { return nil }

// TestNewSessionFnSystemUsesSessionWorkDir verifies the review finding: a
// served session created with an explicit workdir must get a system prompt
// naming THAT workdir, not the process cwd baked into mkCfg's base cfg.
func TestNewSessionFnSystemUsesSessionWorkDir(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	processCwd := t.TempDir()
	sessionWorkDir := t.TempDir()
	if processCwd == sessionWorkDir {
		t.Fatal("test setup: dirs must differ")
	}
	model := message.ModelRef{Provider: "test", Model: "m1"}
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       systemPrompt(processCwd, ""),
			WorkDir:      processCwd,
			SessionDir:   t.TempDir(),
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
		}
	}
	var gotReq *provider.Request
	onRequest := func(_ string, _ int, req *provider.Request) { gotReq = req }

	newSession := newSessionFn(mkCfg, model, &config.Config{SkillsDirs: []string{}}, nil, onRequest)
	sess, err := newSession(message.ModelRef{}, sessionWorkDir)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if gotReq == nil {
		t.Fatal("onRequest never fired")
	}
	if len(gotReq.System) == 0 || !strings.Contains(gotReq.System[0], "Working directory: "+sessionWorkDir) {
		t.Errorf("system = %v, want it to name session workdir %q", gotReq.System, sessionWorkDir)
	}
	for _, seg := range gotReq.System {
		if strings.Contains(seg, "Working directory: "+processCwd) {
			t.Errorf("system = %v, must not name the process cwd %q", gotReq.System, processCwd)
		}
	}
}

// writeMainTestSkill creates a skill directory <root>/<name> with a minimal
// valid SKILL.md, mirroring engine's writeSkill helper.
func writeMainTestSkill(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nDo the thing.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNewSessionFnSkillsUsesSessionWorkDir is the RED test for the review
// finding: mkCfg computes cfg.SkillsDirs via skillsDirs(cfg, flagDirs,
// processCwd) — exactly as serveCmd's real mkCfg does — so a relative
// skills_dirs entry is baked against the process cwd. A served session
// created with an explicit sessionWorkDir must still discover a skill placed
// under sessionWorkDir (where the relative entry actually resolves for that
// session), not silently miss it because newSessionFn never re-resolves
// SkillsDirs the way it already re-resolves System.
func TestNewSessionFnSkillsUsesSessionWorkDir(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	processCwd := t.TempDir()
	sessionWorkDir := t.TempDir()
	if processCwd == sessionWorkDir {
		t.Fatal("test setup: dirs must differ")
	}
	// A relative skills_dirs entry, configured the same way for every
	// session; it must resolve against each session's own workdir.
	writeMainTestSkill(t, filepath.Join(sessionWorkDir, "skills"), "brewing", "Make coffee")

	model := message.ModelRef{Provider: "test", Model: "m1"}
	appCfg := &config.Config{SkillsDirs: []string{"skills"}}
	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       systemPrompt(processCwd, ""),
			WorkDir:      processCwd,
			SessionDir:   t.TempDir(),
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   skillsDirs(appCfg, nil, processCwd),
		}
	}
	var gotReq *provider.Request
	onRequest := func(_ string, _ int, req *provider.Request) { gotReq = req }

	newSession := newSessionFn(mkCfg, model, appCfg, nil, onRequest)
	sess, err := newSession(message.ModelRef{}, sessionWorkDir)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	if _, err := sess.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if gotReq == nil {
		t.Fatal("onRequest never fired")
	}
	found := false
	for _, seg := range gotReq.System {
		if strings.Contains(seg, "brewing") {
			found = true
		}
	}
	if !found {
		t.Errorf("system = %v, want it to discover the skill under sessionWorkDir %q, not processCwd %q", gotReq.System, sessionWorkDir, processCwd)
	}
}

// TestLoadSessionFnSkillsUsesRestoredWorkDir is the RED test for the same
// finding on the resume path: the durable WorkDir restored from the session
// log header must drive skills_dirs resolution, not the process cwd baked
// into mkCfg's base cfg.SkillsDirs.
func TestLoadSessionFnSkillsUsesRestoredWorkDir(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	processCwd := t.TempDir()
	sessionWorkDir := t.TempDir()
	if processCwd == sessionWorkDir {
		t.Fatal("test setup: dirs must differ")
	}
	writeMainTestSkill(t, filepath.Join(sessionWorkDir, "skills"), "brewing", "Make coffee")

	sesDir := t.TempDir()
	model := message.ModelRef{Provider: "test", Model: "m1"}
	appCfg := &config.Config{SkillsDirs: []string{"skills"}}

	// Create and persist a session whose durable WorkDir is sessionWorkDir —
	// mirroring a session originally created via newSessionFn above.
	orig := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": prov},
		Model:        model,
		WorkDir:      sessionWorkDir,
		SessionDir:   sesDir,
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   skillsDirs(appCfg, nil, sessionWorkDir),
	})
	if err := orig.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       systemPrompt(processCwd, ""),
			WorkDir:      processCwd,
			SessionDir:   sesDir,
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   skillsDirs(appCfg, nil, processCwd),
		}
	}
	var gotReq *provider.Request
	onRequest := func(_ string, _ int, req *provider.Request) { gotReq = req }

	loadSession := loadSessionFn(mkCfg, model, appCfg, nil, onRequest)
	sess, err := loadSession(orig.ID)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got := sess.WorkDir(); got != sessionWorkDir {
		t.Fatalf("test setup: sess.WorkDir() = %q, want %q", got, sessionWorkDir)
	}
	if _, err := sess.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if gotReq == nil {
		t.Fatal("onRequest never fired")
	}
	found := false
	for _, seg := range gotReq.System {
		if strings.Contains(seg, "brewing") {
			found = true
		}
	}
	if !found {
		t.Errorf("system = %v, want it to discover the skill under restored sessionWorkDir %q, not processCwd %q", gotReq.System, sessionWorkDir, processCwd)
	}
}

// TestLoadSessionFnSystemUsesRestoredWorkDir verifies the same finding for a
// resumed session: the durable WorkDir restored from the session log header
// must drive the system prompt, not the process cwd baked into mkCfg's base
// cfg (used because the workdir isn't known until after the log is read).
func TestLoadSessionFnSystemUsesRestoredWorkDir(t *testing.T) {
	prov := &scriptedProvider{name: "test"}
	processCwd := t.TempDir()
	sessionWorkDir := t.TempDir()
	if processCwd == sessionWorkDir {
		t.Fatal("test setup: dirs must differ")
	}
	sesDir := t.TempDir()
	model := message.ModelRef{Provider: "test", Model: "m1"}

	// Create and persist a session whose durable WorkDir is sessionWorkDir —
	// mirroring a session originally created via newSessionFn above.
	orig := engine.NewSession(engine.Config{
		Providers:    provider.Registry{"test": prov},
		Model:        model,
		WorkDir:      sessionWorkDir,
		SessionDir:   sesDir,
		Instructions: &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:   []string{},
	})
	if err := orig.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	mkCfg := func(m message.ModelRef) engine.Config {
		return engine.Config{
			Providers:    provider.Registry{"test": prov},
			Model:        m,
			System:       systemPrompt(processCwd, ""),
			WorkDir:      processCwd,
			SessionDir:   sesDir,
			Instructions: &engine.InstructionsConfig{Disabled: true},
			SkillsDirs:   []string{},
		}
	}
	var gotReq *provider.Request
	onRequest := func(_ string, _ int, req *provider.Request) { gotReq = req }

	loadSession := loadSessionFn(mkCfg, model, &config.Config{SkillsDirs: []string{}}, nil, onRequest)
	sess, err := loadSession(orig.ID)
	if err != nil {
		t.Fatalf("loadSession: %v", err)
	}
	if got := sess.WorkDir(); got != sessionWorkDir {
		t.Fatalf("test setup: sess.WorkDir() = %q, want %q", got, sessionWorkDir)
	}
	if _, err := sess.Prompt(context.Background(), "hi"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if gotReq == nil {
		t.Fatal("onRequest never fired")
	}
	if len(gotReq.System) == 0 || !strings.Contains(gotReq.System[0], "Working directory: "+sessionWorkDir) {
		t.Errorf("system = %v, want it to name the restored session workdir %q", gotReq.System, sessionWorkDir)
	}
	for _, seg := range gotReq.System {
		if strings.Contains(seg, "Working directory: "+processCwd) {
			t.Errorf("system = %v, must not name the process cwd %q", gotReq.System, processCwd)
		}
	}
}
