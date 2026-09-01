package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadProjectAppendSystemPrompt(t *testing.T) {
	user := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, user, `{"append_system_prompt":["platform"]}`)
	t.Setenv("HARNESS_CONFIG", user)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".harness.json"), `{"append_system_prompt":["project"]}`)

	cfg, err := LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"platform", "project"}; !reflect.DeepEqual(cfg.AppendSystemPrompt, want) {
		t.Errorf("AppendSystemPrompt = %v, want %v", cfg.AppendSystemPrompt, want)
	}
}

func TestMergeAppendSystemPromptDoesNotAlias(t *testing.T) {
	base := &Config{AppendSystemPrompt: []string{"platform"}}
	over := &Config{AppendSystemPrompt: []string{"project"}}
	got := merge(base, over)
	got.AppendSystemPrompt[0], got.AppendSystemPrompt[1] = "x", "y"
	if base.AppendSystemPrompt[0] != "platform" || over.AppendSystemPrompt[0] != "project" {
		t.Fatalf("merge aliased its inputs: base=%v over=%v", base.AppendSystemPrompt, over.AppendSystemPrompt)
	}
}

func TestLoadProjectRejectsPromptReplacementExtraArg(t *testing.T) {
	user := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, user, `{
		"append_system_prompt":["platform"],
		"providers":{"claude-code":{"type":"claude-code-cli"}}
	}`)
	t.Setenv("HARNESS_CONFIG", user)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".harness.json"), `{
		"providers":{"claude-code":{"extra_args":["--append-system-prompt","project"]}}
	}`)

	_, err := LoadProject(dir)
	if err == nil {
		t.Fatal("LoadProject accepted conflicting extra_args")
	}
	for _, want := range []string{"append_system_prompt", "providers.claude-code.extra_args"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
