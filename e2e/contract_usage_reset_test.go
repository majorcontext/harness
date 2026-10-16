package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

const resetCue = "usage limit of the provider has reset"

// resetLane is a lane whose provider states the reset time of a usage limit: the model API on the
// ChatGPT Codex backend, and the Claude Code CLI.
type resetLane struct {
	name string
	// open returns a session host whose model refuses the first limited turns with a usage limit
	// that resets in resetsIn, then answers. steps follow the refusals on the Codex lane.
	open func(t *testing.T, h host, resetsIn time.Duration, limited int, steps ...harnesstest.Step) (*runtimeDriver, *harnesstest.Server)
}

func codexLimit(resetsIn time.Duration) harnesstest.Reply {
	return harnesstest.Reply{HTTPStatus: 429, ErrorCode: "usage_limit_reached", ErrorMessage: "The usage limit has been reached", ResetsAt: time.Now().Add(resetsIn).Unix()}
}

var resetLanes = []resetLane{
	{name: "codex", open: func(t *testing.T, h host, resetsIn time.Duration, limited int, steps ...harnesstest.Step) (*runtimeDriver, *harnesstest.Server) {
		var all []harnesstest.Step
		for range limited {
			all = append(all, harnesstest.Step{Name: "limit", Reply: codexLimit(resetsIn)})
		}
		o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey}, append(all, steps...)...)
		return codexDriver(t, h, nil, nil, o.URL(), codexConfig(o.URL(), false, nil)).(*runtimeDriver), o.Server
	}},
	{name: "claudecode", open: func(t *testing.T, h host, resetsIn time.Duration, _ int, _ ...harnesstest.Step) (*runtimeDriver, *harnesstest.Server) {
		lane := claudeLane{mode: "usage_limit_rejected", env: map[string]string{"FAKE_CLAUDE_RESETS_IN": resetsIn.String()}}
		return lane.newDriver(t, h, "").(*claudeDriver).laneHost.(*runtimeDriver), nil
	}},
}

func eachResetLane(t *testing.T, row func(t *testing.T, h host, l resetLane)) {
	onHosts(t, func(t *testing.T, h host) {
		for _, l := range resetLanes {
			t.Run(l.name, func(t *testing.T) { row(t, h, l) })
		}
	})
}

// awaitTurnEnds returns once the session log holds n turn.ended records.
func (d *runtimeDriver) awaitTurnEnds(t *testing.T, id string, n int) {
	t.Helper()
	seen := 0
	d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
		if ev.Kind == "turn.ended" {
			seen++
		}
		return seen >= n
	})
}

// userInputs returns "source: text" for each user message of the session, in order.
func (d *runtimeDriver) userInputs(t *testing.T, id string) []string {
	t.Helper()
	var page protocol.MessagePage
	d.expect(t, http.StatusOK, http.MethodGet, withQuery("/sessions/"+id+"/messages", "limit", protocol.MaxMessageLimit), nil, &page)
	var out []string
	for _, m := range page.Messages {
		if m.Role != protocol.RoleUser {
			continue
		}
		var text []string
		for _, p := range m.Parts {
			text = append(text, p.Text)
		}
		out = append(out, m.Source+": "+strings.Join(text, " "))
	}
	return out
}

func sources(inputs []string) []string {
	out := make([]string, len(inputs))
	for i, in := range inputs {
		out[i], _, _ = strings.Cut(in, ": ")
	}
	return out
}

func TestContractUsageResetContinue(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T, h host, l resetLane)
	}{
		{"a_session_continues_when_the_usage_limit_has_reset_and_stops_after_two_continues", resetStopsAfterTwoContinues},
		{"the_continue_runs_before_the_held_queue", resetRunsBeforeHeldQueue},
		{"the_view_shows_the_pending_continue_and_an_input_cancels_it", resetViewAndCancel},
		{"a_reset_more_than_a_day_away_arms_no_continue", resetBeyondADay},
		{"a_pending_continue_survives_a_restart", resetSurvivesRestart},
		{"an_interrupt_cancels_the_pending_continue_across_a_restart", resetInterruptCancels},
		{"ending_the_session_cancels_the_pending_continue_across_a_restart", resetEndCancels},
		{"a_prompt_that_names_the_usage_reset_source_is_an_ordinary_input", resetClientSourceIsOrdinary},
	}
	eachResetLane(t, func(t *testing.T, h host, l resetLane) {
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				t.Parallel()
				row.run(t, h, l)
			})
		}
	})
}

