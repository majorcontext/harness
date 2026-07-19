package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateGoalRequiresActive(t *testing.T) {
	s := NewSession(Config{})
	err := s.UpdateGoal("some new condition")
	if err == nil {
		t.Fatal("UpdateGoal on an inactive goal should error")
	}
	if !strings.Contains(err.Error(), "no active goal") {
		t.Fatalf("error = %q, want it to mention no active goal", err.Error())
	}
}

func TestUpdateGoalRewritesConditionJournalsAndEmits(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.RegisterGoal("original condition"); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	if err := s.UpdateGoal("new condition"); err != nil {
		t.Fatalf("UpdateGoal = %v", err)
	}

	cond, ok := s.ActiveGoal()
	if !ok || cond != "new condition" {
		t.Errorf("ActiveGoal = %q, %v; want active with new condition", cond, ok)
	}

	var sawEvent bool
	for _, ev := range evs {
		if ev.Type == EventGoalUpdated {
			sawEvent = true
			if ev.GoalCondition != "new condition" {
				t.Errorf("EventGoalUpdated.GoalCondition = %q, want %q", ev.GoalCondition, "new condition")
			}
		}
	}
	if !sawEvent {
		t.Error("EventGoalUpdated was not emitted")
	}

	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, `"type":"goal.updated"`) || !strings.Contains(log, `"condition":"new condition"`) {
		t.Fatalf("log missing goal.updated record with new condition: %s", log)
	}
}

func TestUpdateGoalSameConditionNoop(t *testing.T) {
	dir := t.TempDir()
	s := NewSession(Config{SessionDir: dir})
	if err := s.RegisterGoal("same condition"); err != nil {
		t.Fatal(err)
	}
	var evs []Event
	s.cfg.OnEvent = func(ev Event) { evs = append(evs, ev) }

	if err := s.UpdateGoal("same condition"); err != nil {
		t.Fatalf("UpdateGoal = %v, want nil for a same-condition update", err)
	}
	for _, ev := range evs {
		if ev.Type == EventGoalUpdated {
			t.Error("EventGoalUpdated emitted for a same-condition update")
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, s.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "goal.updated") {
		t.Fatalf("log has a goal.updated record for a same-condition update: %s", string(data))
	}
}

func TestUpdateGoalEmptyConditionRejected(t *testing.T) {
	s := NewSession(Config{})
	if err := s.RegisterGoal("original condition"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGoal("   "); err == nil {
		t.Fatal("UpdateGoal with a whitespace-only condition should error")
	}
	cond, ok := s.ActiveGoal()
	if !ok || cond != "original condition" {
		t.Errorf("ActiveGoal = %q, %v; want unchanged original condition", cond, ok)
	}
}
