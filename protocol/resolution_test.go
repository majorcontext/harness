package protocol

import (
	"encoding/json"
	"testing"
)

func TestResolutionValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Resolution
		ok   bool
	}{
		{"answer", Resolution{Answer: json.RawMessage(`{"q":"a"}`)}, true},
		{"dismiss", Resolution{Dismiss: true}, true},
		{"dismiss with null", Resolution{Dismiss: true, Answer: json.RawMessage(` null `)}, true},
		{"dismiss with empty object", Resolution{Dismiss: true, Answer: json.RawMessage(`{}`)}, true},
		{"dismiss with answer", Resolution{Dismiss: true, Answer: json.RawMessage(`"x"`)}, false},
		{"neither", Resolution{}, false},
		{"null answer", Resolution{Answer: json.RawMessage(`null`)}, false},
		{"empty string answer", Resolution{Answer: json.RawMessage(`""`)}, false},
		{"invalid json", Resolution{Answer: json.RawMessage(`{`)}, false},
	} {
		if err := tc.r.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
