package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyEnv(t *testing.T) {
	file := Config{SessionDir: "/file", Model: "a/b"}
	with := func(f func(*Config)) Config {
		c := file
		f(&c)
		return c
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{name: "a string key", env: map[string]string{"HARNESS_SESSION_DIR": "/env"}, want: with(func(c *Config) { c.SessionDir = "/env" })},
		{name: "an explicit zero int pointer", env: map[string]string{"HARNESS_PROMPT_RETRIES": "0"}, want: with(func(c *Config) { c.PromptRetries = new(0) })},
		{name: "an explicit false bool pointer", env: map[string]string{"HARNESS_CONTEXT_WINDOW_REQUIRED": "false"}, want: with(func(c *Config) { c.ContextWindowRequired = new(false) })},
		{name: "a task limit", env: map[string]string{"HARNESS_MAX_TASK_DEPTH": "2"}, want: with(func(c *Config) { c.MaxTaskDepth = 2 })},
		{name: "a tree token budget", env: map[string]string{"HARNESS_MAX_TREE_TOKENS": "500"}, want: with(func(c *Config) { c.MaxTreeTokens = 500 })},
		{name: "a float", env: map[string]string{"HARNESS_COMPACTION_THRESHOLD": "0.5"}, want: with(func(c *Config) { c.CompactionThreshold = 0.5 })},
		{name: "an empty value keeps the file value", env: map[string]string{"HARNESS_SESSION_DIR": ""}, want: file},
		{name: "map and non-field variables", env: map[string]string{"HARNESS_MCP_SERVERS": `{"m":{}}`, "HARNESS_CONFIG": "/c.json"}, want: file},
		{name: "a malformed integer", env: map[string]string{"HARNESS_PROMPT_RETRIES": "x"}, want: file, wantErr: "HARNESS_PROMPT_RETRIES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := file
			err := got.ApplyEnv(func(k string) string { return tc.env[k] })
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ApplyEnv() = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), "x")):
				t.Fatalf("ApplyEnv() = %v, want an error naming %s without the value", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ApplyEnv() config = %+v, want %+v", got, tc.want)
			}
		})
	}
}
