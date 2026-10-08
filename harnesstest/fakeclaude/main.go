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
//	FAKE_CLAUDE_DISMISS_DIES   makes a dismissed parked question exit at once
//	FAKE_CLAUDE_INIT_TOOLS     JSON tool list of the init frame
//	FAKE_CLAUDE_ENV_LOG        receives the environment as a JSON array
//	FAKE_CLAUDE_CWD_LOG        receives the working directory
//	FAKE_CLAUDE_SIGNAL_LOG     receives the name of a SIGINT before the exit
//	FAKE_CLAUDE_SPAWN_MODES    "n=mode,..." runs mode in spawn n instead of FAKE_CLAUDE_MODE
//	FAKE_CLAUDE_MCP_CONFIG_LOG append the --mcp-config file of each invocation as one line
//	FAKE_CLAUDE_CALL_ARGS      JSON arguments of the FAKE_CLAUDE_CALL_TOOL call
//	FAKE_CLAUDE_LIST_TOOLS     file that receives the tools/list response of the harness MCP server
//
// The mode names and what each proves are in modes.go, modes_thinking.go,
// modes_stdin.go, modes_question.go, modes_mirror.go, modes_mcp.go, and modes_children.go.
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
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type fake struct {
	mode      string
	first     string
	sessionID string
	out       *bufio.Writer
	stdin     *bufio.Reader
	held      []byte
	spawn     int
}

func (f *fake) emit(frames ...obj) {
	buf := f.held
	f.held = nil
	for _, v := range frames {
		b, _ := json.Marshal(v)
		buf = append(append(buf, b...), '\n')
	}
	_, _ = f.out.Write(buf)
	_ = f.out.Flush()
}

func (f *fake) hold(v obj) {
	b, _ := json.Marshal(v)
	f.held = append(append(f.held, b...), '\n')
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

// spawnNumber counts the spawns of this fake in the state directory of the
// session, from 1. Without a state file every spawn is the first.
func spawnNumber() int {
	state := os.Getenv("FAKE_CLAUDE_STATE")
	if state == "" {
		return 1
	}
	path := state + ".spawns"
	b, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	n++
	_ = os.WriteFile(path, []byte(strconv.Itoa(n)), 0o644)
	return n
}

// modeOfSpawn is the mode that spawn n runs: its entry in
// FAKE_CLAUDE_SPAWN_MODES, or FAKE_CLAUDE_MODE.
func modeOfSpawn(n int) string {
	for _, entry := range strings.Split(os.Getenv("FAKE_CLAUDE_SPAWN_MODES"), ",") {
		if k, mode, ok := strings.Cut(entry, "="); ok && k == strconv.Itoa(n) {
			return mode
		}
	}
	return os.Getenv("FAKE_CLAUDE_MODE")
}

// perSpawn is entry n of a comma separated list, counting spawns from 1. An
// entry past the end is the last one when repeat is set, else it is empty.
func perSpawn(list string, n int, repeat bool) string {
	entries := strings.Split(list, ",")
	if n > len(entries) {
		if !repeat {
			return ""
		}
		n = len(entries)
	}
	return entries[n-1]
}

// exitWithParent ends the process once its parent is gone, as the real CLI
// ends when its stdin closes, so a killed harness leaves no CLI behind.
func exitWithParent() {
	ppid := os.Getppid()
	for os.Getppid() == ppid {
		time.Sleep(100 * time.Millisecond)
	}
	os.Exit(1)
}

func listModes() {
	names := map[string]bool{}
	for name := range modes {
		names[name] = true
	}
	for name := range preInitModes {
		names[name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		fmt.Println(name)
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--list-modes" {
		listModes()
		return
	}
	spawn := spawnNumber()
	mode := modeOfSpawn(spawn)
	if mode == "fast_no_drain" {
		_ = os.Stdin.Close()
	}
	go exitWithParent()
	logArgv()
	logMCPConfig()
	logCwd()
	logEnv()
	logInterrupt(mode)

	f := &fake{mode: mode, spawn: spawn, sessionID: os.Getenv("FAKE_CLAUDE_SESSION_ID"), out: bufio.NewWriter(os.Stdout), stdin: bufio.NewReader(os.Stdin)}
	if f.sessionID == "" {
		f.sessionID = "fake-session-1"
	}
	if mode != "fast_no_drain" && !f.questionParked() {
		f.first, _ = f.readLine()
	}
	if h, ok := preInitModes[mode]; ok && h(f) {
		return
	}

	init := system("init", obj{"session_id": f.sessionID})
	if mode == "per_call_usage" || mode == "compact_after_window" {
		init["model"] = "claude-opus-5-5[1m]"
	}
	initExtras(init)
	f.hold(init)
	if h, ok := modes[mode]; ok {
		h(f)
	} else {
		normalTurn(f)
	}
	f.emit()
	callHostedTool(f)
}
