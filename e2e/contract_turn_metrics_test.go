package e2e

import (
	"encoding/json"
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

func TestContractTurnMetricsLineOfEachModelCall(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t,
		harnesstest.Step{Name: "broken", Reply: harnesstest.Reply{HTTPStatus: 500, ErrorMessage: "upstream broke"}},
		harnesstest.Step{Name: "call", Reply: harnesstest.Reply{
			ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash", Input: map[string]any{"command": "echo hi"}}},
			Usage:     harnesstest.Usage{Input: 11, Output: 7}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "done", Usage: harnesstest.Usage{Input: 13, Output: 5}}},
	)
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(nil)), nil, t.TempDir())
	id := d.Create(t)
	d.Submit(t, id, "go")
	d.WaitIdle(t, id)
	lines := awaitLogLines(t, d, "turn_metrics", 2)
	if len(lines) != 2 {
		t.Fatalf("turn_metrics lines = %d, want 2: a model call that failed before its stream ended logs none\n%s", len(lines), d.Stderr())
	}
	if n := len(logLines(d.Stderr(), "startup_prewarm")); n != 0 {
		t.Errorf("startup_prewarm lines = %d, want 0: a backend that cannot warm logs none\n%s", n, d.Stderr())
	}
	common := map[string]any{"session_id": id, "model": "anthropic/claude-fable-5", "cache_read_tokens": 0.0, "cache_write_tokens": 0.0}
	wantFields(t, "first call", lines[0], map[string]any{"retry": 1.0, "input_tokens": 11.0, "output_tokens": 7.0})
	wantFields(t, "second call", lines[1], map[string]any{"retry": 0.0, "input_tokens": 13.0, "output_tokens": 5.0})
	for i, rec := range lines {
		what := []string{"first call", "second call"}[i]
		wantFields(t, what, rec, common)
		wantNumbers(t, what, rec, "ttft_ms", "stream_ms", "system_len", "tools_count")
		if rec["system_len"].(float64) == 0 || rec["tools_count"].(float64) == 0 {
			t.Errorf("%s: system_len %v and tools_count %v, want the size of the request that the call sent", what, rec["system_len"], rec["tools_count"])
		}
		wantAbsent(t, what, rec, "service_tier", "effort", "request_mode", "chain_refusal")
	}
}

func TestContractTurnMetricsNamesTheTierAndTheEffortOfTheCall(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("hi"))
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
	fake := harnesstest.New(t, replyText("unused"))
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
