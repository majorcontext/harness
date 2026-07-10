package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestAnswerRecoversPendingResumeAfterCrash is the red-first test for issue
// #64 item 2: engine.Session.PendingResumeAnswer (design doc §3's
// answered-but-never-resumed crash window) exists and is tested at the
// engine layer, but nothing on the server consumed it — a process death
// between POST /answer's atomic claim persisting question.answered and the
// resumed goal worker's first Prompt call ever appending a message left the
// answer stranded: durably recorded on disk, restored as goalActive true,
// but never actually delivered, so the next resume would replay the bare
// condition and the answer would be silently lost.
//
// This constructs that exact journal shape directly (per the issue's own
// red-test guidance: "question.asked + question.answered-with-answer + no
// subsequent worker message") using the engine package straight — pausing a
// goal on ask_user and answering it, but deliberately never calling
// PursueGoal again, so no worker message ever follows question.answered.
// A fresh Server loading that log (simulating the restart after the crash)
// must, on a retried POST /answer for the same session, recover the
// recorded answer and deliver it as turn 1's directive — without writing a
// second question.answered record (the answer was already durably
// recorded; POST /answer must not re-record it).
func TestAnswerRecoversPendingResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	model := message.ModelRef{Provider: "test", Model: "m1"}

	// --- Build the crash fixture: pause on ask_user, answer it, stop. ---
	fixtureProv := &scriptedProvider{name: "test", turns: [][]provider.Event{
		askUserTurn("tc1", `{"questions":[{"question":"Which environment?"}]}`),
	}}
	seed := engine.NewSession(engine.Config{
		Providers:  provider.Registry{fixtureProv.Name(): fixtureProv},
		Model:      model,
		SessionDir: dir,
	})
	seed.ID = "ses_00000000c00a5b00"
	id := seed.ID
	if err := seed.RegisterGoal("deploy the service"); err != nil {
		t.Fatalf("seed RegisterGoal: %v", err)
	}
	if _, err := seed.Prompt(context.Background(), "deploy the service"); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}
	if _, awaiting := seed.AwaitingQuestion(); !awaiting {
		t.Fatal("seed session not awaiting a question after ask_user turn")
	}
	answerText := formatAnswers([]struct {
		Question string   `json:"question"`
		Selected []string `json:"selected,omitempty"`
		Text     string   `json:"text,omitempty"`
	}{{Question: "Which environment?", Selected: []string{"staging"}}})
	won, hadPending := seed.AnswerQuestion("tc1", answerText)
	if !won || !hadPending {
		t.Fatalf("seed AnswerQuestion = (%v, %v), want (true, true)", won, hadPending)
	}
	// The crash: nothing else ever runs on this engine.Session. Its log now
	// carries goal.set, the ask_user tool-call/result messages,
	// question.asked, and question.answered — with no worker message after
	// the answer.
	logBytes, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logBytes), `"type":"question.answered"`) != 1 {
		t.Fatalf("fixture log = %s, want exactly one question.answered", logBytes)
	}

	// --- "Process 2": a fresh server over the same dir. ---
	prov := &goalProv{
		name:   "test",
		worker: [][]provider.Event{asstTurn("deployed to staging")},
		eval:   [][]provider.Event{asstTurn("MET: deployment confirmed")},
	}
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.GoalEvaluator = message.ModelRef{Provider: prov.Name(), Model: "eval"}
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	h := &harness{t: t, dir: dir, token: "secret-run-token", srv: srv, ts: ts}

	sse := h.openSSE("?from=0", "")

	// The recovery action: a retried POST /answer (the natural orchestrator
	// response to a request whose connection died before a response ever
	// arrived) for the very question that was already durably answered.
	resp, data := h.do("POST", "/session/"+id+"/answer", map[string]any{
		"call_id": "tc1",
		"answers": []map[string]any{{"question": "Which environment?", "selected": []string{"staging"}}},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer (crash recovery) status %d: %s", resp.StatusCode, data)
	}

	evs := sse.collectUntilIdle(t)
	var sawAchieved bool
	for _, ev := range evs {
		if ev.Type == "goal.achieved" {
			sawAchieved = true
		}
		if ev.Type == "question.answered" {
			t.Errorf("a SECOND question.answered was journaled by the recovery /answer call: %v", evs)
		}
	}
	if !sawAchieved {
		t.Fatalf("goal never achieved after crash-recovery resume: %v", evs)
	}

	resp, data = h.do("GET", "/session/"+id+"/message", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET message status %d: %s", resp.StatusCode, data)
	}
	var msgs []struct {
		Role  string `json:"role"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(data, &msgs); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "deploy the service") && strings.Contains(p.Text, "staging") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no resumed directive message carrying both the condition and the recovered answer text: %s", data)
	}

	// Exactly one question.answered record total, in the FINAL log.
	logBytes, err = os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(logBytes), `"type":"question.answered"`); n != 1 {
		t.Errorf("question.answered records = %d, want exactly 1", n)
	}
}
