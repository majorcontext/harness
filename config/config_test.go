package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load missing file: %v", err)
	}
	if c == nil {
		t.Fatal("Load returned nil config for missing file")
	}
	if c.Model != "" || len(c.Aliases) != 0 || c.SessionDir != "" || len(c.Providers) != 0 {
		t.Errorf("missing file gave non-zero config: %+v", c)
	}
}

func TestLoadBasic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	writeFile(t, p, `{
		"model": "anthropic/claude-fable-5",
		"aliases": {"fast": "anthropic/claude-haiku-4-5"},
		"providers": {"anthropic": {"api_key_env": "MY_KEY", "base_url": "http://x"}}
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Model != "anthropic/claude-fable-5" {
		t.Errorf("Model = %q", c.Model)
	}
	if c.Aliases["fast"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("alias fast = %q", c.Aliases["fast"])
	}
	if c.Providers["anthropic"].APIKeyEnv != "MY_KEY" || c.Providers["anthropic"].BaseURL != "http://x" {
		t.Errorf("provider anthropic = %+v", c.Providers["anthropic"])
	}
}

func TestLoadUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"modle": "typo"}`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name path %q", err, p)
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{not json`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name path %q", err, p)
	}
}

func TestLoadExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, p, `{"session_dir": "~/custom/sessions"}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "custom", "sessions")
	if c.SessionDir != want {
		t.Errorf("SessionDir = %q, want %q", c.SessionDir, want)
	}
}

func TestPath(t *testing.T) {
	t.Run("HARNESS_CONFIG wins", func(t *testing.T) {
		t.Setenv("HARNESS_CONFIG", "/etc/harness.json")
		if got := Path(); got != "/etc/harness.json" {
			t.Errorf("Path() = %q, want /etc/harness.json", got)
		}
	})
	t.Run("defaults to HOME/.harness/config.json", func(t *testing.T) {
		t.Setenv("HARNESS_CONFIG", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".harness", "config.json")
		if got := Path(); got != want {
			t.Errorf("Path() = %q, want %q", got, want)
		}
	})
}

func TestResolveModel(t *testing.T) {
	t.Run("empty falls back to config model", func(t *testing.T) {
		c := &Config{Model: "anthropic/claude-opus-4-8"}
		ref, err := c.ResolveModel("")
		if err != nil {
			t.Fatalf("ResolveModel: %v", err)
		}
		if ref.String() != "anthropic/claude-opus-4-8" {
			t.Errorf("ref = %q", ref)
		}
	})
	t.Run("empty and no config model falls back to hard default", func(t *testing.T) {
		c := &Config{}
		ref, err := c.ResolveModel("")
		if err != nil {
			t.Fatalf("ResolveModel: %v", err)
		}
		if ref.String() != DefaultModel {
			t.Errorf("ref = %q, want %q", ref, DefaultModel)
		}
	})
	t.Run("alias resolves one level", func(t *testing.T) {
		c := &Config{Aliases: map[string]string{"fast": "anthropic/claude-haiku-4-5"}}
		ref, err := c.ResolveModel("fast")
		if err != nil {
			t.Fatalf("ResolveModel: %v", err)
		}
		if ref.String() != "anthropic/claude-haiku-4-5" {
			t.Errorf("ref = %q", ref)
		}
	})
	t.Run("config model may itself be an alias", func(t *testing.T) {
		c := &Config{Model: "smart", Aliases: map[string]string{"smart": "anthropic/claude-fable-5"}}
		ref, err := c.ResolveModel("")
		if err != nil {
			t.Fatalf("ResolveModel: %v", err)
		}
		if ref.String() != "anthropic/claude-fable-5" {
			t.Errorf("ref = %q", ref)
		}
	})
	t.Run("unknown alias errors", func(t *testing.T) {
		c := &Config{}
		if _, err := c.ResolveModel("nope"); err == nil {
			t.Error("expected error for unknown alias / bare name")
		}
	})
	t.Run("aliases do not recurse", func(t *testing.T) {
		c := &Config{Aliases: map[string]string{"a": "b", "b": "anthropic/claude-fable-5"}}
		if _, err := c.ResolveModel("a"); err == nil {
			t.Error("expected error: alias should resolve one level only, not recurse")
		}
	})
	t.Run("explicit ref passes through", func(t *testing.T) {
		c := &Config{}
		ref, err := c.ResolveModel("openai/gpt-5")
		if err != nil {
			t.Fatalf("ResolveModel: %v", err)
		}
		if ref.String() != "openai/gpt-5" {
			t.Errorf("ref = %q", ref)
		}
	})
}

