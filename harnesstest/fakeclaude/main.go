// Command fakeclaude is a stand-in for the Claude Code CLI. It reads the
// stream-json input lines that harness writes and prints a canned
// stream-json reply chosen by the FAKE_CLAUDE_MODE environment variable. An
// unset or unknown mode prints a turn with text, a tool call, a tool result,
// usage, and cost.
//
// Other environment variables:
//
//	FAKE_CLAUDE_LOG        append the argv of each invocation as one JSON line
//	FAKE_CLAUDE_STDIN_LOG  append every input line read
//	FAKE_CLAUDE_SESSION_ID session id in the init frame (default fake-session-1)
//	FAKE_CLAUDE_STATE      file path that keeps the "question" mode parked state
//	FAKE_CLAUDE_LEAK_PID_FILE  receives the pid of a leaked grandchild
//	FAKE_CLAUDE_DISMISS_DIES   makes a dismissed parked question exit at once
//	FAKE_CLAUDE_INIT_TOOLS     JSON tool list of the init frame
//	FAKE_CLAUDE_ENV_LOG        receives the environment as a JSON array
//	FAKE_CLAUDE_CWD_LOG        receives the working directory
//	FAKE_CLAUDE_SIGNAL_LOG     receives the name of a SIGINT before the exit
//	FAKE_CLAUDE_MCP_CONFIG_LOG append the --mcp-config file of each invocation as one line
//
// The mode names and what each proves are in modes.go, modes_thinking.go,
// modes_stdin.go, modes_question.go, modes_mirror.go, and modes_mcp.go.
//
// The normal turn is not byte-faithful to the real CLI in two ways. Its
// result frame has no num_turns or session_id, so the driver takes its
// num_turns-absent branch. Every spawn reuses the tool id toolu_1, so a
// session that spans spawns holds duplicate tool call ids.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type fake struct {
	mode      string
	sessionID string
	out       *bufio.Writer
	stdin     *bufio.Reader
}

func (f *fake) emit(frames ...obj) {
	for _, v := range frames {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintln(f.out, string(b))
		_ = f.out.Flush()
	}
}

func (f *fake) readLine() (string, bool) {
	b, err := f.stdin.ReadString('\n')
	if path := os.Getenv("FAKE_CLAUDE_STDIN_LOG"); path != "" && b != "" {
		appendFile(path, b)
	}
	return strings.TrimRight(b, "\n"), err == nil
}

func appendFile(path, s string) {
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fh.WriteString(s)
	_ = fh.Close()
}

func logArgv() {
	path := os.Getenv("FAKE_CLAUDE_LOG")
	if path == "" {
		return
	}
	b, _ := json.Marshal(os.Args[1:])
	appendFile(path, string(b)+"\n")
}

func logCwd() {
	if path := os.Getenv("FAKE_CLAUDE_CWD_LOG"); path != "" {
		wd, _ := os.Getwd()
		appendFile(path, wd+"\n")
	}
}

func main() {
	mode := os.Getenv("FAKE_CLAUDE_MODE")
	if mode == "bg_leak_child" {
		time.Sleep(time.Hour)
		return
	}
	if mode == "fast_no_drain" {
		_ = os.Stdin.Close()
	}
	logArgv()
	logMCPConfig()
	logCwd()
	logEnv()
	logInterrupt()

	f := &fake{mode: mode, sessionID: os.Getenv("FAKE_CLAUDE_SESSION_ID"), out: bufio.NewWriter(os.Stdout), stdin: bufio.NewReader(os.Stdin)}
	if f.sessionID == "" {
		f.sessionID = "fake-session-1"
	}
	if mode != "fast_no_drain" && !f.questionParked() {
		f.readLine()
	}
	if h, ok := preInitModes[mode]; ok && h(f) {
		return
	}

	init := system("init", obj{"session_id": f.sessionID})
	if mode == "per_call_usage" {
		init["model"] = "claude-opus-5-5[1m]"
	}
	initExtras(init)
	f.emit(init)
	if h, ok := modes[mode]; ok {
		h(f)
	} else {
		normalTurn(f)
	}
	callHostedTool(f)
}
