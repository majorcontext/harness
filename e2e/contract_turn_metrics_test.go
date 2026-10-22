package e2e

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// logLines decodes the JSON log lines of stderr whose msg is msg.
func logLines(stderr, msg string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(stderr, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// awaitLogLines returns the lines of msg once stderr holds n of them.
func awaitLogLines(t *testing.T, d interface{ Stderr() string }, msg string, n int) []map[string]any {
	t.Helper()
	if !testpoll.UntilNoT(waitBound, func() bool { return len(logLines(d.Stderr(), msg)) >= n }) {
		t.Fatalf("stderr holds %d %q lines, want %d\n%s", len(logLines(d.Stderr(), msg)), msg, n, d.Stderr())
	}
	return logLines(d.Stderr(), msg)
}

func wantFields(t *testing.T, what string, rec map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got, ok := rec[k]; !ok || got != v {
			t.Errorf("%s: %s = %v (present %v), want %v\n%v", what, k, got, ok, v, rec)
		}
	}
}

func wantNumbers(t *testing.T, what string, rec map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := rec[k].(float64); !ok {
			t.Errorf("%s: %s = %v, want a number\n%v", what, k, rec[k], rec)
		}
	}
}

func wantAbsent(t *testing.T, what string, rec map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := rec[k]; ok {
			t.Errorf("%s: key %s is present, want none\n%v", what, k, rec)
		}
	}
}

func wantPurposes(t *testing.T, lines []map[string]any, want map[string]int) {
	t.Helper()
	got := map[string]int{}
	for _, line := range lines {
		purpose, _ := line["purpose"].(string)
		got[purpose]++
	}
	if !maps.Equal(got, want) {
		t.Fatalf("turn_metrics purposes = %v, want %v", got, want)
	}
}

func wantPurposeFields(t *testing.T, lines []map[string]any, sessionID string) {
	t.Helper()
	for _, line := range lines {
		switch line["purpose"] {
		case "turn":
			wantFields(t, "turn", line, map[string]any{"session_id": sessionID, "model": "anthropic/claude-fable-5"})
		case "title":
			wantFields(t, "title", line, map[string]any{"session_id": sessionID, "model": "anthropic/title-model", "effort": "off", "tools_count": 0.0})
		case "generate":
			wantFields(t, "generate", line, map[string]any{"session_id": "", "model": "anthropic/claude-fable-5", "tools_count": 0.0})
			wantAbsent(t, "generate", line, "service_tier", "effort")
		case "judge":
			wantFields(t, "judge", line, map[string]any{"session_id": sessionID, "model": "anthropic/eval-model", "effort": "off", "retry": 0.0, "tools_count": 0.0})
			wantNumbers(t, "judge", line, "system_len")
		case "compaction":
			wantFields(t, "compaction", line, map[string]any{"session_id": sessionID, "model": "anthropic/claude-fable-5", "tools_count": 0.0})
			wantNumbers(t, "compaction", line, "system_len")
		default:
			t.Fatalf("unexpected turn_metrics purpose: %v", line["purpose"])
		}
	}
}