func resetStopsAfterTwoContinues(t *testing.T, h host, l resetLane) {
	d, _ := l.open(t, h, -time.Minute, 3)
	id := d.Create(t)
	d.Submit(t, id, "first")
	d.awaitTurnEnds(t, id, 3)
	v := d.view(t, id)
	if v.ContinueAt != 0 || v.LastTurn == nil || v.LastTurn.Cause != protocol.CauseProviderExhausted || v.LastTurn.ResetsAt == 0 {
		t.Fatalf("view after the third refusal = continue_at %d, last_turn %+v; want no continue and a provider_exhausted turn with resets_at", v.ContinueAt, v.LastTurn)
	}
	inputs := d.userInputs(t, id)
	if want := []string{"user", "usage_reset", "usage_reset"}; !slices.Equal(sources(inputs), want) {
		t.Fatalf("user message sources = %v, want %v", sources(inputs), want)
	}
	if !strings.Contains(inputs[1], resetCue) {
		t.Fatalf("continue input = %q, want the usage limit reset text", inputs[1])
	}
}

func resetRunsBeforeHeldQueue(t *testing.T, h host, l resetLane) {
	if l.name != "codex" {
		t.Skip("the Claude Code lane has no step that holds a turn open")
	}
	d, fake := l.open(t, h, -time.Minute, 0,
		harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("first"), Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}},
		harnesstest.Step{Name: "wall", Match: harnesstest.LastUserText("first"), Reply: codexLimit(-time.Minute)},
		harnesstest.Step{Name: "continued", Match: harnesstest.LastUserText(resetCue), Reply: harnesstest.Reply{Text: "continued"}},
		harnesstest.Step{Name: "second", Match: harnesstest.LastUserText("second"), Reply: harnesstest.Reply{Text: "2"}},
	)
	id := d.Create(t)
	d.Submit(t, id, "first")
	if !fake.AwaitRequests(1, waitBound) {
		t.Fatal("the first request never arrived")
	}
	d.EnqueueNext(t, id, "second")
	d.awaitTurnEnds(t, id, 3)
	d.WaitIdle(t, id)
	if got, want := sources(d.userInputs(t, id)), []string{"user", "usage_reset", "user"}; !slices.Equal(got, want) {
		t.Fatalf("user message sources = %v, want the continue before the held input %v", got, want)
	}
}

func resetViewAndCancel(t *testing.T, h host, l resetLane) {
	d, _ := l.open(t, h, time.Hour, 1, harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true})
	id := d.Create(t)
	d.Submit(t, id, "first")
	d.awaitTurnEnds(t, id, 1)
	v := d.view(t, id)
	if v.LastTurn == nil || v.LastTurn.ResetsAt == 0 || v.ContinueAt != v.LastTurn.ResetsAt {
		t.Fatalf("view = continue_at %d, last_turn %+v; want continue_at equal to last_turn.resets_at", v.ContinueAt, v.LastTurn)
	}
	if got := time.Until(time.Unix(v.ContinueAt, 0)); got < 50*time.Minute || got > time.Hour {
		t.Fatalf("continue_at is %s away, want about an hour", got)
	}
	d.Submit(t, id, "again")
	d.awaitTurnEnds(t, id, 2)
	if got := sources(d.userInputs(t, id)); slices.Contains(got, "usage_reset") {
		t.Fatalf("user message sources = %v, want no usage_reset input after an input cancelled the continue", got)
	}
}

func resetBeyondADay(t *testing.T, h host, l resetLane) {
	d, _ := l.open(t, h, 48*time.Hour, 1)
	id := d.Create(t)
	d.Submit(t, id, "first")
	d.awaitTurnEnds(t, id, 1)
	v := d.view(t, id)
	if v.ContinueAt != 0 || v.LastTurn == nil || v.LastTurn.ResetsAt == 0 {
		t.Fatalf("view = continue_at %d, last_turn %+v; want no continue and the reset time of the limit", v.ContinueAt, v.LastTurn)
	}
}

func resetSurvivesRestart(t *testing.T, h host, l resetLane) {
	d, _ := l.open(t, h, time.Hour, 1)
	id := d.Create(t)
	d.Submit(t, id, "first")
	d.awaitTurnEnds(t, id, 1)
	want := d.view(t, id).ContinueAt
	if want == 0 {
		t.Fatal("no pending continue before the restart")
	}
	d.Restart(t, false)
	if !d.serve {
		d.openAll(t)
	}
	if got := d.view(t, id).ContinueAt; got != want {
		t.Fatalf("continue_at after the restart = %d, want %d", got, want)
	}
}

