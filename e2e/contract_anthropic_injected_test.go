package e2e

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// recordInjectedRequests records the credential headers of each request that the injecting fake saw.
type recordInjectedRequests struct{}

func (recordInjectedRequests) run(t *testing.T, r *run) {
	raw, err := json.Marshal(map[string]any{"requests": r.fake.InjectedRequests()})
	if err != nil {
		t.Fatal(err)
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	r.record(t, "injected_requests", "", callResult{Status: 200, Body: body})
}

// recordLastTurn records how the last turn of a session ended, with the text of its error.
type recordLastTurn struct{ as string }

func (a recordLastTurn) run(t *testing.T, r *run) {
	state := map[string]any{}
	if body, ok := r.drv.GetSession(t, r.id(t, a.as)).Body.(map[string]any); ok {
		if lt, ok := body["last_turn"].(map[string]any); ok {
			state["stop_reason"], state["error"] = lt["stop_reason"], lt["error"]
		}
	}
	r.record(t, "last_turn", a.as, callResult{Status: 200, Body: state})
}

func anthropicInjectedRows() []scenario {
	entry := map[string]any{"api_key_env": nil, "auth": "injected", "login_url": "https://console.example/settings/anthropic"}
	turn := []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	step := []harnesstest.Step{{Name: "reply", Repeat: true, Reply: harnesstest.Reply{Text: "hi"}}}
	return []scenario{
		{
			name:    "anthropic_injected_request_carries_no_credential_header",
			model:   step,
			entry:   entry,
			inject:  &harnesstest.InjectedOptions{},
			actions: append(append([]action{}, turn...), recordInjectedRequests{}),
		},
		{
			name:    "anthropic_injected_extra_headers_pass_through_as_placeholders",
			model:   step,
			entry:   map[string]any{"api_key_env": nil, "auth": "injected", "extra_headers": map[string]string{"x-api-key": "placeholder-key"}},
			inject:  &harnesstest.InjectedOptions{},
			actions: append(append([]action{}, turn...), recordInjectedRequests{}),
		},
		{
			name:    "anthropic_injected_401_fails_the_turn_with_the_login_url",
			model:   step,
			entry:   entry,
			inject:  &harnesstest.InjectedOptions{SignedOut: true},
			actions: append(append([]action{}, turn...), recordLastTurn{as: "a"}),
		},
		{
			name:    "anthropic_injected_403_is_not_login_required",
			model:   step,
			entry:   entry,
			inject:  &harnesstest.InjectedOptions{SignedOut: true, DenyStatus: 403},
			actions: append(append([]action{}, turn...), recordLastTurn{as: "a"}),
		},
	}
}
