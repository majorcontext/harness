package e2e

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/httpapi"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

var retryOutputRows = map[string]runtimeRow{
	"retry_policy_keeps_retries_after_tool_only":                runtimeOnly("embedded-retry-policy"),
	"retry_policy_stops_retries_after_truncated_tool_start":     runtimeOnly("embedded-retry-policy"),
	"retry_policy_allows_pause_continuation_after_output":       runtimeOnly("embedded-retry-policy"),
	"retry_policy_stops_http_retries_during_pause_continuation": runtimeOnly("embedded-retry-policy"),
	"retry_policy_honors_cancellation_after_visible_text":       runtimeOnly("embedded-retry-policy"),
	"retry_policy_resets_for_the_next_turn":                     runtimeOnly("embedded-retry-policy"),
	"retry_policy_keeps_retries_before_visible_text":            runtimeOnly("embedded-retry-policy"),
	"retry_policy_stops_retries_after_reasoning_only":           runtimeOnly("embedded-retry-policy"),
	"retry_policy_stops_retries_after_visible_text":             runtimeOnly("embedded-retry-policy"),
	"retry_policy_allows_retries_on_later_transport_errors":     runtimeOnly("embedded-retry-policy"),
	"retry_policy_allows_retries_on_later_model_calls":          runtimeOnly("embedded-retry-policy"),
	"retry_policy_default_retries_after_visible_text":           sameAsServe(),
}

func TestContractRetryAfterOutput(t *testing.T) {
	var rows []scenario
	for _, mode := range []string{"before_visible_text", "after_reasoning_only", "after_tool_only", "after_visible_text", "across_model_calls", "across_transport_errors", "default"} {
		first := harnesstest.Reply{StopReason: "max_tokens"}
		if mode == "after_reasoning_only" {
			first.Reasoning = "thinking"
		}
		if mode == "after_visible_text" || mode == "default" || mode == "across_model_calls" || mode == "across_transport_errors" {
			first.Text = "partial"
		}
		steps := []harnesstest.Step{{Name: "first", Reply: first}}
		wantCalls := 2
		switch mode {
		case "across_model_calls", "across_transport_errors", "after_tool_only":
			wantCalls = 3
			steps[0].Reply.StopReason = "tool_use"
			steps[0].Reply.ToolCalls = []harnesstest.ToolCall{{ID: "toolu_1", Name: "no_such_tool", Input: map[string]any{}}}
			failure := harnesstest.Reply{HTTPStatus: 500}
			if mode == "across_transport_errors" {
				failure = harnesstest.Reply{Hangup: true}
			}
			steps = append(steps, harnesstest.Step{Name: "failure", Reply: failure})
		case "after_visible_text", "after_reasoning_only":
			wantCalls = 1
		}
		steps = append(steps, harnesstest.Step{Name: "answer", Repeat: true, Reply: harnesstest.Reply{Text: "done"}})
		name := "retry_policy_"
		switch mode {
		case "before_visible_text", "after_tool_only":
			name += "keeps_retries_" + mode
		case "after_reasoning_only":
			name += "stops_retries_after_reasoning_only"
		case "after_visible_text":
			name += "stops_retries_after_visible_text"
		case "across_model_calls":
			name += "allows_retries_on_later_model_calls"
		case "across_transport_errors":
			name += "allows_retries_on_later_transport_errors"
		case "default":
			name += "default_retries_after_visible_text"
		}
		row := scenario{name: name, config: map[string]any{"stream_max_retries": 1, "request_max_retries": 1}, model: steps,
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, retryOutputCalls{want: wantCalls}, recordLastTurn{as: "a"}}}
		if mode != "default" {
			row.driver = retryOutputDriver
		}
		rows = append(rows, row)
	}
	rows = append(rows, scenario{name: "retry_policy_stops_retries_after_truncated_tool_start", driver: retryOutputDriver,
		model: []harnesstest.Step{
			{Name: "cut", Reply: harnesstest.Reply{TruncateToolStart: true, ToolCalls: []harnesstest.ToolCall{{ID: "toolu_cut", Name: "no_such_tool", Input: map[string]any{}}}}},
			{Name: "unexpected_retry", Repeat: true, Reply: harnesstest.Reply{Text: "wrong"}},
		},
		actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, retryOutputCalls{want: 1}, recordLastTurn{as: "a"}},
	})
	rows = append(rows, retryOutputLifecycleRows()...)
	rows = append(rows, retryOutputPauseRows()...)
	runScenarios(t, rows)
}

