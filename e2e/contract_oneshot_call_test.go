package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func TestContractOneShotCallLineOfATitleCall(t *testing.T) {
	skipShort(t)
	t.Parallel()
	const prompt = "please look at why login is flaky"
	const boxRule = "Box rule: keep the work directory clean."
	const token = "sk-live-0123456789abcdef"
	extra := func(model string) map[string]any {
		return map[string]any{"title_model": model, "append_system_prompt": []string{boxRule}}
	}
	modelAPI := func(t *testing.T, url string) driver {
		return newServeDriverIn(t, writeGoalConfigWith(t, url, scenarioConfig(extra("anthropic/title-model"))), nil, t.TempDir())
	}
	rows := []struct {
		name, model, answer string
		reply               harnesstest.Reply
		sendsBox, fails     bool
		driver              func(t *testing.T, url string) driver
	}{
		{name: "model API", model: "anthropic/title-model", answer: "\n\"Dinner plans for two\"\nsecond line", driver: modelAPI,
			reply: harnesstest.Reply{Text: "\n\"Dinner plans for two\"\nsecond line", Usage: harnesstest.Usage{Input: 11, Output: 7}}},
		{name: "model API failure", model: "anthropic/title-model", fails: true, driver: modelAPI,
			reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "bad request, Authorization: Bearer " + token}},
		{name: "claude code", model: "claude-code/sonnet", answer: "Claude answer.", sendsBox: true,
			reply: harnesstest.Reply{Text: "unused"},
			driver: func(t *testing.T, url string) driver {
				return claudeLane{mode: "text_only", extra: extra("claude-code/sonnet")}.newDriver(t, serveHost, url)
			}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fake := harnesstest.NewAnthropic(t,
				harnesstest.Step{Name: "title", Match: isTitleRequest, Reply: row.reply, Repeat: true},
				harnesstest.Step{Name: "turn", Match: notTitleRequest, Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			)
			d := row.driver(t, fake.URL())
			id := d.Create(t)
			d.Submit(t, id, prompt)
			d.WaitIdle(t, id)
			lines := awaitLogLines(t, d, "oneshot_call", 1)
			if len(lines) != 1 {
				t.Fatalf("oneshot_call lines = %d, want 1 for the one title call\n%s", len(lines), d.Stderr())
			}
			rec := lines[0]
			wantFields(t, "title call", rec, map[string]any{"session_id": id, "model": row.model, "answer": row.answer})
			wantNumbers(t, "title call", rec, "duration_ms", "system_bytes", "input_tokens", "output_tokens")
			if errText, _ := rec["error"].(string); row.fails != (errText != "") || strings.Contains(errText, token) {
				t.Errorf("error = %q, want present %v and no credential", errText, row.fails)
			}
			system, _ := rec["system"].(string)
			if !strings.Contains(system, "Generate a short title for a conversation") {
				t.Errorf("system = %q, want the title instructions that the call sent", system)
			}
			if got := strings.Contains(system, boxRule); got != row.sendsBox {
				t.Errorf("system holds append_system_prompt = %v, want %v: the line names what the backend sent\n%q", got, row.sendsBox, system)
			}
			msgs, _ := rec["messages"].([]any)
			if len(msgs) != 1 {
				t.Fatalf("messages = %v, want the one user message of the call", rec["messages"])
			}
			wantFields(t, "title message", msgs[0].(map[string]any), map[string]any{"role": "user", "text": prompt})
		})
	}
}
