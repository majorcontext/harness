package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type createSystem struct {
	as     string
	body   map[string]any
	status int
}

func (a createSystem) run(t *testing.T, r *run) {
	res := r.drv.(*runtimeDriver).call(t, http.MethodPost, "/sessions", a.body)
	if res.Status != a.status {
		t.Fatalf("create = %d %v, want %d", res.Status, res.Body, a.status)
	}
	if a.status == 201 {
		r.ids[a.as] = res.Body.(map[string]any)["id"].(string)
		r.aliases = append(r.aliases, a.as)
	}
}

type expectSystemBlocks struct {
	texts      []string
	breakpoint int
}

func (a expectSystemBlocks) run(t *testing.T, r *run) {
	raw := r.fake.RawRequests()
	var blocks []struct {
		Text  string          `json:"text"`
		Cache json.RawMessage `json:"cache_control"`
	}
	if system := first(raw[len(raw)-1], "system"); len(system) > 0 {
		if err := json.Unmarshal(system, &blocks); err != nil {
			t.Fatal(err)
		}
	}
	var texts []string
	for i, b := range blocks {
		texts = append(texts, b.Text)
		if (len(b.Cache) > 0) != (i == a.breakpoint) {
			t.Fatalf("system breakpoint at %d = %s", i, b.Cache)
		}
	}
	if !slices.Equal(texts, a.texts) {
		t.Fatalf("system = %q, want %q", texts, a.texts)
	}
}

var sessionSystemRows = map[string]runtimeRow{
	"session_system_blocks_are_isolated_and_survive_reopen": sameAsServe(),
	"session_system_rejects_invalid_breakpoint":             sameAsServe(),
}

func TestContractSessionSystem(t *testing.T) {
	runScenarios(t, []scenario{{name: "session_system_blocks_are_isolated_and_survive_reopen", model: []harnesstest.Step{{Name: "reply", Repeat: true, Reply: harnesstest.Reply{Text: "ok"}}}, actions: []action{
		createSystem{as: "a", body: map[string]any{"system_blocks": []string{"stable", "private"}, "system_cache_breakpoint": 0}, status: 201},
		submit{as: "a", text: "first"}, waitIdle{as: "a"}, expectSystemBlocks{[]string{"stable", "private"}, 0},
		createSystem{as: "b", body: map[string]any{"system_blocks": []string{"other", "last"}}, status: 201}, submit{as: "b", text: "second"}, waitIdle{as: "b"}, expectSystemBlocks{[]string{"other", "last"}, 1},
		createSystem{as: "c", body: map[string]any{"system_blocks": []string{}}, status: 201}, submit{as: "c", text: "clear"}, waitIdle{as: "c"}, expectSystemBlocks{nil, -1},
		restart{}, submit{as: "a", text: "again"}, waitIdle{as: "a"}, expectSystemBlocks{[]string{"stable", "private"}, 0},
	}}, {name: "session_system_rejects_invalid_breakpoint", actions: []action{
		createSystem{body: map[string]any{"system_blocks": []string{""}}, status: 400},
		createSystem{body: map[string]any{"system_blocks": []string{}, "system_cache_breakpoint": 0}, status: 400},
		createSystem{body: map[string]any{"system_blocks": []string{"a"}, "system_cache_breakpoint": -1}, status: 400},
		createSystem{body: map[string]any{"system_blocks": []string{"a"}, "system_cache_breakpoint": 1}, status: 400},
	}}})
}