func TestLoadInstructionsFields(t *testing.T) {
	t.Run("instructions false and path", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"instructions": false, "instructions_path": "docs/AGENTS.md"}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Instructions == nil || *c.Instructions != false {
			t.Errorf("Instructions = %v, want *false", c.Instructions)
		}
		if c.InstructionsPath != "docs/AGENTS.md" {
			t.Errorf("InstructionsPath = %q", c.InstructionsPath)
		}
	})
	t.Run("unset leaves nil", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"model": "anthropic/claude-fable-5"}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.Instructions != nil {
			t.Errorf("Instructions = %v, want nil (unset)", c.Instructions)
		}
		if c.InstructionsPath != "" {
			t.Errorf("InstructionsPath = %q, want empty", c.InstructionsPath)
		}
	})
}

func TestLoadSkillsDirs(t *testing.T) {
	t.Run("array parsed", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"skills_dirs": ["a/skills", "b/skills"]}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(c.SkillsDirs) != 2 || c.SkillsDirs[0] != "a/skills" || c.SkillsDirs[1] != "b/skills" {
			t.Errorf("SkillsDirs = %v", c.SkillsDirs)
		}
	})
	t.Run("unset leaves nil", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"model": "anthropic/claude-fable-5"}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.SkillsDirs != nil {
			t.Errorf("SkillsDirs = %v, want nil (unset)", c.SkillsDirs)
		}
	})
}

func TestMergeSkillsDirs(t *testing.T) {
	t.Run("non-empty project overrides user entirely", func(t *testing.T) {
		base := &Config{SkillsDirs: []string{"user/a", "user/b"}}
		over := &Config{SkillsDirs: []string{"proj/x"}}
		merged := merge(base, over)
		if len(merged.SkillsDirs) != 1 || merged.SkillsDirs[0] != "proj/x" {
			t.Errorf("merged SkillsDirs = %v, want [proj/x]", merged.SkillsDirs)
		}
	})
	t.Run("unset project inherits user", func(t *testing.T) {
		base := &Config{SkillsDirs: []string{"user/a"}}
		merged := merge(base, &Config{})
		if len(merged.SkillsDirs) != 1 || merged.SkillsDirs[0] != "user/a" {
			t.Errorf("merged SkillsDirs = %v, want inherited [user/a]", merged.SkillsDirs)
		}
	})
	t.Run("empty project slice inherits user (only non-empty overrides)", func(t *testing.T) {
		base := &Config{SkillsDirs: []string{"user/a"}}
		merged := merge(base, &Config{SkillsDirs: []string{}})
		if len(merged.SkillsDirs) != 1 || merged.SkillsDirs[0] != "user/a" {
			t.Errorf("merged SkillsDirs = %v, want inherited [user/a]", merged.SkillsDirs)
		}
	})
	t.Run("does not alias base slice", func(t *testing.T) {
		base := &Config{SkillsDirs: []string{"user/a"}}
		merged := merge(base, &Config{})
		merged.SkillsDirs[0] = "mutated"
		if base.SkillsDirs[0] != "user/a" {
			t.Errorf("base SkillsDirs mutated through merged config: %v", base.SkillsDirs)
		}
	})
}

func TestMergeInstructions(t *testing.T) {
	trueV, falseV := true, false
	t.Run("project overrides user", func(t *testing.T) {
		base := &Config{Instructions: &trueV, InstructionsPath: "user/AGENTS.md"}
		over := &Config{Instructions: &falseV, InstructionsPath: "proj/AGENTS.md"}
		merged := merge(base, over)
		if merged.Instructions == nil || *merged.Instructions != false {
			t.Errorf("merged Instructions = %v, want *false", merged.Instructions)
		}
		if merged.InstructionsPath != "proj/AGENTS.md" {
			t.Errorf("merged InstructionsPath = %q, want proj/AGENTS.md", merged.InstructionsPath)
		}
	})
	t.Run("unset project inherits user", func(t *testing.T) {
		base := &Config{Instructions: &trueV, InstructionsPath: "user/AGENTS.md"}
		merged := merge(base, &Config{})
		if merged.Instructions == nil || *merged.Instructions != true {
			t.Errorf("merged Instructions = %v, want *true (inherited)", merged.Instructions)
		}
		if merged.InstructionsPath != "user/AGENTS.md" {
			t.Errorf("merged InstructionsPath = %q, want inherited user/AGENTS.md", merged.InstructionsPath)
		}
	})
}

