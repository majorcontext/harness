package engine

import (
	"context"
	"strings"
	"testing"
)

func TestAppendSystemPromptNativeOrder(t *testing.T) {
	system := batchingSystem(t, Config{
		System:             []string{"base"},
		AppendSystemPrompt: []string{"platform", "project"},
	})
	if len(system) != 4 {
		t.Fatalf("system = %v, want four segments", system)
	}
	if system[0] != "base" || system[1] != "platform" || system[2] != "project" || !isBatchingSegment(system[3]) {
		t.Errorf("unexpected system order: %v", system)
	}
}

func TestClaudeCodeAppendSystemPromptArgv(t *testing.T) {
	s, logPath := claudeCodeTestSession(t, "normal")
	s.cfg.System = []string{"native-only"}
	s.cfg.AppendSystemPrompt = []string{" first ", "gateway → --append-system-prompt"}

	if _, err := s.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	argv := readInvocations(t, logPath)[0]
	got, ok := argvValueAfter(argv, "--append-system-prompt")
	if !ok || got != " first \n\ngateway → --append-system-prompt" {
		t.Errorf("append arg = %q, %v; argv=%v", got, ok, argv)
	}
	var count int
	for _, arg := range argv {
		if arg == "--append-system-prompt" {
			count++
		}
		if strings.Contains(arg, "native-only") {
			t.Errorf("Config.System reached Claude Code argv: %q", arg)
		}
	}
	if count != 1 {
		t.Errorf("append flag count = %d, want 1", count)
	}
}

func TestClaudeCodeAppendSystemPromptValidation(t *testing.T) {
	tests := []struct {
		name string
		segs []string
		args []string
		want string
	}{
		{"prompt", []string{"platform"}, []string{"--append-system-prompt", "project"}, "ExtraArgs"},
		{"prompt equals", []string{"platform"}, []string{"--append-system-prompt=project"}, "ExtraArgs"},
		{"prompt file", []string{"platform"}, []string{"--append-system-prompt-file", "file"}, "ExtraArgs"},
		{"prompt file equals", []string{"platform"}, []string{"--append-system-prompt-file=file"}, "ExtraArgs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := claudeCodeTestSession(t, "normal")
			s.cfg.AppendSystemPrompt = tt.segs
			s.cfg.ClaudeCode.ExtraArgs = tt.args
			_, err := s.Prompt(context.Background(), "hi")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prompt error = %v, want text %q", err, tt.want)
			}
		})
	}
}
