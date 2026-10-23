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

var modes = mergeModes(basicModes, threadModes, stdinModes, childModes, partialModes, searchModes)

func normalTurn(f *fake) {
	f.emit(
		say("Let me check that."),
		assistant(toolUse("toolu_1", "mcp__harness__bash", obj{"command": "echo hi"})),
		user(toolResult("toolu_1", "hi\n", false)),
		say("Done — it printed hi."),
		success("Done — it printed hi.", 0, 0).
			set("usage", obj{"input_tokens": 101, "output_tokens": 42, "cache_read_input_tokens": 7, "cache_creation_input_tokens": 5}).
			set("total_cost_usd", 0.0123).set("ttft_ms", 50).set("duration_ms", 400),
	)
}

func parallelCalls(f *fake) {
	bash := func(n, cmd string) obj {
		return assistant(toolUse("toolu_"+n, "mcp__harness__bash", obj{"command": cmd})).inMessage("id", "msg_parallel")
	}
	note := func(s string) obj { return say(s).inMessage("id", "msg_parallel") }
	f.emit(
		note("Running a."),
		bash("a", "echo a"),
		note("Running b."),
		bash("b", "echo b"),
		user(toolResult("toolu_a", "a\n", false)),
		user(toolResult("toolu_b", "b\n", false)),
		say("Both done."),
		success("Both done.", 0, 0),
	)
}

