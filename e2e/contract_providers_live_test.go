package e2e

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// putProviders sends PUT /providers with the body that body builds, and records the status and the name and availability of each provider.
type putProviders struct {
	body   func(r *run, url string) any
	status int
}

func (a putProviders) run(t *testing.T, r *run) { a.send(t, r, r.fake.URL()) }

func (a putProviders) send(t *testing.T, r *run, url string) {
	t.Helper()
	res := r.drv.PutProviders(t, a.body(r, url))
	if want := cmpStatus(a.status); res.Status != want {
		t.Fatalf("PUT /providers = %d %v, want %d", res.Status, res.Body, want)
	}
	if res.Status != http.StatusOK {
		r.record(t, "put_providers", "", res)
		return
	}
	states := []string{}
	list, _ := res.Body.(map[string]any)["providers"].([]any)
	for _, e := range list {
		entry, _ := e.(map[string]any)
		states = append(states, fmt.Sprint(entry["name"], " ", entry["available"]))
	}
	r.record(t, "put_providers", "", callResult{Status: res.Status, Body: states})
}

// reconnectProvider sends POST /providers/{name}/reconnect and records the status.
type reconnectProvider struct {
	name   string
	status int
}

func (a reconnectProvider) run(t *testing.T, r *run) {
	t.Helper()
	res := r.drv.ReconnectProvider(t, a.name)
	if want := a.status; res.Status != want {
		t.Fatalf("POST /providers/%s/reconnect = %d %v, want %d", a.name, res.Status, res.Body, want)
	}
	r.record(t, "reconnect_provider", a.name, res)
}

// wireProviders is putProviders in a Codex scenario, where the Responses server has the URL.
type wireProviders struct {
	body   func(r *run, url string) any
	status int
}

func (a wireProviders) run(t *testing.T, _ *run) {
	t.Fatal("wireProviders runs only under runCodexScenario")
}
func (a wireProviders) runWire(t *testing.T, r *run, o *harnesstest.OpenAI) {
	putProviders(a).send(t, r, o.URL())
}

func gatewayEntry(url string) map[string]any {
	return map[string]any{"type": "openai-compat", "api_key_env": "ANTHROPIC_API_KEY", "base_url": url + "/v1"}
}

// bifrostEntry is the entry that a chat scenario configures.
func bifrostEntry(url string) map[string]any {
	e := gatewayEntry(url)
	e["extra_headers"] = map[string]string{"X-Tag": "e2e"}
	return e
}

func providersOf(entries func(url string) map[string]any) func(*run, string) any {
	return func(_ *run, url string) any { return map[string]any{"providers": entries(url)} }
}

func TestContractProviderLiveSet(t *testing.T) {
	reply := func(text string) harnesstest.Step {
		return harnesstest.Step{Name: text, Match: harnesstest.LastUserText(text), Reply: harnesstest.Reply{Text: "re " + text}}
	}
	turn := func(text string) []action { return []action{submit{as: "a", text: text}, waitIdle{as: "a"}} }
	gw := "gw/" + bifrostModel
	both := providersOf(func(url string) map[string]any {
		return map[string]any{"bifrost": bifrostEntry(url), "gw": gatewayEntry(url)}
	})
	runScenarios(t, []scenario{
		{
			name:  "provider_put_adds_a_provider_and_its_model_works_without_a_restart",
			chat:  true,
			model: []harnesstest.Step{reply("one")},
			actions: []action{
				tryCreate{model: gw},
				putProviders{body: both},
				create{as: "a", model: gw},
				submit{as: "a", text: "one"},
				waitIdle{as: "a"},
			},
		},
		{
			name:  "provider_put_that_removes_the_provider_of_a_session_fails_its_next_call_and_keeps_the_history",
			chat:  true,
			model: []harnesstest.Step{reply("one"), reply("three")},
			actions: append(append([]action{create{as: "a"}}, turn("one")...), append([]action{
				putProviders{body: providersOf(func(url string) map[string]any { return map[string]any{"gw": gatewayEntry(url)} })},
				tryCreate{model: "bifrost/" + bifrostModel},
			}, append(turn("two"), setModel{as: "a", model: gw}, submit{as: "a", text: "three"}, waitIdle{as: "a"})...)...),
		},
		{
			name:  "provider_put_with_an_invalid_entry_is_rejected_and_the_old_set_still_serves",
			chat:  true,
			model: []harnesstest.Step{reply("one")},
			actions: append([]action{
				create{as: "a"},
				putProviders{body: providersOf(func(url string) map[string]any {
					return map[string]any{"bifrost": map[string]any{"type": "nope", "base_url": url}}
				}), status: http.StatusBadRequest},
				putProviders{body: func(*run, string) any { return map[string]any{} }, status: http.StatusBadRequest},
			}, turn("one")...),
		},
		{
			name:    "provider_reconnect_of_an_unknown_provider_is_invalid",
			chat:    true,
			actions: []action{reconnectProvider{name: "nope", status: http.StatusBadRequest}},
		},
	})
}

func TestContractCodexProviderLiveSet(t *testing.T) {
	twoSteps := []harnesstest.Step{
		{Name: "one", Match: harnesstest.LastUserText("one"), Reply: codexText("1")},
		{Name: "two", Match: harnesstest.LastUserText("two"), Reply: codexText("2")},
	}
	codex := func(extra map[string]any) func(*run, string) any {
		return func(_ *run, url string) any {
			entry := codexConfig(url, true, nil)["providers"].(map[string]any)["codex"].(map[string]any)
			for k, v := range extra {
				entry[k] = v
			}
			return map[string]any{"providers": map[string]any{"codex": entry}}
		}
	}
	wire := []action{recordWire{}}
	runCodexScenarios(t, []codexScenario{
		{
			scenario: scenario{name: "codex_provider_put_with_the_same_entry_keeps_the_websocket", model: twoSteps,
				actions: codexSession(codexTurn("a", "one"), []action{wireProviders{body: codex(nil)}}, codexTurn("a", "two"), wire)},
			websocket: true,
		},
		{
			scenario: scenario{name: "codex_provider_put_with_a_changed_entry_dials_a_new_websocket", model: twoSteps,
				actions: codexSession(codexTurn("a", "one"), []action{wireProviders{body: codex(map[string]any{"extra_headers": map[string]string{"X-Tag": "e2e"}})}}, codexTurn("a", "two"), wire)},
			websocket: true,
		},
		{
			scenario: scenario{name: "codex_provider_reconnect_dials_a_new_websocket", model: twoSteps,
				actions: codexSession(codexTurn("a", "one"), []action{reconnectProvider{name: "codex", status: http.StatusNoContent}}, codexTurn("a", "two"), wire)},
			websocket: true,
		},
	})
}

// providerLiveRows is the disposition of the rows of the live provider set.
var providerLiveRows = map[string]runtimeRow{
	"provider_put_adds_a_provider_and_its_model_works_without_a_restart":                            sameAsServe(),
	"provider_put_that_removes_the_provider_of_a_session_fails_its_next_call_and_keeps_the_history": sameAsServe(),
	"provider_put_with_an_invalid_entry_is_rejected_and_the_old_set_still_serves":                   sameAsServe(),
	"provider_reconnect_of_an_unknown_provider_is_invalid":                                          sameAsServe(),
	"codex_provider_put_with_the_same_entry_keeps_the_websocket":                                    sameAsServe(),
	"codex_provider_put_with_a_changed_entry_dials_a_new_websocket":                                 sameAsServe(),
	"codex_provider_reconnect_dials_a_new_websocket":                                                sameAsServe(),
}