func TestContractGoalWaitsForTheUsageLimitReset(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_goal_pauses_at_the_reset_time_and_the_pause_does_not_count", func(t *testing.T) {
			t.Parallel()
			o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey},
				harnesstest.Step{Name: "limit", Reply: codexLimit(time.Hour)})
			d := codexDriver(t, h, nil, nil, o.URL(), codexConfig(o.URL(), false, nil)).(*runtimeDriver)
			id := d.Create(t)
			d.SetGoal(t, id, "say done", 0, false)
			d.awaitTurnEnds(t, id, 1)
			v := d.view(t, id)
			if v.Goal == nil || v.Goal.State != protocol.GoalPaused || v.Goal.Pauses != 0 || v.LastTurn == nil {
				t.Fatalf("goal = %+v, want paused with no counted pause", v.Goal)
			}
			if got := v.Goal.RetryAt.Unix(); got != v.LastTurn.ResetsAt {
				t.Fatalf("goal retry_at = %d, want the reset time %d", got, v.LastTurn.ResetsAt)
			}
			if v.ContinueAt != 0 {
				t.Fatalf("continue_at = %d, want none for a session with a goal", v.ContinueAt)
			}
		})
	})
}

// limitedSession runs one turn that a usage limit refuses, resetting in an hour, and returns the reset time.
func limitedSession(t *testing.T, h host, l resetLane) (*runtimeDriver, string, int64) {
	t.Helper()
	d, _ := l.open(t, h, time.Hour, 1, harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true})
	id := d.Create(t)
	d.Submit(t, id, "first")
	d.awaitTurnEnds(t, id, 1)
	at := d.view(t, id).ContinueAt
	if at == 0 {
		t.Fatal("no pending continue after the usage limit")
	}
	return d, id, at
}

func (d *runtimeDriver) reopen(t *testing.T) {
	t.Helper()
	d.Restart(t, false)
	if !d.serve {
		d.openAll(t)
	}
}

func resetInterruptCancels(t *testing.T, h host, l resetLane) {
	d, id, _ := limitedSession(t, h, l)
	d.Interrupt(t, id)
	if at := d.view(t, id).ContinueAt; at != 0 {
		t.Fatalf("continue_at after the interrupt = %d, want none", at)
	}
	d.reopen(t)
	if at := d.view(t, id).ContinueAt; at != 0 {
		t.Fatalf("continue_at after the restart = %d, want none for an interrupted continue", at)
	}
}

func resetEndCancels(t *testing.T, h host, l resetLane) {
	d, id, _ := limitedSession(t, h, l)
	d.EndSession(t, id)
	d.reopen(t)
	if at := d.view(t, id).ContinueAt; at != 0 {
		t.Fatalf("continue_at after the end and the restart = %d, want none", at)
	}
}

func resetClientSourceIsOrdinary(t *testing.T, h host, l resetLane) {
	d, id, _ := limitedSession(t, h, l)
	path, body := d.inputAs(id, "client_reset", "carry on", "", protocol.SourceUsageReset)
	d.expect(t, http.StatusCreated, http.MethodPost, path, body, nil)
	d.awaitTurnEnds(t, id, 2)
	if at := d.view(t, id).ContinueAt; at != 0 && l.name == "codex" {
		t.Fatalf("continue_at = %d, want the input of a client to cancel the continue", at)
	}
	inputs := d.userInputs(t, id)
	if len(inputs) != 2 || inputs[1] != "usage_reset: carry on" {
		t.Fatalf("user messages = %v, want the client prompt as the only usage_reset message, run as an ordinary input", inputs)
	}
}

