package main

import (
	"encoding/json"
	"os"
)

// preInitModes print before the init frame, or never print it. A handler
// reports whether it ended the turn.
var preInitModes = map[string]func(f *fake) bool{
	"crash_before_init":         crashBeforeInit,
	"compact_turn":              compactTurn,
	"queued_empty_result":       queuedEmptyResult,
	"queued_empty_result_error": queuedEmptyResult,
	"question":                  question,
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

// queuedEmptyResult prints a task notification, init, and an empty zero-turn
// result. The "_error" mode then ends with an error result. The other mode
// prints a second init, a reply, and a final result.
func queuedEmptyResult(f *fake) bool {
	f.emit(system("task_notification", obj{"session_id": f.sessionID}), system("init", obj{"session_id": f.sessionID}))
	if f.mode == "queued_empty_result_error" {
		f.emit(result("error_during_execution", true, "", 0, 0).set("num_turns", 0))
		return true
	}
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
	if f.mode != "question" {
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
	f.emit(
		system("init", obj{"session_id": f.sessionID}),
		assistant(toolUse("toolu_q", "AskUserQuestion", askQuestionInput)),
		obj{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "tool_deferred", "result": ""},
	)
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
		os.Exit(1)
	}
	f.emit(
		user(toolResult("toolu_q", line, false)),
		system("init", obj{"session_id": f.sessionID}),
		say("Noted."),
		obj{"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "end_turn", "result": "Noted."},
	)
}
