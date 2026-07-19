package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// runGoalToolAction runs the goal tool's Run function directly against s and
// decodes a successful result as goalToolResult. t.Fatal on a tool error or
// undecodable result — use callGoalToolExpectError below for the error path.
func runGoalToolAction(t *testing.T, s *Session, args string) goalToolResult {
	t.Helper()
	tool, ok := s.tools[goalToolName]
	if !ok {
		t.Fatal("goal tool absent")
	}
	parts, err := tool.Run(context.Background(), s, []byte(args))
	if err != nil {
		t.Fatalf("goal tool run(%s): %v", args, err)
	}
	text, ok := parts[0].(*message.Text)
	if !ok {
		t.Fatalf("goal tool result is not text: %#v", parts[0])
	}
	var res goalToolResult
	if err := json.Unmarshal([]byte(text.Text), &res); err != nil {
		t.Fatalf("goal tool result not valid JSON: %v (%s)", err, text.Text)
	}
	return res
}

// callGoalToolExpectError runs the goal tool's Run function directly and
// requires it to return a non-nil error, returning that error's message.
func callGoalToolExpectError(t *testing.T, s *Session, args string) string {
	t.Helper()
	tool, ok := s.tools[goalToolName]
	if !ok {
		t.Fatal("goal tool absent")
	}
	_, err := tool.Run(context.Background(), s, []byte(args))
	if err == nil {
		t.Fatalf("goal tool run(%s): want error, got nil", args)
	}
	return err.Error()
}

func newGoalToolSession(t *testing.T) *Session {
	t.Helper()
	return NewSession(Config{GoalTool: true})
}

func TestGoalToolStatus(t *testing.T) {
	s := newGoalToolSession(t)

	res := runGoalToolAction(t, s, `{"action":"status"}`)
	if res.Active || res.Condition != "" {
		t.Fatalf("status with no goal = %+v, want inactive/empty", res)
	}

	if err := s.RegisterGoal("ship the feature"); err != nil {
		t.Fatal(err)
	}
	res = runGoalToolAction(t, s, `{"action":"status"}`)
	if !res.Active || res.Condition != "ship the feature" {
		t.Fatalf("status with active goal = %+v, want active/\"ship the feature\"", res)
	}
}

func TestGoalToolSetArms(t *testing.T) {
	s := newGoalToolSession(t)

	res := runGoalToolAction(t, s, `{"action":"set","condition":"tests pass"}`)
	if !res.Active || res.Condition != "tests pass" {
		t.Fatalf("set result = %+v, want active/\"tests pass\"", res)
	}

	cond, ok := s.ActiveGoal()
	if !ok || cond != "tests pass" {
		t.Fatalf("ActiveGoal = %q, %v; want active with \"tests pass\"", cond, ok)
	}
}

func TestGoalToolSetWhileActiveSaysAdjust(t *testing.T) {
	s := newGoalToolSession(t)
	if err := s.RegisterGoal("original goal"); err != nil {
		t.Fatal(err)
	}

	msg := callGoalToolExpectError(t, s, `{"action":"set","condition":"a different goal"}`)
	if !strings.Contains(msg, "adjust") {
		t.Fatalf("set-while-active error = %q, want it to mention \"adjust\"", msg)
	}

	// The original goal must be untouched.
	cond, ok := s.ActiveGoal()
	if !ok || cond != "original goal" {
		t.Fatalf("ActiveGoal = %q, %v; want unchanged \"original goal\"", cond, ok)
	}
}

func TestGoalToolAdjust(t *testing.T) {
	s := newGoalToolSession(t)
	if err := s.RegisterGoal("original goal"); err != nil {
		t.Fatal(err)
	}

	res := runGoalToolAction(t, s, `{"action":"adjust","condition":"updated goal"}`)
	if !res.Active || res.Condition != "updated goal" {
		t.Fatalf("adjust result = %+v, want active/\"updated goal\"", res)
	}

	cond, ok := s.ActiveGoal()
	if !ok || cond != "updated goal" {
		t.Fatalf("ActiveGoal = %q, %v; want active with \"updated goal\"", cond, ok)
	}
}

func TestGoalToolAdjustRequiresActive(t *testing.T) {
	s := newGoalToolSession(t)
	msg := callGoalToolExpectError(t, s, `{"action":"adjust","condition":"anything"}`)
	if !strings.Contains(msg, "no active goal") {
		t.Fatalf("adjust-without-active error = %q, want it to mention no active goal", msg)
	}
}

func TestGoalToolRejectsUnknownAction(t *testing.T) {
	s := newGoalToolSession(t)
	if err := s.RegisterGoal("some goal"); err != nil {
		t.Fatal(err)
	}

	for _, action := range []string{"clear", "bogus", ""} {
		args, err := json.Marshal(map[string]string{"action": action})
		if err != nil {
			t.Fatal(err)
		}
		msg := callGoalToolExpectError(t, s, string(args))
		if !strings.Contains(msg, "unknown action") {
			t.Fatalf("action %q error = %q, want it to mention unknown action", action, msg)
		}
	}

	// Rejecting "clear" must not have touched the active goal.
	cond, ok := s.ActiveGoal()
	if !ok || cond != "some goal" {
		t.Fatalf("ActiveGoal = %q, %v; want unchanged \"some goal\"", cond, ok)
	}
}

func TestGoalToolAbsentWhenDisabled(t *testing.T) {
	off := NewSession(Config{})
	if _, ok := off.tools[goalToolName]; ok {
		t.Fatal("goal tool present with Config.GoalTool false, want absent")
	}
	for _, d := range off.toolDefs(context.Background()) {
		if d.Name == goalToolName {
			t.Fatal("goal tool advertised in toolDefs with Config.GoalTool false")
		}
	}

	on := NewSession(Config{GoalTool: true})
	if _, ok := on.tools[goalToolName]; !ok {
		t.Fatal("goal tool absent with Config.GoalTool true, want present")
	}
	var found bool
	for _, d := range on.toolDefs(context.Background()) {
		if d.Name == goalToolName {
			found = true
		}
	}
	if !found {
		t.Fatal("goal tool not advertised in toolDefs with Config.GoalTool true")
	}
}