func TestContractCancelledContinueDoesNotBlockTheNextOne(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_new_limit_with_the_same_reset_time_arms_a_continue_after_a_cancel", func(t *testing.T) {
			t.Parallel()
			reset := time.Now().Add(time.Hour).Unix()
			limit := codexLimit(0)
			limit.ResetsAt = reset
			o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey},
				harnesstest.Step{Name: "limit", Reply: limit}, harnesstest.Step{Name: "again", Reply: limit})
			d := codexDriver(t, h, nil, nil, o.URL(), codexConfig(o.URL(), false, nil)).(*runtimeDriver)
			id := d.Create(t)
			d.Submit(t, id, "first")
			d.awaitTurnEnds(t, id, 1)
			d.Interrupt(t, id)
			d.Submit(t, id, "again")
			d.awaitTurnEnds(t, id, 2)
			if at := d.view(t, id).ContinueAt; at != reset {
				t.Fatalf("continue_at = %d, want %d: a cancel must not suppress a later continue", at, reset)
			}
		})
	})
}

func TestContractClaudeCodeGoalWaitsForTheUsageLimitReset(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("claudecode_goal_pauses_at_the_reset_time_and_the_pause_does_not_count", func(t *testing.T) {
			t.Parallel()
			lane := claudeLane{mode: "usage_limit_rejected", env: map[string]string{"FAKE_CLAUDE_RESETS_IN": "1h"}}
			d := lane.newDriver(t, h, "").(*claudeDriver).laneHost.(*runtimeDriver)
			id := d.Create(t)
			d.SetGoal(t, id, "say done", 0, false)
			d.awaitTurnEnds(t, id, 1)
			v := d.view(t, id)
			if v.Goal == nil || v.Goal.State != protocol.GoalPaused || v.Goal.Pauses != 0 || v.LastTurn == nil || v.Goal.RetryAt.Unix() != v.LastTurn.ResetsAt {
				t.Fatalf("goal = %+v, last turn %+v; want paused until the reset time with no counted pause", v.Goal, v.LastTurn)
			}
		})
	})
}

func TestContractInterruptByCompactionIDCancelsTheContinue(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("an_interrupt_with_the_id_of_a_running_compaction_cancels_the_pending_continue", func(t *testing.T) {
			t.Parallel()
			ok := func(user string) harnesstest.Step {
				return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "re " + user}}
			}
			o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey},
				ok("one"), ok("two"),
				harnesstest.Step{Name: "limit", Match: harnesstest.LastUserText("three"), Reply: codexLimit(time.Hour)},
				harnesstest.Step{Name: "slow_summary", Match: harnesstest.SystemContains("You are summarizing a prefix"),
					Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}, Repeat: true})
			d := codexDriver(t, h, nil, nil, o.URL(), codexConfig(o.URL(), false, nil)).(*runtimeDriver)
			id := d.Create(t)
			for _, text := range []string{"one", "two", "three"} {
				d.Submit(t, id, text)
				d.awaitTurnEnds(t, id, map[string]int{"one": 1, "two": 2, "three": 3}[text])
			}
			if d.view(t, id).ContinueAt == 0 {
				t.Fatal("no pending continue after the usage limit")
			}
			head := d.view(t, id).HeadSeq
			base, client := d.endpoint()
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/sessions/"+id+"/compact", strings.NewReader("{}"))
				if err != nil {
					return
				}
				req.Header.Set("Content-Type", "application/json")
				d.authorize(req)
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}()
			var compaction string
			d.stream(t, id, head, false, func(_ string, ev protocol.Event) bool {
				var f protocol.StatusFrame
				if ev.Kind == protocol.KindStatus && json.Unmarshal(ev.Data, &f) == nil && f.Status == protocol.StatusCompacting {
					compaction = f.TurnID
				}
				return compaction != ""
			})
			if status, _ := d.do(t, http.MethodPost, "/sessions/"+id+"/interrupt", map[string]any{"turn_id": "other"}); status != http.StatusConflict {
				t.Fatalf("interrupt with another ID = %d, want 409", status)
			}
			if d.view(t, id).ContinueAt == 0 {
				t.Fatal("an interrupt with another ID cancelled the continue")
			}
			d.expect(t, http.StatusNoContent, http.MethodPost, "/sessions/"+id+"/interrupt", map[string]any{"turn_id": compaction}, nil)
			if at := d.view(t, id).ContinueAt; at != 0 {
				t.Fatalf("continue_at after the interrupt of the compaction = %d, want none", at)
			}
			cancelled := false
			d.stream(t, id, 0, false, func(_ string, ev protocol.Event) bool {
				cancelled = cancelled || ev.Kind == protocol.KindContinueCancelled
				return cancelled || ev.Seq >= d.view(t, id).HeadSeq
			})
			if !cancelled {
				t.Fatal("the log holds no continue.cancelled")
			}
		})
	})
}
