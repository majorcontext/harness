package message

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolChangeRoundTrip(t *testing.T) {
	in := Parts{&ToolChange{Removed: []string{"a"}, Added: []string{"b", "c"}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Parts
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip = %s, want %s", mustMarshal(t, out), raw)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