func TestContractTurnMetricsLineOfEachModelCall(t *testing.T) {
	skipShort(t)
	t.Parallel()
	agentRequest := func(r harnesstest.Request) bool { return notEvaluator(r) && notTitleRequest(r) && notGenerate(r) }
	fake := harnesstest.NewAnthropic(t,
		harnesstest.Step{Name: "broken", Match: agentRequest, Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "0", ErrorMessage: "upstream broke"}},
		harnesstest.Step{Name: "call", Match: matchAll(agentRequest, harnesstest.LastUserText("go")), Reply: harnesstest.Reply{
			ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "fixture_generate", Input: map[string]any{"system": generateSystem, "text": "hello"}}},
			Usage:     harnesstest.Usage{Input: 11, Output: 7}}},
		harnesstest.Step{Name: "after", Match: matchAll(agentRequest, harnesstest.LastToolResult("fixture_generate")), Reply: harnesstest.Reply{Text: "done", Usage: harnesstest.Usage{Input: 13, Output: 5}}},
		harnesstest.Step{Name: "title", Match: isTitleRequest, Reply: harnesstest.Reply{Text: "Session title"}},
		harnesstest.Step{Name: "generate", Match: isToollessGenerate, Reply: harnesstest.Reply{Text: "Generated reply"}},
		agentStep("goal work", "done", false),
		evaluatorStep("judge", "MET: said done", false),
		harnesstest.Step{Name: "summary", Match: harnesstest.LastUserText("Summarize the conversation above"), Reply: harnesstest.Reply{Text: "Summary"}},
	)
	cfg := pluginConfig(t, map[string]any{"host_calls": true})
	cfg["title_model"], cfg["compaction_keep_turns"] = "anthropic/title-model", 1
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(cfg)), nil, t.TempDir())
	id := d.Create(t)
	d.Submit(t, id, "go")
	d.WaitIdle(t, id)
	if !fake.AwaitRequests(5, waitBound) {
		t.Fatalf("model requests = %d, want five for turn, title, and generate calls", len(fake.Requests()))
	}
	testpoll.Until(t, waitBound, "the title call did not settle", func() bool {
		body, _ := d.GetSession(t, id).Body.(map[string]any)
		return body["title"] == "Session title"
	})
	initial := logLines(d.Stderr(), "turn_metrics")
	wantPurposes(t, initial, map[string]int{"turn": 2, "title": 1, "generate": 1})
	if n := len(logLines(d.Stderr(), "startup_prewarm")); n != 0 {
		t.Errorf("startup_prewarm lines = %d, want 0: a backend that cannot warm logs none\n%s", n, d.Stderr())
	}
	wantPurposeFields(t, initial, id)
	var turnCalls []map[string]any
	for _, rec := range initial {
		if rec["purpose"] == "turn" {
			turnCalls = append(turnCalls, rec)
		}
	}
	if len(turnCalls) != 2 {
		t.Fatalf("turn-purpose lines = %d, want 2", len(turnCalls))
	}
	wantFields(t, "first call", turnCalls[0], map[string]any{"retry": 1.0, "input_tokens": 11.0, "output_tokens": 7.0, "cache_read_tokens": 0.0, "cache_write_tokens": 0.0})
	wantFields(t, "second call", turnCalls[1], map[string]any{"retry": 0.0, "input_tokens": 13.0, "output_tokens": 5.0, "cache_read_tokens": 0.0, "cache_write_tokens": 0.0})
	for _, rec := range initial {
		if rec["purpose"] != "turn" {
			continue
		}
		wantNumbers(t, "turn", rec, "ttft_ms", "stream_ms", "system_len", "tools_count")
		if rec["system_len"].(float64) == 0 || rec["tools_count"].(float64) == 0 {
			t.Errorf("turn system_len %v and tools_count %v, want the size of the request that the call sent", rec["system_len"], rec["tools_count"])
		}
		wantAbsent(t, "turn", rec, "service_tier", "effort", "request_mode", "chain_refusal")
	}
	d.SetGoal(t, id, "say done", 1, false)
	d.WaitIdle(t, id)
	testpoll.Until(t, waitBound, "the goal was not judged", func() bool {
		body, _ := d.GetSession(t, id).Body.(map[string]any)
		goal, _ := body["goal"].(map[string]any)
		return goal["state"] == "achieved"
	})
	withJudge := logLines(d.Stderr(), "turn_metrics")
	wantPurposes(t, withJudge, map[string]int{"turn": 3, "title": 1, "generate": 1, "judge": 1})
	wantPurposeFields(t, withJudge, id)
	if res := d.Compact(t, id); res.Status != http.StatusOK {
		t.Fatalf("compact = %d %v", res.Status, res.Body)
	}
	withCompaction := logLines(d.Stderr(), "turn_metrics")
	wantPurposes(t, withCompaction, map[string]int{"turn": 3, "title": 1, "generate": 1, "judge": 1, "compaction": 1})
	wantPurposeFields(t, withCompaction, id)
}

