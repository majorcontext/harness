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
		{"a zero cap falls back to the engine default", "", "0", 0},
		// A negative value must reach the engine's documented clamp
		// (Config.ToolConcurrency: "clamped to 1 (sequential)"). envInt
		// alone folds it to 0, which would silently give parallel-at-8 to
		// an operator who asked for the opposite.
		{"a negative cap means sequential", "", "-1", 1},
		{"a large negative cap means sequential", "", "-5", 1},
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

// TestToolReadBudgetKnob pins the HARNESS_TOOL_READ_BUDGET_MB seam. The
// engine's own default is safe, so the important cases are "unset leaves
// the engine default" and "an explicit negative disables the bound".
func TestToolReadBudgetKnob(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name string
		env  string
		want int64
	}{
		{"unset leaves the engine default", "", 0},
		{"a positive value is megabytes", "128", 128 * mib},
		{"one megabyte", "1", mib},
		{"zero leaves the engine default", "0", 0},
		{"a negative value disables the bound", "-1", -1},
		{"any negative value normalizes to -1", "-4096", -1},
		{"a malformed value falls back to the default", "lots", 0},
		{"an absurd value falls back to the default", "99999999999999999", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_TOOL_READ_BUDGET_MB", tc.env)
			if got := toolReadBudgetBytes(); got != tc.want {
				t.Errorf("toolReadBudgetBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}
