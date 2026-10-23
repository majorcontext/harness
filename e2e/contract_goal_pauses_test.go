package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestContractGoalPauses(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_goal_that_keeps_hitting_a_usage_limit_fails_after_six_pauses", func(t *testing.T) { goalFailsAfterSixPauses(t, h) })
		t.Run("a_judge_usage_limit_is_typed_and_the_next_judge_uses_the_updated_account", func(t *testing.T) { goalJudgeUsesUpdatedAccount(t, h) })
		t.Run("a_claude_code_judge_usage_limit_carries_the_provider_reset", func(t *testing.T) { claudeJudgeUsageLimitReset(t, h) })

		t.Run("a_paused_goal_reports_its_pause_count_and_a_verdict_resets_it", func(t *testing.T) {
			t.Parallel()
			wall := func(name string) harnesstest.Step {
				return harnesstest.Step{Name: name, Match: notEvaluator, Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}}
			}
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				wall("wall1"), wall("wall2"),
				agentStep("work", "partial", false),
				evaluatorStep("judge", "NOT MET: keep going", false),
				harnesstest.Step{Name: "blocked", Match: notEvaluator, Reply: harnesstest.Reply{Text: "done", Block: true}},
				evaluatorStep("judge2", "MET: ok", false),
			)
			id := d.Create(t)
			d.SetGoal(t, id, "say done", 0, false)
			d.WaitIdle(t, id)
			if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != 1 {
				t.Fatalf("goal after the first pause = %+v, want paused with 1 pause", g)
			}
			for _, event := range d.events(t, id) {
				if event.Kind != protocol.KindGoalChanged {
					continue
				}
				var changed struct {
					State string `json:"state"`
					Cause string `json:"cause"`
				}
				if err := json.Unmarshal(event.Data, &changed); err != nil {
					t.Fatalf("decode goal.changed: %v", err)
				}
				if changed.State == string(protocol.GoalPaused) && changed.Cause != "" {
					t.Fatalf("goal.changed cause = %q for a turn failure, want empty", changed.Cause)
				}
			}
			d.Submit(t, id, "again")
			d.WaitIdle(t, id)
			if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != 2 {
				t.Fatalf("goal after the second pause = %+v, want paused with 2 pauses", g)
			}
			d.Submit(t, id, "third")
			if !fake.AwaitRequests(5, waitBound) {
				t.Fatalf("waited %s for the guidance turn; saw %d requests", waitBound, len(fake.Requests()))
			}
			if g := d.view(t, id).Goal; g == nil || g.State != "active" || g.Pauses != 0 {
				t.Errorf("goal after a verdict = %+v, want active with no pauses", g)
			}
			fake.Release("blocked")
			d.WaitIdle(t, id)
		})
	})
}

