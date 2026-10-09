package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

func evaluatorTranscript(t *testing.T, fake *harnesstest.Server) string {
	t.Helper()
	for _, r := range fake.Requests() {
		if isEvaluator(r) {
			return r.LastUserText()
		}
	}
	t.Fatal("no evaluator request reached the model server")
	return ""
}

func TestContractGoalEvaluatorTranscript(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("reasoning and attachments read as the engine rendered them", func(t *testing.T) {
			t.Parallel()
			fake, config := scenarioFake(t, scenario{chat: true, model: []harnesstest.Step{
				{Name: "look", Match: notEvaluator, Reply: harnesstest.Reply{Reasoning: "weigh the options", Text: "seen"}},
				agentStep("work", "done", true),
				evaluatorStep("judge", "MET: ok", true),
			}})
			d := h.newDriver(t, fake.URL(), config).(*runtimeDriver)
			id := d.Create(t)
			d.Attach(t, id, "look", rowAttachments())
			d.WaitIdle(t, id)
			d.SetGoal(t, id, "say done", 3, false)
			d.WaitIdle(t, id)
			got := evaluatorTranscript(t, fake)
			for _, want := range []string{"[reasoning] weigh the options\nseen\n", "[blob image/png]\n", "[blob application/pdf]\n"} {
				if !strings.Contains(got, want) {
					t.Errorf("evaluator transcript lacks %q:\n%s", want, got)
				}
			}
		})

		t.Run("a cut part and an omitted conversation carry the notices of the engine", func(t *testing.T) {
			t.Parallel()
			part := strings.Repeat("x", 4097)
			d, fake := startOn(t, h, t.TempDir(), map[string]any{"context_window_tokens": 0},
				harnesstest.Step{Name: "reply", Match: notEvaluator, Reply: harnesstest.Reply{Text: part}, Repeat: true},
				evaluatorStep("judge", "MET: ok", true))
			id := d.Create(t)
			d.Submit(t, id, "first-marker")
			d.WaitIdle(t, id)
			for range 20 {
				d.Submit(t, id, part)
				d.WaitIdle(t, id)
			}
			d.SetGoal(t, id, "say done", 3, false)
			d.WaitIdle(t, id)
			got := evaluatorTranscript(t, fake)
			if !strings.Contains(got, "\n\nCONVERSATION TRANSCRIPT:\n[earlier conversation omitted to fit the evaluator's context budget]\n\nUSER:\n") {
				t.Errorf("evaluator transcript does not open with the omission notice of the engine:\n%.400s", got)
			}
			if strings.Contains(got, "first-marker") {
				t.Errorf("evaluator transcript holds the oldest message, which the budget omits")
			}
			if cut := strings.Repeat("x", 4096) + "…[truncated]\n"; !strings.Contains(got, cut) || strings.Contains(got, strings.Repeat("x", 4097)) {
				t.Errorf("a part over 4096 bytes does not end with the cut marker of the engine:\n%.400s", got)
			}
		})
	})
}

// evaluatorBudget runs turns turns of a part over the 4096-byte cap, sets a
// goal, and returns the transcript that the evaluator read.
func evaluatorBudget(t *testing.T, h host, extra map[string]any, turns int) string {
	t.Helper()
	part := strings.Repeat("x", 4097)
	d, fake := startOn(t, h, t.TempDir(), extra,
		harnesstest.Step{Name: "reply", Match: notEvaluator, Reply: harnesstest.Reply{Text: part}, Repeat: true},
		evaluatorStep("judge", "MET: ok", true))
	id := d.Create(t)
	for range turns {
		d.Submit(t, id, part)
		d.WaitIdle(t, id)
	}
	d.SetGoal(t, id, "say done", 3, false)
	d.WaitIdle(t, id)
	_, body, _ := strings.Cut(evaluatorTranscript(t, fake), "CONVERSATION TRANSCRIPT:\n")
	return body
}

func TestContractGoalEvaluatorTranscriptBudget(t *testing.T) {
	skipShort(t)
	const pair = 8241 // the user and the assistant block of one turn with a cut part
	onHosts(t, func(t *testing.T, h host) {
		t.Run("an evaluator model with no known window reads the floor budget of the engine", func(t *testing.T) {
			t.Parallel()
			const budget = (16000/2 - 2048) * 4
			body := evaluatorBudget(t, h, map[string]any{"context_window_tokens": 0}, 8)
			if !strings.HasPrefix(body, "[earlier conversation omitted to fit the evaluator's context budget]\n\n") {
				t.Errorf("transcript does not open with the omission notice:\n%.200s", body)
			}
			if len(body) > budget || len(body) < budget-pair {
				t.Errorf("transcript = %d bytes, want between %d and %d", len(body), budget-pair, budget)
			}
		})

		t.Run("an evaluator model reads half its window less the reserve", func(t *testing.T) {
			t.Parallel()
			const budget = (120000/2 - 2048) * 4
			body := evaluatorBudget(t, h, map[string]any{"context_window_tokens": 120000, "compaction_threshold": 0.99}, 32)
			if len(body) > budget || len(body) < budget-pair {
				t.Errorf("transcript = %d bytes, want between %d and %d", len(body), budget-pair, budget)
			}
		})
	})
}