func perCallUsage(f *fake) {
	call := func(id string, block obj, cacheWrite, cacheRead, output int) obj {
		return assistant(block).inMessage("id", id).inMessage("usage", obj{
			"input_tokens": 2, "cache_creation_input_tokens": cacheWrite,
			"cache_read_input_tokens": cacheRead, "output_tokens": output,
		})
	}
	bash := func(n, cmd string) obj { return toolUse("toolu_"+n, "mcp__harness__bash", obj{"command": cmd}) }
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
				"claude-opus-5-5":           obj{"inputTokens": 6, "contextWindow": 500000},
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

// usageLimitRejected is a run that the plan limit refuses: the CLI reports
// the rejected limit, then its API error message, then fails the run with
// the text of that message.
func usageLimitRejected(f *fake) {
	info := obj{
		"status": "rejected", "resetsAt": 1792050000, "rateLimitType": "five_hour",
		"overageStatus": "rejected", "overageDisabledReason": "org_level_disabled", "isUsingOverage": false,
		"unifiedWindows": obj{
			"five_hour": obj{"utilization": 1, "resetsAt": 1792050000},
			"seven_day": obj{"utilization": 0.4, "resetsAt": 1792260000},
		},
	}
	const text = "You've hit your limit · resets 3pm (UTC)"
	f.emit(rateLimitEvent(info), say(text).set("error", "rate_limit").set("is_api_error_message", true),
		result("success", true, text, 0, 0).set("api_error_status", 429))
}

func compactBoundary(f *fake) {
	f.emit(
		system("compact_boundary", obj{"compact_metadata": obj{"trigger": "auto", "pre_tokens": 123456}, "session_id": f.sessionID}),
		say("Continuing after compaction."),
		success("Continuing after compaction.", 12, 6),
	)
}

func compactBoundaryPostTokens(f *fake) {
	f.emit(
		system("compact_boundary", obj{"compact_metadata": obj{"trigger": "auto", "pre_tokens": 123456, "post_tokens": 7039}, "session_id": f.sessionID}),
		say("Continuing after compaction.").inMessage("usage", obj{"input_tokens": 31, "cache_read_input_tokens": 11, "cache_creation_input_tokens": 7}),
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
	"cache_1h": func(f *fake) {
		f.emit(say("ok"), success("ok", 0, 0).set("usage", obj{"input_tokens": 10, "output_tokens": 3, "cache_creation_input_tokens": 50,
			"cache_creation": obj{"ephemeral_1h_input_tokens": 50, "ephemeral_5m_input_tokens": 0}}))
	},
	"one_shot":       func(f *fake) { f.emit(say("MET: ok"), success("MET: ok", 0, 0)) },
	"parallel_calls": parallelCalls,
	"builtin_tool":   builtinTool,
	"builtin_refused": func(f *fake) {
		f.emit(say("Let me check that."), assistant(toolUse("toolu_b", "Bash", obj{"command": "ls"})),
			user(toolResult("toolu_b", "<tool_use_error>Error: No such tool available: Bash. Bash is disabled for this session, in subagents as well as here.</tool_use_error>", true)).set("tool_use_result", "Error: No such tool available: Bash. Bash is disabled for this session, in subagents as well as here."))
		turnCalls(f)
		f.emit(say("Done."), success("Done.", 0, 0))
	},
	"unknown_refused": func(f *fake) {
		f.emit(assistant(toolUse("toolu_u", "bash", obj{"command": "ls"})),
			user(toolResult("toolu_u", "<tool_use_error>Error: No such tool available: bash</tool_use_error>", true)).set("tool_use_result", "Error: No such tool available: bash"))
		turnCalls(f)
		f.emit(say("Done."), success("Done.", 0, 0))
	},
	"builtin_ran_failed": func(f *fake) {
		f.emit(assistant(toolUse("toolu_b", "Bash", obj{"command": "touch x; false"})),
			user(toolResult("toolu_b", "Exit code 1", true)).set("tool_use_result", obj{"stdout": "", "stderr": "", "interrupted": false}),
			success("done", 0, 0))
	},
	"turn_calls": func(f *fake) {
		f.emit(say("Let me check that."))
		turnCalls(f)
		f.emit(say("Done."), success("Done.", 0, 0))
	},
	"turn_calls_error": func(f *fake) {
		turnCalls(f)
		os.Exit(1)
	},
	"turn_calls_hang": func(f *fake) {
		turnCalls(f)
		f.emit(say("Pending model set."))
		hang(f)
	},
	"per_call_usage":               perCallUsage,
	"compact_after_window":         perCallUsage,
	"compact_boundary":             compactBoundary,
	"compact_boundary_post_tokens": compactBoundaryPostTokens,
	"rate_limit_event":             rateLimitTurn(),
	"usage_limit_rejected":         usageLimitRejected,
	"hang_after_text":              hangAfterText,
	"hang_after_listing":           hangAfterListing,
	"hang_in_tool":                 hangInTool,
	"abandon_tool":                 abandonTool,
	"hold_tool":                    abandonTool,
	"drop_tool":                    abandonTool,
	"question":                     question,
	"question_sibling":             question,
	"question_late_sibling":        question,
	"tool_on_interrupt":            hangAfterText,
	"tool_result_on_interrupt":     hangAfterText,
	"success_on_interrupt":         hangAfterText,
	"placeholder_on_interrupt":     hangAfterText,
	"exit_on_interrupt":            hangAfterText,
	"crash":                        crashAfter(),
	"placeholder_before_turn":      frames(success("", 0, 0).set("num_turns", 0), say("Real answer."), success("Real answer.", 7, 2)),
	"fast_no_drain":                frames(say("Done before you finished writing."), success("Done before you finished writing.", 4, 6)),
	"fast_gated":                   fastGated,
	"text_only":                    frames(say("Claude answer."), success("Claude answer.", 7, 2)),
	"error":                        frames(failed(11, 3, "fake failure")),
	"logged_out":                   frames(result("success", true, "Not logged in · Please run /login", 0, 0)),
	"rate_limit_error":             frames(failed(6, 1, "rate_limit_error: please retry later")),
	"credential_error":             frames(failed(6, 1, "API Error: 502 credential resolution failed. This is a server-side issue, usually temporary — try again in a moment.")),
}

// fastGated is fast_no_drain that reads its input and waits for the gate file
// before it answers, so a test orders the end of the turn after its own steps.
func fastGated(f *fake) {
	awaitGate()
	f.emit(say("Done before you finished writing."), success("Done before you finished writing.", 4, 6))
}

func hangAfterText(f *fake) {
	f.emit(say("Working on it."))
	hang(f)
}

// hangInTool sends a text, then a text and a tool call of one API response,
// as the CLI does, and hangs while the tool runs.
func hangInTool(f *fake) {
	f.emit(say("Working on it.").inMessage("id", "msg_0"), say("Checking.").inMessage("id", "msg_A"),
		assistant(toolUse("toolu_h", "mcp__harness__bash", obj{"command": "sleep 60"})).inMessage("id", "msg_A"))
	hang(f)
}

// onInterrupt holds the frames that a mode prints after a SIGINT, before the
// error result. A list that ends with a result replaces the error result,
// and an empty list exits with no frame.
// They reach the driver only after it stopped reading the turn.
var onInterrupt = map[string][]obj{
	"tool_on_interrupt": {assistant(toolUse("toolu_i", "mcp__harness__bash", obj{"command": "sleep 60"}))},
	"tool_result_on_interrupt": {assistant(toolUse("toolu_i", "mcp__harness__bash", obj{"command": "sleep 60"})),
		user(toolResult("toolu_i", "ok\n", false))},
	"success_on_interrupt":     {say("Finished anyway."), success("Finished anyway.", 7, 2)},
	"placeholder_on_interrupt": {success("", 0, 0).set("num_turns", 0), failed(7, 2, interrupted)},
	"exit_on_interrupt":        {},
}

// builtinTool runs a built-in tool that the init frame does not list, which a
// CLI under --tools "" cannot do.
func builtinTool(f *fake) {
	f.emit(
		assistant(toolUse("toolu_b", "Bash", obj{"command": "echo hi"})),
		user(toolResult("toolu_b", "hi\n", false)),
		success("done", 0, 0),
	)
}