func TestContractTurnMetricsNamesTheTierAndTheEffortOfTheCall(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t, replyText("hi"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := d.Create(t)
	d.SetThinking(t, id, "high")
	d.SetServiceTier(t, id, "priority")
	d.Submit(t, id, "go")
	d.WaitIdle(t, id)
	lines := awaitLogLines(t, d, "turn_metrics", 1)
	wantFields(t, "call", lines[0], map[string]any{"effort": "high", "service_tier": "priority"})
}

func TestContractTurnMetricsLineOfAClaudeCodeTurn(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.NewAnthropic(t, replyText("unused"))
	d := claudeLane{mode: "normal"}.newDriver(t, serveHost, fake.URL())
	id := d.Create(t)
	d.Submit(t, id, "run it")
	d.WaitIdle(t, id)
	lines := awaitLogLines(t, d, "turn_metrics", 1)
	if len(lines) != 1 {
		t.Fatalf("turn_metrics lines = %d, want 1: the CLI turn is one model call\n%s", len(lines), d.Stderr())
	}
	wantFields(t, "turn", lines[0], map[string]any{"session_id": id, "model": "claude-code/sonnet", "retry": 0.0,
		"ttft_ms": 50.0, "stream_ms": 350.0, "input_tokens": 101.0, "output_tokens": 42.0, "cache_read_tokens": 7.0, "cache_write_tokens": 5.0})
	wantFields(t, "turn", lines[0], map[string]any{"system_len": 0.0, "tools_count": 0.0})
	wantAbsent(t, "turn", lines[0], "service_tier", "effort", "request_mode", "chain_refusal")
}

func TestContractTurnMetricsLineOnTheStderrOfRun(t *testing.T) {
	skipShort(t)
	t.Parallel()
	h := newCLIHost(t, nil, replyText("hello"))
	out, errOut, code := h.run("run", "-p", "hi")
	if code != 0 || out != "hello\n" {
		t.Fatalf("run = %d %q, want 0 and the reply alone on stdout\n%s", code, out, errOut)
	}
	lines := logLines(errOut, "turn_metrics")
	if len(lines) != 1 {
		t.Fatalf("turn_metrics lines on stderr = %d, want 1\n%s", len(lines), errOut)
	}
	wantFields(t, "call", lines[0], map[string]any{"session_id": sessionID(t, errOut), "retry": 0.0})
}

func TestContractStartupPrewarmLinesOfACodexSession(t *testing.T) {
	skipShort(t)
	t.Parallel()
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey}, codexHi...)
	d := serveHost.newDriver(t, o.URL(), codexConfig(o.URL(), true, nil))
	id := d.Create(t)
	d.Submit(t, id, "hello")
	d.WaitIdle(t, id)
	prewarms := awaitLogLines(t, d, "startup_prewarm", 3)
	var statuses []string
	for _, rec := range prewarms {
		statuses = append(statuses, rec["status"].(string))
		wantFields(t, "startup_prewarm", rec, map[string]any{"session_id": id})
		wantNumbers(t, "startup_prewarm", rec, "duration_ms", "age_ms")
	}
	if got, want := strings.Join(statuses, " "), "started ready consumed"; got != want {
		t.Errorf("startup_prewarm statuses = %q, want %q\n%s", got, want, d.Stderr())
	}
	turns := awaitLogLines(t, d, "turn_metrics", 1)
	wantFields(t, "call", turns[0], map[string]any{"request_mode": "incremental", "previous_response_used": true, "chain_recovered": false})
	wantNumbers(t, "call", turns[0], "complete_input_items", "sent_input_items")
}