func TestContractAgentProfileWithAnUnknownKeyAndAnotherError(t *testing.T) {
	skipShort(t)
	spawnAndRead := func(t *testing.T, h host, file string) string {
		t.Helper()
		d, _ := startOn(t, h, runtimeWorkdir(t, map[string]string{".agents/w.md": file}), nil,
			taskStep("spawn", userStarts("spawn"), fixed(spawn("w", "a"))),
			harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true})
		id := d.Create(t)
		d.Submit(t, id, "spawn it")
		d.WaitIdle(t, id)
		results := toolResults(d.Messages(t, id))
		if len(results) != 1 || !results[0].IsError {
			t.Fatalf("tool results = %+v, want one error result", results)
		}
		return results[0].Content
	}
	onHosts(t, func(t *testing.T, h host) {
		for _, tc := range []struct{ name, file, want string }{
			{"a missing name beside an unknown key", "---\nhooks: y\ndescription: W.\n---\n\nBody.\n", "frontmatter missing required 'name'"},
			{"a missing description beside an unknown key", "---\nname: w\nhooks: y\n---\n\nBody.\n", "frontmatter missing required 'description'"},
			{"a stray indented line after an inline unknown value beside an unknown key", "---\nname: w\ndescription: W.\nhooks: y\n  stray\n---\n\nBody.\n", "unexpected indented line"},
			{"a malformed line after the key beside an unknown key", "---\nname: w\ndescription: W.\nhooks: y\nstray\n---\n\nBody.\n", "malformed frontmatter line"},
			{"a repeated key after the key beside an unknown key", "---\nname: w\nhooks: y\ndescription: W.\nname: v\n---\n\nBody.\n", "duplicate frontmatter key"},
			{"a list under an unknown key", "---\nname: w\ndescription: W.\nhooks:\n  - a\n  - b\n---\n\nBody.\n", "unexpected indented line"},
			{"block text with no colon under an unknown key", "---\nname: w\ndescription: W.\nhooks: |\n  text without a colon\n---\n\nBody.\n", "unexpected indented line"},
		} {
			t.Run("the load fails on "+tc.name, func(t *testing.T) {
				t.Parallel()
				got := spawnAndRead(t, h, tc.file)
				if !strings.Contains(got, "agent definition") || !strings.Contains(got, "w.md") || !strings.Contains(got, tc.want) {
					t.Errorf("task result = %q, want a load failure of w.md that says %q", got, tc.want)
				}
			})
		}
		for _, tc := range []struct{ name, file string }{
			{"an unknown key alone", "---\nname: w\ndescription: W.\nhooks: y\n---\n\nBody.\n"},
			{"an unknown key with an inline list", "---\nname: w\ndescription: W.\nhooks: [a, b]\n---\n\nBody.\n"},
			{"an unknown key with a mapping under it", "---\nname: w\ndescription: W.\nhooks:\n  a: b\n---\n\nBody.\n"},
			{"an unknown key with block text that holds a colon", "---\nname: w\ndescription: W.\nhooks: |\n  text: with a colon\n---\n\nBody.\n"},
		} {
			t.Run(tc.name+" skips the file", func(t *testing.T) {
				t.Parallel()
				got := spawnAndRead(t, h, tc.file)
				if !strings.Contains(got, "unknown agent") {
					t.Errorf("task result = %q, want the refusal of an unknown agent", got)
				}
			})
		}
	})
}

func TestContractStartupPrewarmEndsWhenTheFirstTurnIsInterrupted(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Parallel()
		o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey, HoldPrewarm: true}, codexHi...)
		d := h.newDriver(t, o.URL(), codexConfig(o.URL(), true, nil)).(*runtimeDriver)
		id := d.Create(t)
		if !o.AwaitPrewarmHeld(waitBound) {
			t.Fatal("the session sent no warm-up")
		}
		d.Submit(t, id, "hello")
		d.Interrupt(t, id)
		if !o.AwaitPrewarmAbandoned(10 * time.Second) {
			t.Fatalf("the interrupt of the first turn left the warm-up running\n%s", d.Stderr())
		}
		if err := d.AwaitTurnEnd(id); err != nil {
			t.Fatal(err)
		}
		if n := len(o.Requests()); n != 0 {
			t.Errorf("model requests = %d, want 0: the interrupted turn never sent one", n)
		}
		d.Submit(t, id, "again")
		d.WaitIdle(t, id)
		if n := len(o.Requests()); n != 1 {
			t.Errorf("model requests = %d, want 1: the next turn runs with no warm-up", n)
		}
		if d.serve {
			lines := awaitLogLines(t, d, "startup_prewarm", 2)
			var statuses []string
			for _, rec := range lines {
				statuses = append(statuses, rec["status"].(string))
			}
			if got, want := strings.Join(statuses, " "), "started cancelled"; got != want {
				t.Errorf("startup_prewarm statuses = %q, want %q\n%s", got, want, d.Stderr())
			}
		}
	})
}

func TestContractPromptCommandWithAnUnknownKeyAndAnotherErrorListsTheUnknownKey(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Parallel()
		files := map[string]string{
			".agents/commands/stray.md": "---\ndescription: S.\nhooks: y\nstray\n---\nBody.\n",
			".agents/commands/ok.md":    "---\ndescription: O.\nhooks: y\n---\nBody.\n",
		}
		d, _ := startOn(t, h, runtimeWorkdir(t, files), nil, replyText("ok"))
		body := bodyOf(t, d.Commands(t))
		support, _ := body["serve_support"].(map[string]any)
		for _, name := range []string{"stray", "ok"} {
			entry, _ := support[name].(map[string]any)
			reason, _ := entry["reason"].(string)
			if entry["supported"] != false || !strings.Contains(reason, "unknown frontmatter key") || strings.Contains(reason, "malformed") {
				t.Errorf("serve_support[%s] = %v, want an unsupported entry whose reason is the unknown key", name, entry)
			}
		}
	})
}
