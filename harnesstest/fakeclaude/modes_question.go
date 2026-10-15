package main

import (
	"os"
	"strings"
)

// preInitModes print before the init frame, or never print it. A handler
// reports whether it ended the turn.
var preInitModes = map[string]func(f *fake) bool{
	"crash_before_init":    crashBeforeInit,
	"compact_turn":         compactTurn,
	"compact_after_tokens": compactAfterTokens,
	"compact_after_window": compactAfterTokens,
	"queued_empty_result":  queuedEmptyResult,
	"mirror":               replayMirror,
	"no_init":              noInit,
	"mcp":                  mcpTurn,
}

// noInit prints a hook frame and a reply, but no init frame.
func noInit(f *fake) bool {
	f.emit(system("hook_started", obj{"session_id": f.sessionID}), say("hi"), success("hi", 1, 1))
	return true
}

func crashBeforeInit(*fake) bool {
	os.Exit(1)
	return true
}

// compactTurn prints the stream of a /compact command: status frames, init,
// a compact boundary, two inert user frames, and a zero-turn result. The
// FAKECLAUDE_COMPACT_LOCAL_COMMAND variable sets the result's local_command
// (default "compact", empty omits it). FAKECLAUDE_COMPACT_RESULT sets the
// settling status's compact_result (default "success").
func compactTurn(f *fake) bool {
	localCommand, ok := os.LookupEnv("FAKECLAUDE_COMPACT_LOCAL_COMMAND")
	if !ok {
		localCommand = "compact"
	}
	compactResult := os.Getenv("FAKECLAUDE_COMPACT_RESULT")
	if compactResult == "" {
		compactResult = "success"
	}
	f.emit(
		system("status", obj{"status": "compacting"}),
		system("status", obj{"status": nil, "compact_result": compactResult}),
	)
	if compactResult != "success" {
		return true
	}
	res := success("", 0, 0).set("num_turns", 0)
	if localCommand != "" {
		res["local_command"] = localCommand
	}
	f.emit(
		initFrame(f),
		system("compact_boundary", obj{"compact_metadata": obj{"trigger": "manual", "pre_tokens": 42010, "post_tokens": 7039}, "session_id": f.sessionID}),
		userString("This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation.\n\nSummary:\nfake CLI summary\n\nIf you need specific details from before compaction (like exact code snippets, error messages, or content you generated), read the full transcript at: /fake/transcript.jsonl\nContinue the conversation from where it left off without asking the user any further questions."),
		userString("<local-command-stdout>Compacted </local-command-stdout>"),
		res,
	)
	return true
}

// compactAfterTokens runs a /compact command as compactTurn does, and any
// other turn as the normal turn, with usage.
func compactAfterTokens(f *fake) bool {
	if strings.Contains(f.first, "/compact") {
		return compactTurn(f)
	}
	return false
}

// queuedEmptyResult prints a task notification, init, and an empty zero-turn
// result, a second init, a reply, and a final result.
func queuedEmptyResult(f *fake) bool {
	f.emit(system("task_notification", obj{"session_id": f.sessionID}), initFrame(f))
	f.emit(
		success("", 3, 1).set("num_turns", 0).set("duration_ms", 80),
		initFrame(f),
		say("second"),
		success("second", 9, 4).set("num_turns", 1),
	)
	return true
}

var askQuestionInput = obj{"questions": []obj{{
	"question": "Which database?", "header": "DB", "multiSelect": false,
	"options": []obj{{"label": "PostgreSQL", "description": "a"}, {"label": "SQLite", "description": "b"}},
}}}

// answeredPrefix starts the text that harness sends as the answer to a question.
const answeredPrefix = "User has answered your questions:"

const questionMsg = "msg_011FAKEQUESTION"

// questionStopped is the file that marks the end of the streamed response
// that holds the question call.
func questionStopped() string { return os.Getenv("FAKE_CLAUDE_STATE") + ".stopped" }

// questionInterrupt is what the CLI prints for a SIGINT in "question" modes: a
// SIGINT during the response aborts the run before the CLI accounts the call
// (no cost), and a SIGINT after the response ends accounts it.
func questionInterrupt() []obj {
	if _, err := os.Stat(questionStopped()); err != nil {
		return []obj{user(toolResult("toolu_q", rejected, true)), user(textBlock("[Request interrupted by user]")),
			failed(0, 0, interrupted).set("terminal_reason", "aborted_streaming").set("total_cost_usd", 0).set("modelUsage", obj{})}
	}
	return []obj{user(toolResult("toolu_q", rejected, true)), user(textBlock("[Request interrupted by user for tool use]")),
		failed(10, 277, interrupted).set("terminal_reason", "aborted_tools").set("total_cost_usd", 0.017629).
			set("modelUsage", obj{"claude-haiku-4-5": obj{"costUSD": 0.017629, "inputTokens": 10, "outputTokens": 277}})}
}

// rejected is the result that the CLI prints for the call that a SIGINT cut off.
const rejected = "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). STOP what you are doing and wait for the user to tell you how to proceed."

// question calls ask_user_question on the harness MCP server while its
// response still streams, and waits for the reply, which never comes: harness
// opens the request and interrupts the CLI once the response ends. With
// FAKE_CLAUDE_QUESTION_GATE set, the response ends when the window gate opens.
// A turn that starts with the answer text prints a reply, and so does every
// turn after the first question. The mode question_sibling calls bash beside
// the question.
func question(f *fake) {
	if strings.Contains(f.first, answeredPrefix) {
		f.emit(say("Noted."), success("Noted.", 1, 1))
		return
	}
	asked := os.Getenv("FAKE_CLAUDE_STATE") + ".asked"
	if _, err := os.Stat(asked); err == nil {
		normalTurn(f)
		return
	}
	_ = os.WriteFile(asked, nil, 0o644)
	partialEmit(f, messageStart(questionMsg), blockStart(0, toolUse("toolu_q", "mcp__harness__ask_user_question", obj{})))
	calls := []obj{toolUse("toolu_q", "mcp__harness__ask_user_question", askQuestionInput)}
	if f.mode == "question_sibling" {
		calls = append(calls, toolUse("toolu_s", "mcp__harness__bash", obj{"command": "echo hi"}))
	}
	f.emit(assistant(calls...).inMessage("id", questionMsg))
	if _, ok := hostedServer(); !ok {
		hang(f)
		return
	}
	if f.mode == "question_sibling" {
		sibling := hostedCall("bash", obj{"command": "echo hi"}, "toolu_s")
		f.emit(user(toolResult("toolu_s", sibling.text(), sibling.Result.IsError)))
	}
	done := make(chan hostedText, 1)
	go func() { done <- hostedCall("ask_user_question", askQuestionInput, "toolu_q") }()
	if os.Getenv("FAKE_CLAUDE_QUESTION_GATE") != "" {
		awaitGate()
	}
	_ = os.WriteFile(questionStopped(), nil, 0o644)
	partialEmit(f, blockStop(0), messageStop("tool_use"), streamEvent(obj{"type": "message_stop"}))
	res := <-done
	f.emit(user(toolResult("toolu_q", res.text(), res.Result.IsError)), say("Noted."), success("Noted.", 1, 1))
}

// initFrame is the init frame of a CLI whose only tool is the bash tool of the harness MCP server.
func initFrame(f *fake) obj {
	tools := []string{}
	if strings.HasPrefix(f.mode, "web_search") || f.mode == "tool_search" {
		tools = argTools()
	}
	return system("init", obj{"session_id": f.sessionID, "tools": tools})
}