func retryOutputLifecycleRows() []scenario {
	return []scenario{
		{name: "retry_policy_honors_cancellation_after_visible_text", driver: retryOutputDriver,
			model:   []harnesstest.Step{{Name: "blocked", Reply: harnesstest.Reply{Text: "partial", Block: true}}},
			actions: []action{create{as: "a"}, retryOutputCancel{}, retryOutputCalls{want: 1}, recordLastTurn{as: "a"}}},
		{name: "retry_policy_resets_for_the_next_turn", driver: retryOutputDriver,
			model: []harnesstest.Step{
				{Name: "first", Reply: harnesstest.Reply{Text: "done"}},
				{Name: "retry", Reply: harnesstest.Reply{HTTPStatus: 500}},
				{Name: "second", Reply: harnesstest.Reply{Text: "done again"}},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"},
				submit{as: "a", text: "again"}, waitIdle{as: "a"}, retryOutputCalls{want: 3}, recordLastTurn{as: "a"}}},
	}
}

func retryOutputPauseRows() []scenario {
	var rows []scenario
	for _, fails := range []bool{false, true} {
		name := "retry_policy_allows_pause_continuation_after_output"
		reply := harnesstest.Reply{Text: "done"}
		if fails {
			name = "retry_policy_stops_http_retries_during_pause_continuation"
			reply = harnesstest.Reply{HTTPStatus: 500}
		}
		rows = append(rows, scenario{name: name, driver: retryOutputPauseDriver,
			model: []harnesstest.Step{
				{Name: "first", Reply: pauseLeg("srvtoolu_1", false)},
				{Name: "continuation", Reply: reply},
				{Name: "unexpected_retry", Repeat: true, Reply: harnesstest.Reply{Text: "wrong"}},
			},
			actions: []action{create{as: "a"}, submit{as: "a", text: "go"}, waitIdle{as: "a"}, retryOutputCalls{want: 2}, recordLastTurn{as: "a"}},
		})
	}
	return rows
}

func retryOutputDriver(t *testing.T, h host, modelURL string) driver {
	return retryOutputConfiguredDriver(t, h, modelURL, false)
}

func retryOutputPauseDriver(t *testing.T, h host, modelURL string) driver {
	return retryOutputConfiguredDriver(t, h, modelURL, true)
}

func retryOutputConfiguredDriver(t *testing.T, h host, modelURL string, search bool) driver {
	t.Helper()
	d := h.newDriver(t, modelURL, map[string]any{"stream_max_retries": 1, "request_max_retries": 1, "web": map[string]any{"search": search}}).(*runtimeDriver)
	d.stop(t, context.Background())
	rt, err := newWired(t, harness.Options{Store: store.NewDiskStore(d.store), Config: d.cfg, WorkDir: d.workDir, Version: runtimeVersion,
		Getenv: d.getenv, DisableRetryAfterOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	d.rt, d.srv = rt, httptest.NewServer(httpapi.Handler(rt))
	return d
}

type retryOutputCancel struct{}

func (retryOutputCancel) run(t *testing.T, r *run) {
	t.Helper()
	s, err := r.drv.(*runtimeDriver).rt.Open(t.Context(), r.id(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "cancel", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	text := ""
	for e, err := range stream.Events(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == protocol.KindItemDelta {
			text += decodeEvent[protocol.ItemFrame](t, e).Text
			if err := stream.Cancel(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, err := stream.Wait(t.Context())
	if err != nil || result.StopReason != protocol.StopInterrupted || text != "partial" {
		t.Fatalf("canceled turn = %+v, %v, streamed text = %q", result, err, text)
	}
}

type retryOutputCalls struct{ want int }

func (a retryOutputCalls) run(t *testing.T, r *run) {
	t.Helper()
	if got := len(r.fake.Requests()); got != a.want {
		t.Fatalf("model requests = %d, want %d under retry-after-output policy", got, a.want)
	}
}
