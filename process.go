package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/protocol"
)

func newProcesses(workDir string, specs map[string]config.ProcessSpec) *process.Manager {
	defs := make(map[string]process.Def, len(specs))
	for name, s := range specs {
		defs[name] = process.Def{Command: s.Command, Dir: s.Dir, Env: s.Env, Ports: s.Ports, ReadyRegex: s.ReadyRegex,
			ReadyPort: s.ReadyPort, ReadyHTTP: s.ReadyHTTP, ReadyTimeout: time.Duration(s.ReadyTimeoutS) * time.Second}
	}
	return process.NewManager(workDir, defs)
}

// processTool is the process tool over m. Its description names the
// configured processes only, so it never changes during the runtime.
type processTool struct {
	m    *process.Manager
	spec protocol.ToolSpec
}

const processSchema = `{
	"type": "object",
	"properties": {
		"action": {"type": "string", "enum": ["start", "stop", "restart", "status", "logs", "list", "declare", "undeclare"]},
		"name": {"type": "string", "description": "The process name. Required for every action except list."},
		"tail": {"type": "integer", "description": "logs: the number of last log lines (default 50)."},
		"command": {"type": "array", "items": {"type": "string"}, "description": "declare: the argv."},
		"dir": {"type": "string", "description": "declare: the working directory, relative to the work directory."},
		"env": {"type": "array", "items": {"type": "string"}, "description": "declare: K=V entries."},
		"ports": {"type": "array", "items": {"type": "integer"}, "description": "declare: the TCP ports that the process listens on."},
		"ready_regex": {"type": "string", "description": "declare: a log line that matches marks the process ready."},
		"ready_port": {"type": "integer", "description": "declare: start waits until a TCP dial to this port succeeds."},
		"ready_http": {"type": "string", "description": "declare: start waits until a GET of this URL returns a status below 500."},
		"ready_timeout_s": {"type": "integer", "description": "declare: the seconds that start waits for the ready gate (default 60)."}
	},
	"required": ["action"]
}`

func newProcessTool(m *process.Manager, specs map[string]config.ProcessSpec) processTool {
	var b strings.Builder
	b.WriteString("Manage long-lived processes, such as a dev server: start, stop, restart, status, logs, and list. ")
	b.WriteString("start waits until the process is ready or its ready gate times out. ")
	b.WriteString("declare adds a process until the harness exits. It never changes a config file. A configured process cannot be declared again or undeclared. ")
	b.WriteString("list shows every process with its origin and state.")
	if len(specs) == 0 {
		b.WriteString(" No processes are configured.")
	} else {
		b.WriteString(" Configured processes:")
	}
	for _, name := range slices.Sorted(maps.Keys(specs)) {
		fmt.Fprintf(&b, "\n- %s: %s (dir: %s)", name, strings.Join(specs[name].Command, " "), cmp.Or(specs[name].Dir, "."))
	}
	return processTool{m: m, spec: protocol.ToolSpec{Name: "process", Description: b.String(), InputSchema: json.RawMessage(processSchema)}}
}

func (t processTool) Spec() protocol.ToolSpec { return t.spec }

type processArgs struct {
	Action        string   `json:"action"`
	Name          string   `json:"name"`
	Tail          int      `json:"tail"`
	Command       []string `json:"command"`
	Dir           string   `json:"dir"`
	Env           []string `json:"env"`
	Ports         []int    `json:"ports"`
	ReadyRegex    string   `json:"ready_regex"`
	ReadyPort     int      `json:"ready_port"`
	ReadyHTTP     string   `json:"ready_http"`
	ReadyTimeoutS int      `json:"ready_timeout_s"`
}

