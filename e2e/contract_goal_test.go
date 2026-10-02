package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

func notEvaluator(r fakemodel.Request) bool {
	return !strings.Contains(r.System, goalEvaluatorMarker)
}

func agentStep(name, text string, repeat bool) fakemodel.Step {
	return fakemodel.Step{Name: name, Match: notEvaluator, Reply: fakemodel.Reply{Text: text}, Repeat: repeat}
}

func evaluatorStep(name, verdict string, repeat bool) fakemodel.Step {
	return fakemodel.Step{
		Name:   name,
		Match:  fakemodel.SystemContains("You are a strict goal-completion evaluator"),
		Reply:  fakemodel.Reply{Text: verdict},
		Repeat: repeat,
	}
}

// An exhausted goal stays active, so the session never reads idle and the
// runner's final waitIdle would block; the alias stays out of run.aliases.
type createUntracked struct{ as string }

func (a createUntracked) run(t *testing.T, r *run) { r.ids[a.as] = r.drv.Create(t) }

type awaitMaxTurnsExceeded struct{}

func (awaitMaxTurnsExceeded) run(t *testing.T, r *run) {
	t.Helper()
	d := r.drv.(*httpDriver)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.p.addr+"/event?from=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /event: %v", err)
	}
	defer resp.Body.Close()
	sc := newSSEScanner(resp.Body)
	for {
		raw, err := sc.next()
		if err != nil {
			t.Fatalf("event stream ended before max_turns_exceeded: %v", err)
		}
		var ev struct{ Type, Outcome string }
		if json.Unmarshal(raw, &ev) == nil && ev.Type == "turn.end" && ev.Outcome == "max_turns_exceeded" {
			return
		}
	}
}

func TestContractGoal(t *testing.T) {
	armed := func(g setGoal) []action {
		return []action{create{as: "a"}, g, waitIdle{as: "a"}}
	}
	deferredThenGo := func(g setGoal) []action {
		return []action{create{as: "a"}, g, submit{as: "a", text: "go"}, waitIdle{as: "a"}}
	}
	runScenarios(t, []scenario{
		{
			name: "goal_met_first_turn",
			model: []fakemodel.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "goal_not_met_then_met",
			model: []fakemodel.Step{
				agentStep("try", "try", false),
				evaluatorStep("judge1", "NOT MET: say done", false),
				agentStep("finish", "done", false),
				evaluatorStep("judge2", "MET: said done", false),
			},
			actions: armed(setGoal{as: "a", condition: "say done", maxTurns: 3}),
		},
		{
			name: "deferred_goal_judges_finished_turn",
			model: []fakemodel.Step{
				agentStep("work", "done", false),
				evaluatorStep("judge", "MET: said done", false),
			},
			actions: deferredThenGo(setGoal{as: "a", condition: "say done", deferred: true}),
		},
		{
			name: "goal_exhausts_max_turns",
			model: []fakemodel.Step{
				agentStep("try", "try", true),
				evaluatorStep("judge", "NOT MET: keep going", true),
			},
			actions: []action{
				createUntracked{as: "a"},
				setGoal{as: "a", condition: "say done", maxTurns: 2},
				awaitMaxTurnsExceeded{},
			},
		},
	})
}
