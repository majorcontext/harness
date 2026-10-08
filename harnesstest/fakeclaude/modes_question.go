package main

import (
	"encoding/json"
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
	"question":             question,
	"question_continues":   question,
	"question_no_result":   question,
	"question_sibling":     question,
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
		system("init", obj{"session_id": f.sessionID}),
		system("compact_boundary", obj{"compact_metadata": obj{"trigger": "manual", "pre_tokens": 42010, "post_tokens": 7039}, "session_id": f.sessionID}),
		userString("This session is being continued from a previous conversation ..."),
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
	f.emit(system("task_notification", obj{"session_id": f.sessionID}), system("init", obj{"session_id": f.sessionID}))
	f.emit(
		success("", 3, 1).set("num_turns", 0).set("duration_ms", 80),
		system("init", obj{"session_id": f.sessionID}),
		say("second"),
		success("second", 9, 4).set("num_turns", 1),
	)
	return true
}

var askQuestionInput = obj{"questions": []obj{{
	"question": "Which database?", "header": "DB", "multiSelect": false,
	"options": []obj{{"label": "PostgreSQL", "description": "a"}, {"label": "SQLite", "description": "b"}},
}}}

func (f *fake) questionState() string { return os.Getenv("FAKE_CLAUDE_STATE") }

func (f *fake) questionParked() bool {
	if !strings.HasPrefix(f.mode, "question") {
		return false
	}
	_, err := os.Stat(f.questionState())
	return err == nil
}

// question parks an AskUserQuestion call on the first turn, as a defer hook
// does. A later resume answers it over the control channel before it reads
// any prompt text, and later turns print the normal turn.
func question(f *fake) bool {
	state := f.questionState()
	if f.questionParked() {
		resumeParkedQuestion(f, state)
		return true
	}
	if _, err := os.Stat(state + ".asked"); err == nil {
		return false
	}
	_ = os.WriteFile(state+".asked", nil, 0o644)
	_ = os.WriteFile(state, nil, 0o644)
	calls := []obj{toolUse("toolu_q", "AskUserQuestion", askQuestionInput)}
	if f.mode == "question_sibling" {
		calls = append(calls, toolUse("toolu_s", "Bash", obj{"command": "echo hi"}))
	}
	f.emit(system("init", obj{"session_id": f.sessionID}), assistant(calls...))
	questionMirrorFrame(f, "parked")
	f.emit(obj{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "tool_deferred", "result": ""})
	return true
}

func resumeParkedQuestion(f *fake, state string) {
	f.emit(obj{"type": "control_request", "request_id": "req-1", "request": obj{
		"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "toolu_q", "input": askQuestionInput}})
	line, _ := f.readLine()
	_ = os.Remove(state)
	var reply struct {
		Response struct {
			Response struct {
				Message   string `json:"message"`
				Interrupt bool   `json:"interrupt"`
			} `json:"response"`
		} `json:"response"`
	}
	_ = json.Unmarshal([]byte(line), &reply)
	if d := reply.Response.Response; d.Interrupt {
		f.emit(user(toolResult("toolu_q", d.Message, true)))
		if os.Getenv("FAKE_CLAUDE_DISMISS_DIES") != "" {
			os.Exit(1)
		}
		f.emit(
			user(textBlock("[Request interrupted by user]")),
			obj{"type": "result", "subtype": "error_during_execution", "is_error": true, "num_turns": 2, "stop_reason": nil},
		)
		questionMirrorFrame(f, "dismissal-tail")
		os.Exit(1)
	}
	if f.mode == "question_continues" {
		continueInTool(f, line)
		return
	}
	if f.mode != "question_no_result" {
		f.emit(user(toolResult("toolu_q", line, false)))
	}
	f.emit(
		system("init", obj{"session_id": f.sessionID}),
		say("Noted."),
		obj{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "end_turn", "result": "Noted."},
	)
}

// continueInTool continues the answered question with a tool call and waits
// for a queued message while the tool runs. A driver that writes none before
// the test opens the window gate ends the run with the "no second message"
// result.
func continueInTool(f *fake, answer string) {
	f.emit(
		user(toolResult("toolu_q", answer, false)),
		system("init", obj{"session_id": f.sessionID}),
		assistant(textBlock(waitingMarker), toolUse("toolu_c", "Bash", obj{"command": "sleep 1"})),
	)
	text := "no second message received"
	if content, ok := awaitQueued(f); ok {
		text = "received queued: " + content
	}
	f.emit(user(toolResult("toolu_c", "slept", false)), say(text), success(text, 5, 5))
}