func (t processTool) Run(ctx context.Context, call protocol.ToolCall) (protocol.ToolResult, error) {
	var in processArgs
	if err := json.Unmarshal(call.Arguments, &in); err != nil {
		return protocol.ToolResult{}, fmt.Errorf("process: invalid arguments: %w", err)
	}
	if in.Action == "" {
		return protocol.ToolResult{}, fmt.Errorf("process: action is required")
	}
	if in.Action != "list" && in.Name == "" {
		return protocol.ToolResult{}, fmt.Errorf("process: name is required for action %q", in.Action)
	}
	var out any
	var st process.Status
	var err error
	switch in.Action {
	case "start":
		st, err = t.m.Start(ctx, in.Name)
	case "stop":
		st, err = t.m.Stop(ctx, in.Name)
	case "restart":
		st, err = t.m.Restart(ctx, in.Name)
	case "status":
		st, err = t.m.Status(in.Name)
	case "logs":
		var logs string
		logs, st, err = t.m.Logs(in.Name, in.Tail)
		out = processResult{Status: st, Logs: logs}
	case "list":
		out = t.m.List()
	case "declare":
		err = t.m.Declare(in.Name, process.Def{Command: in.Command, Dir: in.Dir, Env: in.Env, Ports: in.Ports, ReadyRegex: in.ReadyRegex,
			ReadyPort: in.ReadyPort, ReadyHTTP: in.ReadyHTTP, ReadyTimeout: time.Duration(in.ReadyTimeoutS) * time.Second})
		out = map[string]any{"ok": true, "name": in.Name, "origin": process.OriginRuntime}
	case "undeclare":
		err = t.m.Undeclare(in.Name)
		out = map[string]any{"ok": true, "name": in.Name}
	default:
		return protocol.ToolResult{}, fmt.Errorf("process: unknown action %q", in.Action)
	}
	if err != nil {
		if !strings.HasPrefix(err.Error(), "process:") {
			err = fmt.Errorf("process: %w", err)
		}
		return protocol.ToolResult{}, err
	}
	if out == nil {
		out = processResult{Status: st}
	}
	b, err := json.Marshal(out)
	return protocol.ToolResult{Text: string(b)}, err
}

type processResult struct {
	process.Status
	Logs string
}

// MarshalJSON reports the exit code only after the process ends, and no
// times: the model reads them from the status line.
func (r processResult) MarshalJSON() ([]byte, error) {
	var code *int
	if r.HasExitCode {
		code = &r.ExitCode
	}
	return json.Marshal(struct {
		Name     string        `json:"name"`
		State    process.State `json:"state,omitempty"`
		PID      int           `json:"pid,omitempty"`
		Ready    bool          `json:"ready"`
		Log      string        `json:"log"`
		ExitCode *int          `json:"exit_code,omitempty"`
		Note     string        `json:"note,omitempty"`
		Logs     string        `json:"logs,omitempty"`
		Ports    []int         `json:"ports,omitempty"`
	}{r.Name, r.State, r.PID, r.Ready, r.Log, code, r.Note, r.Logs, r.Ports})
}

// processStatus is one line for each process that has started, or "". It
// names instants, not durations, so the line changes only when a process
// changes state.
func processStatus(m *process.Manager, workDir string) string {
	var lines []string
	for _, info := range m.List() {
		st := info.Status
		if st.State == "" {
			continue
		}
		state, word, at := string(st.State), "since", st.StartedAt
		if !st.FinishedAt.IsZero() {
			word, at = "at", st.FinishedAt
		}
		if st.State == process.StateExited {
			state += "(" + strconv.Itoa(st.ExitCode) + ")"
		}
		if len(info.Ports) > 0 {
			ports := make([]string, len(info.Ports))
			for i, p := range info.Ports {
				ports[i] = strconv.Itoa(p)
			}
			state += " :" + strings.Join(ports, ",")
		}
		log := st.Log
		if rel, err := filepath.Rel(workDir, log); err == nil && !strings.HasPrefix(rel, "..") {
			log = rel
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s log=%s", info.Name, state, word, at.UTC().Format(time.RFC3339), log))
	}
	if len(lines) == 0 {
		return ""
	}
	return "[processes: " + strings.Join(lines, " | ") + "]"
}
