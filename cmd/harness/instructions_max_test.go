package main

import (
	"testing"

	"github.com/majorcontext/harness/config"
)

// TestInstructionsMaxBytesKnob pins the instruction cap seam:
// HARNESS_INSTRUCTIONS_MAX_KB (kilobytes) overrides config
// `instructions_max_bytes` (bytes), and neither set leaves the engine
// default (a nil InstructionsConfig, or a zero MaxBytes) in place.
func TestInstructionsMaxBytesKnob(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		cfg   *config.Config
		want  int  // expected ic.MaxBytes
		nilOK bool // a nil InstructionsConfig is the expected result
	}{
		{name: "unset stays nil", cfg: &config.Config{}, nilOK: true},
		{name: "config bytes", cfg: &config.Config{InstructionsMaxBytes: 4096}, want: 4096},
		{name: "config disables the cap", cfg: &config.Config{InstructionsMaxBytes: -1}, want: -1},
		{name: "env kilobytes", env: "8", cfg: &config.Config{}, want: 8 * 1024},
		{name: "env wins over config", env: "8", cfg: &config.Config{InstructionsMaxBytes: 4096}, want: 8 * 1024},
		{name: "env disables the cap", env: "-1", cfg: &config.Config{InstructionsMaxBytes: 4096}, want: -1},
		{name: "malformed env falls back to config", env: "banana", cfg: &config.Config{InstructionsMaxBytes: 4096}, want: 4096},
		{name: "zero env falls back to config", env: "0", cfg: &config.Config{InstructionsMaxBytes: 4096}, want: 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_INSTRUCTIONS_MAX_KB", tc.env)
			ic := instructionsConfig(tc.cfg, false)
			if tc.nilOK {
				if ic != nil {
					t.Fatalf("ic = %+v, want nil (engine default)", ic)
				}
				return
			}
			if ic == nil {
				t.Fatalf("ic = nil, want MaxBytes %d", tc.want)
			}
			if ic.MaxBytes != tc.want {
				t.Errorf("ic.MaxBytes = %d, want %d", ic.MaxBytes, tc.want)
			}
			if ic.Disabled {
				t.Errorf("ic.Disabled = true, want instructions enabled")
			}
		})
	}
}

// TestInstructionsMaxBytesNilConfig verifies the operator knob still applies
// when no config file was loaded: a nil config means "no config", not "keep
// the default cap".
func TestInstructionsMaxBytesNilConfig(t *testing.T) {
	t.Setenv("HARNESS_INSTRUCTIONS_MAX_KB", "8")
	ic := instructionsConfig(nil, false)
	if ic == nil || ic.MaxBytes != 8*1024 {
		t.Fatalf("ic = %+v, want MaxBytes 8192", ic)
	}
	t.Setenv("HARNESS_INSTRUCTIONS_MAX_KB", "")
	if ic := instructionsConfig(nil, false); ic != nil {
		t.Fatalf("ic = %+v, want nil with no config and no env", ic)
	}
}

// TestInstructionsMaxBytesWithPathOverride verifies the cap rides the
// explicit-path branch too: a project that names its own instruction file
// still gets the configured cap.
func TestInstructionsMaxBytesWithPathOverride(t *testing.T) {
	t.Setenv("HARNESS_INSTRUCTIONS_MAX_KB", "")
	ic := instructionsConfig(&config.Config{InstructionsPath: "x/AGENTS.md", InstructionsMaxBytes: 4096}, false)
	if ic == nil || ic.Path != "x/AGENTS.md" || ic.MaxBytes != 4096 {
		t.Fatalf("ic = %+v, want path x/AGENTS.md with MaxBytes 4096", ic)
	}
}

// TestInstructionsMaxBytesNeverEnablesDisabled verifies -no-instructions and
// `instructions: false` still win: a cap must never re-enable injection.
func TestInstructionsMaxBytesNeverEnablesDisabled(t *testing.T) {
	t.Setenv("HARNESS_INSTRUCTIONS_MAX_KB", "8")
	falseV := false
	for _, tc := range []struct {
		name           string
		cfg            *config.Config
		noInstructions bool
	}{
		{"flag", &config.Config{InstructionsMaxBytes: 4096}, true},
		{"config false", &config.Config{Instructions: &falseV, InstructionsMaxBytes: 4096}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ic := instructionsConfig(tc.cfg, tc.noInstructions)
			if ic == nil || !ic.Disabled {
				t.Fatalf("ic = %+v, want disabled", ic)
			}
		})
	}
}
