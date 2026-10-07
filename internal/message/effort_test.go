package message

import "testing"

func TestParseEffort(t *testing.T) {
	cases := []struct {
		in      string
		want    Effort
		wantErr bool
	}{
		{"", EffortUnset, false},
		{"off", EffortOff, false},
		{"minimal", EffortMinimal, false},
		{"low", EffortLow, false},
		{"medium", EffortMedium, false},
		{"high", EffortHigh, false},
		{"MEDIUM", EffortUnset, true}, // case-sensitive
		{"xhigh", EffortUnset, true},
		{"none", EffortUnset, true},
		{"true", EffortUnset, true},
	}
	for _, c := range cases {
		got, err := ParseEffort(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseEffort(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEffort(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseEffort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEffortReasoning(t *testing.T) {
	reasoning := []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh}
	for _, e := range reasoning {
		if !e.Reasoning() {
			t.Errorf("Effort(%q).Reasoning() = false, want true", e)
		}
	}
	for _, e := range []Effort{EffortUnset, EffortOff} {
		if e.Reasoning() {
			t.Errorf("Effort(%q).Reasoning() = true, want false", e)
		}
	}
}

func TestEffortIsZero(t *testing.T) {
	if !EffortUnset.IsZero() {
		t.Error("EffortUnset.IsZero() = false")
	}
	for _, e := range []Effort{EffortOff, EffortMinimal, EffortLow, EffortMedium, EffortHigh} {
		if e.IsZero() {
			t.Errorf("Effort(%q).IsZero() = true", e)
		}
	}
}