func goalJudgeUsesUpdatedAccount(t *testing.T, h host) {
	t.Parallel()
	const upstream = "http://subscription-api.test"
	fake := harnesstest.NewAnthropic(t,
		agentStep("work_a", "done", false),
		harnesstest.Step{Name: "judge_a", Match: isEvaluator, Reply: harnesstest.Reply{HTTPStatus: http.StatusTooManyRequests, ErrorMessage: usageLimitMessage}},
		evaluatorStep("judge_b", "MET: done", false),
	)
	proxyURL, capture := newAccountProxy(t, fake.URL())
	config := writeGoalConfigWith(t, upstream, map[string]any{
		"providers":       map[string]any{"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": upstream, "vendor": "claude"}},
		"account_routing": map[string]any{"proxy_url_env": "ACCOUNT_PROXY", "separator": "|"},
	})
	env := map[string]string{"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL, "http_proxy": proxyURL, "https_proxy": proxyURL,
		"ACCOUNT_PROXY": proxyURL, "NO_PROXY": "localhost,127.0.0.1", "E2E_MODEL_PROXY": proxyURL}
	d := h.open(t, config, env).(*runtimeDriver)
	id := d.Create(t)
	d.SetAccounts(t, id, map[string]string{"claude": "acct_a"})
	d.SetGoal(t, id, "say done", 1, false)
	var cause string
	var resetsAt int64
	d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind != protocol.KindGoalChanged {
			return false
		}
		var data struct {
			State    string `json:"state"`
			Cause    string `json:"cause"`
			ResetsAt int64  `json:"resets_at"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode goal.changed: %v", err)
		}
		if data.State != string(protocol.GoalPaused) {
			return false
		}
		cause, resetsAt = data.Cause, data.ResetsAt
		return true
	})
	if cause != string(protocol.CauseProviderExhausted) || resetsAt != 0 {
		t.Fatalf("goal.changed cause, resets_at = %q, %d; want provider_exhausted, 0", cause, resetsAt)
	}
	d.SetAccounts(t, id, map[string]string{"claude": "acct_b"})
	d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind != protocol.KindGoalChanged {
			return false
		}
		var data struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode goal.changed: %v", err)
		}
		return data.State == string(protocol.GoalAchieved)
	})
	requests := fake.Requests()
	if len(requests) != 3 || isEvaluator(requests[0]) || !isEvaluator(requests[1]) || !isEvaluator(requests[2]) {
		t.Fatalf("model request purposes = %+v, want work/judge/judge", requests)
	}
	for _, event := range d.events(t, id) {
		if event.Kind != protocol.KindTurnEnded {
			continue
		}
		payload, err := event.Payload()
		if err != nil {
			t.Fatalf("decode turn.ended: %v", err)
		}
		if ended, ok := payload.(*protocol.TurnEnded); ok && ended.Cause == protocol.CauseProviderExhausted {
			t.Fatal("goal judge exhaustion wrote turn.ended instead of goal.changed")
		}
	}
	capture.mu.Lock()
	users := slices.Clone(capture.users)
	capture.mu.Unlock()
	want := []string{"subject|box|acct_a", "subject|box|acct_a", "subject|box|acct_b"}
	if !slices.Equal(users, want) {
		t.Fatalf("model account routes = %v, want %v", users, want)
	}
}

func claudeJudgeUsageLimitReset(t *testing.T, h host) {
	t.Parallel()
	lane := claudeLane{mode: "one_shot", spawnModes: "2=usage_limit_rejected", extra: map[string]any{"goal_evaluator_model": "claude-code/haiku"}}
	d := lane.newDriver(t, h, "http://model.test").(*claudeDriver)
	id := d.Create(t)
	d.SetGoal(t, id, "say done", 0, false)
	var cause string
	var resetsAt int64
	d.laneHost.(*runtimeDriver).stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind != protocol.KindGoalChanged {
			return false
		}
		var data struct {
			State    string `json:"state"`
			Cause    string `json:"cause"`
			ResetsAt int64  `json:"resets_at"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode goal.changed: %v", err)
		}
		if data.State != string(protocol.GoalPaused) {
			return false
		}
		cause, resetsAt = data.Cause, data.ResetsAt
		return true
	})
	if cause != string(protocol.CauseProviderExhausted) || resetsAt != 1792050000 {
		t.Fatalf("goal.changed cause, resets_at = %q, %d; want provider_exhausted, 1792050000", cause, resetsAt)
	}
}

func goalFailsAfterSixPauses(t *testing.T, h host) {
	t.Parallel()
	const budget = 6
	const before = 2
	wall := func(name string) harnesstest.Step {
		return harnesstest.Step{Name: name, Match: notEvaluator, Reply: harnesstest.Reply{HTTPStatus: 429, ErrorMessage: usageLimitMessage}}
	}
	var steps []harnesstest.Step
	for i := range before {
		steps = append(steps, wall(fmt.Sprintf("early%d", i+1)))
	}
	steps = append(steps, agentStep("work", "partial", false), evaluatorStep("judge", "NOT MET: keep going", false))
	for i := range budget + 1 {
		steps = append(steps, wall(fmt.Sprintf("wall%d", i+1)))
	}
	steps = append(steps, agentStep("after", "done", false))
	d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, steps...)
	id := d.Create(t)
	d.SetGoal(t, id, "say done", 0, false)
	d.WaitIdle(t, id)
	expectPaused := func(want int) {
		t.Helper()
		if g := d.view(t, id).Goal; g == nil || g.State != "paused" || g.Pauses != want {
			t.Fatalf("goal = %+v, want paused with %d pauses", g, want)
		}
	}
	for want := 1; want <= before; want++ {
		expectPaused(want)
		d.Submit(t, id, fmt.Sprintf("early %d", want))
		d.WaitIdle(t, id)
	}
	for want := 1; want <= budget; want++ {
		expectPaused(want)
		d.Submit(t, id, fmt.Sprintf("again %d", want))
		d.WaitIdle(t, id)
	}
	g := d.view(t, id).Goal
	if g == nil || g.State != "failed" || g.Reason != "retries_exhausted" {
		t.Fatalf("goal after the seventh wall = %+v, want failed with reason retries_exhausted", g)
	}
	d.Submit(t, id, "continue")
	d.WaitIdle(t, id)
}
