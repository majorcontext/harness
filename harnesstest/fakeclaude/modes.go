package main

import (
	"maps"
	"os"
	"time"
)

type mode func(f *fake)

func frames(fs ...obj) mode {
	return func(f *fake) { f.emit(fs...) }
}

func crashAfter(fs ...obj) mode {
	return func(f *fake) {
		f.emit(fs...)
		os.Exit(1)
	}
}

func mergeModes(ms ...map[string]mode) map[string]mode {
	out := map[string]mode{}
	for _, m := range ms {
		maps.Copy(out, m)
	}
	return out
}

var modes = mergeModes(basicModes, threadModes, stdinModes, childModes)

func normalTurn(f *fake) {
	f.emit(
		say("Let me check that."),
		assistant(toolUse("toolu_1", "Bash", obj{"command": "echo hi"})),
		user(toolResult("toolu_1", "hi\n", false)),
		say("Done — it printed hi."),
		success("Done — it printed hi.", 0, 0).
			set("usage", obj{"input_tokens": 101, "output_tokens": 42, "cache_read_input_tokens": 7, "cache_creation_input_tokens": 5}).
			set("total_cost_usd", 0.0123).set("ttft_ms", 50).set("duration_ms", 400),
	)
}

func perCallUsage(f *fake) {
	call := func(id string, block obj, cacheWrite, cacheRead, output int) obj {
		return assistant(block).inMessage("id", id).inMessage("usage", obj{
			"input_tokens": 2, "cache_creation_input_tokens": cacheWrite,
			"cache_read_input_tokens": cacheRead, "output_tokens": output,
		})
	}
	bash := func(n, cmd string) obj { return toolUse("toolu_"+n, "Bash", obj{"command": cmd}) }
	f.emit(
		call("msg_1", bash("1", "echo one"), 5545, 10234, 16),
		user(toolResult("toolu_1", "ok\n", false)),
		call("msg_2", bash("2", "echo two"), 104, 15779, 16),
		user(toolResult("toolu_2", "ok\n", false)),
		call("msg_3", textBlock("done"), 104, 15883, 39),
		success("done", 0, 0).set("num_turns", 3).
			set("usage", obj{"input_tokens": 6, "cache_creation_input_tokens": 5753, "cache_read_input_tokens": 41896, "output_tokens": 187}).
			set("modelUsage", obj{
				"claude-haiku-4-5-20251001": obj{"inputTokens": 1, "contextWindow": 200000},
				"claude-opus-5-5[1m]":       obj{"inputTokens": 6, "contextWindow": 1000000},
			}),
	)
}

func rateLimitTurn() mode {
	return func(f *fake) {
		info := obj{
			"status": "allowed", "resetsAt": 1788785267, "rateLimitType": "five_hour",
			"overageStatus": "allowed", "overageResetsAt": 1789000000, "isUsingOverage": false,
			"unifiedWindows": obj{
				"five_hour": obj{"utilization": 0.02, "resetsAt": 1788785267},
				"seven_day": obj{"utilization": 0.13, "resetsAt": 1789200000},
			},
		}
		f.emit(rateLimitEvent(info), say("Here is my answer."), success("Here is my answer.", 9, 4))
	}
}

func compactBoundary(f *fake) {
	f.emit(
		system("compact_boundary", obj{"compact_metadata": obj{"trigger": "auto", "pre_tokens": 123456}, "session_id": f.sessionID}),
		say("Continuing after compaction."),
		success("Continuing after compaction.", 12, 6),
	)
}

func hang(f *fake) {
	f.emit()
	time.Sleep(time.Hour)
}

// basicModes cover result classification and process exit. "crash" exits
// nonzero after init and is retryable, while "crash_before_init"
// (modes_question.go) exits before any frame and is not. "fast_no_drain"
// closes stdin before the driver writes, so a broken-pipe write must not
// fail a complete turn.
var basicModes = map[string]mode{
	"per_call_usage":           perCallUsage,
	"compact_after_window":     perCallUsage,
	"compact_boundary":         compactBoundary,
	"rate_limit_event":         rateLimitTurn(),
	"hang_after_text":          hangAfterText,
	"hang_after_listing":       hangAfterListing,
	"hang_in_tool":             hangInTool,
	"tool_on_interrupt":        hangAfterText,
	"tool_result_on_interrupt": hangAfterText,
	"success_on_interrupt":     hangAfterText,
	"placeholder_on_interrupt": hangAfterText,
	"exit_on_interrupt":        hangAfterText,
	"crash":                    crashAfter(),
	"fast_no_drain":            frames(say("Done before you finished writing."), success("Done before you finished writing.", 4, 6)),
	"error":                    frames(failed(11, 3, "fake failure")),
	"rate_limit_error":         frames(failed(6, 1, "rate_limit_error: please retry later")),
	"credential_error":         frames(failed(6, 1, "credential resolution failed: no credential for provider anthropic")),
}

func hangAfterText(f *fake) {
	f.emit(say("Working on it."))
	hang(f)
}

// hangInTool sends a text, then a text and a tool call of one API response,
// as the CLI does, and hangs while the tool runs.
func hangInTool(f *fake) {
	f.emit(say("Working on it.").inMessage("id", "msg_0"), say("Checking.").inMessage("id", "msg_A"),
		assistant(toolUse("toolu_h", "Bash", obj{"command": "sleep 60"})).inMessage("id", "msg_A"))
	hang(f)
}

// onInterrupt holds the frames that a mode prints after a SIGINT, before the
// error result. A list that ends with a result replaces the error result,
// and an empty list exits with no frame.
// They reach the driver only after it stopped reading the turn.
var onInterrupt = map[string][]obj{
	"tool_on_interrupt": {assistant(toolUse("toolu_i", "Bash", obj{"command": "sleep 60"}))},
	"tool_result_on_interrupt": {assistant(toolUse("toolu_i", "Bash", obj{"command": "sleep 60"})),
		user(toolResult("toolu_i", "ok\n", false))},
	"success_on_interrupt":     {say("Finished anyway."), success("Finished anyway.", 7, 2)},
	"placeholder_on_interrupt": {success("", 0, 0).set("num_turns", 0), failed(7, 2, interrupted)},
	"exit_on_interrupt":        {},
}
