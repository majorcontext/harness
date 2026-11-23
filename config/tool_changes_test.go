package config

import "testing"

func TestToolChangesDefaultsOnOnlyForTheDirectAPI(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Provider
		want bool
	}{
		{"direct", Provider{}, true},
		{"gateway", Provider{BaseURL: "https://gateway.test"}, false},
		{"gateway turned on", Provider{BaseURL: "https://gateway.test", ToolChanges: new(true)}, true},
		{"direct turned off", Provider{ToolChanges: new(false)}, false},
	} {
		if got := tc.p.ToolChangesOn(); got != tc.want {
			t.Errorf("%s: ToolChangesOn = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestToolChangesMergeAndClone(t *testing.T) {
	user := &Config{Providers: map[string]Provider{"anthropic": {ToolChanges: new(true)}}}
	project := &Config{Providers: map[string]Provider{"anthropic": {ToolChanges: new(false)}}}
	got, err := mergeAndValidate(user, project)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Providers["anthropic"]
	if p.ToolChanges == nil || *p.ToolChanges || p.Clone().ToolChanges == p.ToolChanges {
		t.Fatalf("merged tool_changes = %v, want false and a copy on clone", p.ToolChanges)
	}
}
