package harness_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/protocol"
)

// calls is one assistant message that calls tool once for each argument set.
func calls(tool string, args ...map[string]any) eventlog.Message {
	m := eventlog.Message{Role: eventlog.RoleAssistant}
	for i, a := range args {
		raw, _ := json.Marshal(a)
		m.Parts = append(m.Parts, eventlog.Part{Type: eventlog.PartToolCall, CallID: "g" + string(rune('0'+i)), Name: tool, Arguments: raw})
	}
	return m
}

// results returns the tool results in the newest request of session that has any.
func (f *family) results(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, req := range f.reqs {
		var got []string
		for _, m := range req.History {
			for _, p := range m.Parts {
				if p.Type == eventlog.PartToolResult && req.SessionID == session {
					got = append(got, p.Text)
				}
			}
		}
		if len(got) > 0 {
			out = got
		}
	}
	return out
}

func goalRuntime(t *testing.T, f *family, evaluator string) *harness.Runtime {
	t.Helper()
	return familyRuntime(t, harness.NewMemStore(), f, nil, config.Config{GoalEvaluatorModel: evaluator}, "")
}

func judging(verdict string, more func(eventlog.Part) []eventlog.Message) func(eventlog.Part) []eventlog.Message {
	return func(p eventlog.Part) []eventlog.Message {
		if strings.HasPrefix(p.Text, "GOAL CONDITION:") {
			return []eventlog.Message{say(verdict)}
		}
		return more(p)
	}
}

func TestGoalTool(t *testing.T) {
	const adjustHint = `use action "adjust" to change its condition instead`
	set := func(c string) map[string]any { return map[string]any{"action": "set", "condition": c} }
	status := map[string]any{"action": "status"}
	for _, tc := range []struct {
		name  string
		calls []map[string]any
		want  []string
	}{
		{"set arms a goal that status reports", []map[string]any{set(" tests pass "), status},
			[]string{`{"active":true,"condition":"tests pass"}`, `{"active":true,"condition":"tests pass"}`}},
		{"set fails while a goal is active", []map[string]any{set("a"), set("b")},
			[]string{`{"active":true,"condition":"a"}`, "goal: a goal is already active; " + adjustHint}},
		{"set needs a condition", []map[string]any{set(" ")},
			[]string{"goal: set requires a non-empty condition (if a goal is already active, " + adjustHint + ")"}},
		{"adjust needs an active goal", []map[string]any{status, {"action": "adjust", "condition": "x"}},
			[]string{`{"active":false,"condition":""}`, "goal: no active goal to update"}},
		{"adjust replaces the condition", []map[string]any{set("a"), {"action": "adjust", "condition": "b"}},
			[]string{`{"active":true,"condition":"a"}`, `{"active":true,"condition":"b"}`}},
		{"clearing a goal is for the operator", []map[string]any{{"action": "clear"}},
			[]string{`goal: unknown action "clear" (clearing a goal is operator-only — DELETE /sessions/{id}/goal on the HTTP API)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &family{answer: judging("MET: ok", func(p eventlog.Part) []eventlog.Message {
					if p.Text == "go" {
						return []eventlog.Message{calls("goal", tc.calls...)}
					}
					return []eventlog.Message{say("ok")}
				})}
				r := goalRuntime(t, f, "test/eval")
				submit(t, create(t, r), text("a", "go"))
				if got := f.results("s1"); !slices.Equal(got, tc.want) {
					t.Errorf("results =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
				}
				closeRuntime(t, r)
			})
		})
	}
}

func TestGoalToolNeedsAnEvaluator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &family{answer: func(eventlog.Part) []eventlog.Message { return []eventlog.Message{say("ok")} }}
		r := goalRuntime(t, f, "")
		submit(t, create(t, r), text("a", "go"))
		if req, _ := f.last("s1", "go"); slices.ContainsFunc(req.Tools, func(s protocol.ToolSpec) bool { return s.Name == "goal" }) {
			t.Error("a runtime with no goal_evaluator_model offers the goal tool")
		}
		closeRuntime(t, r)
	})
}

func TestGoalToolAdjustKeepsTheTurnLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		adjusted := false
		f := &family{answer: judging("NOT MET: more", func(p eventlog.Part) []eventlog.Message {
			if strings.HasPrefix(p.Text, "The goal has not been met yet.") && !adjusted {
				adjusted = true
				return []eventlog.Message{calls("goal", map[string]any{"action": "adjust", "condition": "say done now"})}
			}
			return []eventlog.Message{say("working")}
		})}
		r := goalRuntime(t, f, "test/eval")
		s := create(t, r)
		if err := s.SetGoal(bg, protocol.Goal{Condition: "say done", MaxTurns: 2}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		judged := 0
		for _, req := range f.reqs {
			if strings.HasPrefix(req.History[0].Parts[0].Text, "GOAL CONDITION:") {
				judged++
			}
		}
		if g := s.View().Goal; g == nil || g.State != "exhausted" || judged != 2 || g.Condition != "say done now" {
			t.Errorf("goal %+v after %d judged turns, want say done now exhausted after 2", g, judged)
		}
		closeRuntime(t, r)
	})
}
