package main

import "testing"

// TestToolConcurrencyKnobs pins the operator kill switch. The engine never
// reads an environment variable, so this function is the only place the
// two knobs become a value — and engine.Config.ToolConcurrency 1 is what
// restores strictly sequential tool execution.
func TestToolConcurrencyKnobs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sequential string
		cap        string
		want       int
	}{
		{"unset leaves the engine default", "", "", 0},
		{"sequential kill switch wins", "1", "16", 1},
		{"cap is honored", "", "4", 4},
		{"a non-one sequential value is ignored", "0", "4", 4},
		{"a bad cap falls back to the engine default", "", "nope", 0},
		{"a non-positive cap falls back to the engine default", "", "0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_SEQUENTIAL_TOOLS", tc.sequential)
			t.Setenv("HARNESS_TOOL_CONCURRENCY", tc.cap)
			if got := toolConcurrency(); got != tc.want {
				t.Errorf("toolConcurrency() = %d, want %d", got, tc.want)
			}
		})
	}
}