func TestGoalEvaluatorModel(t *testing.T) {
	t.Run("load", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, p, `{"goal_evaluator_model": "anthropic/claude-opus-4-8"}`)
		c, err := Load(p)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.GoalEvaluatorModel != "anthropic/claude-opus-4-8" {
			t.Errorf("GoalEvaluatorModel = %q", c.GoalEvaluatorModel)
		}
	})
	t.Run("project overrides user", func(t *testing.T) {
		base := &Config{GoalEvaluatorModel: "anthropic/user-model"}
		merged := merge(base, &Config{GoalEvaluatorModel: "anthropic/proj-model"})
		if merged.GoalEvaluatorModel != "anthropic/proj-model" {
			t.Errorf("merged = %q, want proj-model", merged.GoalEvaluatorModel)
		}
	})
	t.Run("unset project inherits user", func(t *testing.T) {
		base := &Config{GoalEvaluatorModel: "anthropic/user-model"}
		merged := merge(base, &Config{})
		if merged.GoalEvaluatorModel != "anthropic/user-model" {
			t.Errorf("merged = %q, want inherited user-model", merged.GoalEvaluatorModel)
		}
	})
	t.Run("resolves through aliases", func(t *testing.T) {
		c := &Config{Aliases: map[string]string{"judge": "anthropic/claude-opus-4-8"}}
		ref, err := c.ResolveModel("judge")
		if err != nil {
			t.Fatal(err)
		}
		if ref.String() != "anthropic/claude-opus-4-8" {
			t.Errorf("ResolveModel(judge) = %q", ref.String())
		}
	})
}

func TestMergeDoesNotAliasBaseMaps(t *testing.T) {
	base := &Config{
		Aliases:   map[string]string{"fast": "anthropic/claude-haiku-4-5"},
		Providers: map[string]Provider{"anthropic": {APIKeyEnv: "USER_KEY"}},
	}
	// An override contributing no map entries must still yield fresh maps.
	merged := merge(base, &Config{Model: "openai/gpt-5"})
	merged.Aliases["fast"] = "mutated"
	merged.Aliases["new"] = "added"
	merged.Providers["anthropic"] = Provider{APIKeyEnv: "MUTATED"}
	merged.Providers["openai"] = Provider{APIKeyEnv: "ADDED"}

	if base.Aliases["fast"] != "anthropic/claude-haiku-4-5" {
		t.Errorf("base alias fast = %q, mutated through merged config", base.Aliases["fast"])
	}
	if _, ok := base.Aliases["new"]; ok {
		t.Error("base aliases gained a key added to the merged config")
	}
	if base.Providers["anthropic"].APIKeyEnv != "USER_KEY" {
		t.Errorf("base provider anthropic = %+v, mutated through merged config", base.Providers["anthropic"])
	}
	if _, ok := base.Providers["openai"]; ok {
		t.Error("base providers gained a key added to the merged config")
	}
}

func TestLoadProject(t *testing.T) {
	t.Run("no project file returns user config", func(t *testing.T) {
		userPath := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, userPath, `{"model": "anthropic/claude-fable-5"}`)
		t.Setenv("HARNESS_CONFIG", userPath)
		c, err := LoadProject(t.TempDir())
		if err != nil {
			t.Fatalf("LoadProject: %v", err)
		}
		if c.Model != "anthropic/claude-fable-5" {
			t.Errorf("Model = %q", c.Model)
		}
	})
	t.Run("project non-zero fields override user config", func(t *testing.T) {
		userPath := filepath.Join(t.TempDir(), "config.json")
		writeFile(t, userPath, `{
			"model": "anthropic/claude-fable-5",
			"session_dir": "/user/sessions",
			"aliases": {"fast": "anthropic/claude-haiku-4-5", "smart": "anthropic/claude-opus-4-8"},
			"providers": {"anthropic": {"api_key_env": "USER_KEY", "base_url": "http://user"}}
		}`)
		t.Setenv("HARNESS_CONFIG", userPath)
		projDir := t.TempDir()
		writeFile(t, filepath.Join(projDir, ".harness.json"), `{
			"model": "openai/gpt-5",
			"aliases": {"smart": "openai/gpt-5-pro"},
			"providers": {"anthropic": {"base_url": "http://project"}}
		}`)
		c, err := LoadProject(projDir)
		if err != nil {
			t.Fatalf("LoadProject: %v", err)
		}
		if c.Model != "openai/gpt-5" {
			t.Errorf("Model = %q, want openai/gpt-5 (project override)", c.Model)
		}
		if c.SessionDir != "/user/sessions" {
			t.Errorf("SessionDir = %q, want /user/sessions (unset in project)", c.SessionDir)
		}
		if c.Aliases["fast"] != "anthropic/claude-haiku-4-5" {
			t.Errorf("alias fast = %q, want inherited from user", c.Aliases["fast"])
		}
		if c.Aliases["smart"] != "openai/gpt-5-pro" {
			t.Errorf("alias smart = %q, want project override", c.Aliases["smart"])
		}
		got := c.Providers["anthropic"]
		if got.APIKeyEnv != "USER_KEY" {
			t.Errorf("anthropic api_key_env = %q, want inherited USER_KEY", got.APIKeyEnv)
		}
		if got.BaseURL != "http://project" {
			t.Errorf("anthropic base_url = %q, want project override", got.BaseURL)
		}
	})
}
