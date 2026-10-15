package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// injectedEntry is the codex entry of a deployment whose server adds the credential.
func injectedEntry(extra map[string]any) map[string]any {
	e := map[string]any{"api_key_env": nil, "auth": "injected", "login_url": "https://console.example/settings/codex"}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// recordTurnError records how the last turn of a session ended, with the text of its error.
type recordTurnError struct{ as string }

func (recordTurnError) run(t *testing.T, _ *run) {
	t.Fatal("recordTurnError runs only under runCodexScenario")
}
func (a recordTurnError) runWire(t *testing.T, r *run, _ *harnesstest.OpenAI) {
	state := map[string]any{}
	if body, ok := r.drv.GetSession(t, r.id(t, a.as)).Body.(map[string]any); ok {
		if lt, ok := body["last_turn"].(map[string]any); ok {
			state["stop_reason"], state["error"] = lt["stop_reason"], lt["error"]
		}
	}
	r.record(t, "turn_error", a.as, callResult{Status: 200, Body: state})
}

// recordCacheKey records whether each request carries the hash of the session id as prompt_cache_key and never the session id.
type recordCacheKey struct{ as string }

func (recordCacheKey) run(t *testing.T, _ *run) {
	t.Fatal("recordCacheKey runs only under runCodexScenario")
}
func (a recordCacheKey) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	id := r.id(t, a.as)
	sum := sha256.Sum256([]byte(id))
	want := hex.EncodeToString(sum[:])[:32]
	state := map[string]any{"requests_with_the_hash": 0, "requests_with_the_session_id": 0, "requests": 0}
	for _, w := range o.WireEvents() {
		if w.Event != "request" {
			continue
		}
		state["requests"] = state["requests"].(int) + 1
		if w.PromptCacheKey == want {
			state["requests_with_the_hash"] = state["requests_with_the_hash"].(int) + 1
		}
		if strings.Contains(w.PromptCacheKey, id) {
			state["requests_with_the_session_id"] = state["requests_with_the_session_id"].(int) + 1
		}
	}
	r.record(t, "cache_key", a.as, callResult{Status: 200, Body: state})
}

func codexInjectedRows() []codexScenario {
	record := []action{recordWire{}}
	failed := []action{recordWire{}, recordTurnError{as: "a"}}
	unused := []harnesstest.Step{{Name: "reply", Repeat: true, Reply: codexText("hi")}}
	signedOut := harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{SignedOut: true}}
	return []codexScenario{
		{
			scenario: scenario{name: "codex_injected_http_request_carries_no_credential_header", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}},
			entry:    injectedEntry(nil),
			env:      map[string]string{"OPENAI_API_KEY": codexSentinelKey},
		},
		{
			scenario:  scenario{name: "codex_injected_websocket_dial_carries_no_credential_header", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			opts:      harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}},
			websocket: true,
			entry:     injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_injected_http_request_carries_the_hash_of_the_session_id_as_cache_key", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{recordCacheKey{as: "a"}})},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}},
			entry:    injectedEntry(nil),
		},
		{
			scenario:  scenario{name: "codex_injected_websocket_request_carries_the_hash_of_the_session_id_as_cache_key", model: codexHi, actions: codexSession(codexTurn("a", "hello"), []action{recordCacheKey{as: "a"}})},
			opts:      harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}},
			websocket: true,
			entry:     injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_injected_extra_headers_pass_through_as_placeholders", model: codexHi, actions: codexSession(codexTurn("a", "hello"), record)},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}},
			entry:    injectedEntry(map[string]any{"extra_headers": map[string]string{"Authorization": "Bearer placeholder-jwt", "ChatGPT-Account-ID": "placeholder-account"}}),
		},
		{
			scenario: scenario{name: "codex_injected_401_fails_the_turn_with_login_required", model: unused, actions: codexSession(codexTurn("a", "hello"), failed)},
			opts:     signedOut,
			entry:    injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_injected_401_without_a_login_url_names_the_provider_only", model: unused, actions: codexSession(codexTurn("a", "hello"), failed)},
			opts:     signedOut,
			entry:    injectedEntry(map[string]any{"login_url": nil}),
		},
		{
			scenario:  scenario{name: "codex_injected_websocket_401_fails_the_turn_with_login_required", model: unused, actions: codexSession(codexTurn("a", "hello"), failed)},
			opts:      signedOut,
			websocket: true,
			entry:     injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_injected_403_is_not_login_required", model: unused, actions: codexSession(codexTurn("a", "hello"), failed)},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{SignedOut: true, DenyStatus: 403}},
			entry:    injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_injected_redirect_is_an_error", model: unused, actions: codexSession(codexTurn("a", "hello"), []action{recordRedirect{}}, failed)},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}, RedirectTo: "/backend-api/codex/elsewhere"},
			entry:    injectedEntry(nil),
		},
	}
}

func codexModelsFromRows() []codexScenario {
	return []codexScenario{
		{
			scenario: scenario{name: "codex_models_from_chatgpt_lists_the_models_of_the_backend", actions: []action{models{}}},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}, Models: chatgptCatalog},
			entry:    injectedEntry(map[string]any{"models_from": "chatgpt"}),
		},
		{
			scenario: scenario{name: "codex_without_models_from_lists_the_static_catalog_and_fetches_nothing", actions: []action{models{}, noModelRequests{}}},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}, Models: chatgptCatalog},
			entry:    injectedEntry(nil),
		},
		{
			scenario: scenario{name: "codex_models_from_chatgpt_with_a_401_lists_the_static_catalog", actions: []action{models{}}},
			opts:     harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{SignedOut: true}, Models: chatgptCatalog},
			entry:    injectedEntry(map[string]any{"models_from": "chatgpt"}),
		},
		{
			scenario: scenario{name: "codex_login_entry_without_models_from_lists_the_static_catalog_and_fetches_nothing", actions: []action{models{}, noModelRequests{}}},
			opts:     harnesstest.OpenAIOptions{Models: chatgptCatalog},
			auth:     &codexAuth{label: "seed", lifetime: time.Hour},
			entry:    map[string]any{"models_from": nil},
		},
	}
}
