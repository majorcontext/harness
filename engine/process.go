// Managed processes: a `process` session tool for starting, stopping, and
// inspecting long-lived dev/support processes ("pnpm dev"), backed by
// *process.Manager (see package process) — a box-scoped singleton built
// once per harness process and shared across every session via
// Config.Processes, exactly like Config.MCP.
//
// The tool's description is assembled once, at Tool-build time, from the
// config-declared roster only: it stays STABLE for the life of the
// process/session regardless of any runtime `declare`/`undeclare` calls
// that happen afterward (see processToolDescription) — the whole point is
// that this text is safe to sit in a cached system-prompt/tool-list
// prefix. The live roster (including runtime declarations) is visible
// through the tool's own `list` action and the ambient status block (see
// processStatusSegment) instead.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/process"
	"github.com/majorcontext/harness/provider"
)

// ProcessRegistry is the slice of process management a Session needs: the
// `process` tool's actions, and the live-roster query the ambient status
// injection reads every turn. *process.Manager satisfies it directly;
// tests use fakes. A nil ProcessRegistry disables the process tool and
// ambient injection entirely — see Config.Processes.
type ProcessRegistry interface {
	Start(ctx context.Context, name string) (process.Status, error)
	Stop(ctx context.Context, name string) (process.Status, error)
	Restart(ctx context.Context, name string) (process.Status, error)
	Status(name string) (process.Status, error)
	Logs(name string, tail int) (string, process.Status, error)
	List() []process.Info
	Declare(name string, def process.Def) error
	Undeclare(name string) error
	EverStarted() bool
}

// processToolName is the session tool's fixed name.
const processToolName = "process"

// defaultLogTail is the logs action's default tail line count when the
// caller omits it.
const defaultLogTail = 50

// processTool builds the `process` session tool over reg. Its Description
// is computed once, here, from reg.List()'s CONFIG-origin entries only
// (see the package doc) — never recomputed per call.
func processTool(reg ProcessRegistry) Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        processToolName,
			Description: processToolDescription(reg),
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["start", "stop", "restart", "status", "logs", "list", "declare", "undeclare"], "description": "The operation to perform"},
					"name": {"type": "string", "description": "The process name (required for every action except list)"},
					"tail": {"type": "integer", "description": "logs action: number of trailing log lines to return (default 50)"},
					"command": {"type": "array", "items": {"type": "string"}, "description": "declare action: the process argv (required, non-empty)"},
					"dir": {"type": "string", "description": "declare action: working directory, resolved against the session working directory"},
					"env": {"type": "array", "items": {"type": "string"}, "description": "declare action: K=V environment entries"},
					"ports": {"type": "array", "items": {"type": "integer"}, "description": "declare action: TCP ports this process listens on (1-65535) — declarative metadata only, never enforced or allocated"},
					"ready_regex": {"type": "string", "description": "declare action: a regex; a matching combined stdout+stderr log line marks the process ready"},
					"ready_port": {"type": "integer", "description": "declare action: a TCP port (1-65535); start blocks until a plain TCP dial to it succeeds. At most one of ready_regex/ready_port/ready_http may be set"},
					"ready_http": {"type": "string", "description": "declare action: a URL; start blocks until a GET to it returns any non-5xx status. At most one of ready_regex/ready_port/ready_http may be set"},
					"ready_timeout_s": {"type": "integer", "description": "declare action: seconds start blocks waiting for the ready gate before giving up (default 60)"}
				},
				"required": ["action"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runProcessTool(ctx, reg, args)
		},
	}
}

// processToolDescription lists the config-declared roster (name, command,
// dir) and explains runtime declaration — stable, cache-safe text (see the
// package doc). An empty config roster still explains the tool's actions
// (the tool is exposed unconditionally in serve mode — see cmd/harness —
// so a fresh box with no processes configured yet must still get a usable
// description).
func processToolDescription(reg ProcessRegistry) string {
	var b strings.Builder
	b.WriteString("Manage long-lived dev/support processes (e.g. a `pnpm dev` server): start, stop, restart, check status, and read logs. ")
	b.WriteString("Actions: start(name), stop(name), restart(name), status(name), logs(name, tail=50), list(), declare(name, command, dir?, env?, ports?, ready_regex?, ready_port?, ready_http?, ready_timeout_s?), undeclare(name). ")
	b.WriteString("start blocks until the process is ready (or the ready gate times out) so one call gives a definitive answer. ")
	b.WriteString("declare registers a NEW process definition for THIS SERVER PROCESS ONLY — it is never written to any config file; edit .harness.json directly for a definition that should persist across restarts. ")
	b.WriteString("Redeclaring a name defined in config, or undeclaring one, is rejected. ")
	b.WriteString("Use list to see the full live roster (including anything declared at runtime) with its origin (config or runtime) and current status.")

	var configured []process.Info
	for _, info := range reg.List() {
		if info.Origin == process.OriginConfig {
			configured = append(configured, info)
		}
	}
	sort.Slice(configured, func(i, j int) bool { return configured[i].Name < configured[j].Name })
	if len(configured) == 0 {
		b.WriteString(" No processes are configured yet.")
		return b.String()
	}
	b.WriteString(" Configured processes:")
	for _, info := range configured {
		dir := info.Dir
		if dir == "" {
			dir = "."
		}
		fmt.Fprintf(&b, "\n- %s: %s (dir: %s)", info.Name, strings.Join(info.Command, " "), dir)
	}
	return b.String()
}

// processToolArgs is the process tool's full input shape; only the fields
// each action cares about are read.
type processToolArgs struct {
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

func runProcessTool(ctx context.Context, reg ProcessRegistry, raw json.RawMessage) (message.Parts, error) {
	var in processToolArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("process: invalid arguments: %w", err)
	}
	if in.Action == "" {
		return nil, fmt.Errorf("process: action is required")
	}
	if in.Action != "list" && in.Name == "" {
		return nil, fmt.Errorf("process: name is required for action %q", in.Action)
	}

	switch in.Action {
	case "start":
		st, err := reg.Start(ctx, in.Name)
		if err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(statusResult(st))
	case "stop":
		st, err := reg.Stop(ctx, in.Name)
		if err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(statusResult(st))
	case "restart":
		st, err := reg.Restart(ctx, in.Name)
		if err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(statusResult(st))
	case "status":
		st, err := reg.Status(in.Name)
		if err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(statusResult(st))
	case "logs":
		tail := in.Tail
		if tail <= 0 {
			tail = defaultLogTail
		}
		content, st, err := reg.Logs(in.Name, tail)
		if err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		res := statusResult(st)
		res.Logs = content
		return jsonResult(res)
	case "list":
		return jsonResult(reg.List())
	case "declare":
		def := process.Def{
			Command:      in.Command,
			Dir:          in.Dir,
			Env:          in.Env,
			Ports:        in.Ports,
			ReadyRegex:   in.ReadyRegex,
			ReadyPort:    in.ReadyPort,
			ReadyHTTP:    in.ReadyHTTP,
			ReadyTimeout: time.Duration(in.ReadyTimeoutS) * time.Second,
		}
		if err := reg.Declare(in.Name, def); err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(map[string]any{"ok": true, "name": in.Name, "origin": process.OriginRuntime})
	case "undeclare":
		if err := reg.Undeclare(in.Name); err != nil {
			return nil, fmt.Errorf("process: %w", err)
		}
		return jsonResult(map[string]any{"ok": true, "name": in.Name})
	default:
		return nil, fmt.Errorf("process: unknown action %q", in.Action)
	}
}

// processResult is the JSON shape returned by start/stop/restart/status/logs.
type processResult struct {
	Name     string `json:"name"`
	State    string `json:"state,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Ready    bool   `json:"ready"`
	Log      string `json:"log"`
	Elapsed  string `json:"elapsed,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Note     string `json:"note,omitempty"`
	Logs     string `json:"logs,omitempty"`
	Ports    []int  `json:"ports,omitempty"`
}

func statusResult(st process.Status) processResult {
	res := processResult{
		Name:  st.Name,
		State: string(st.State),
		PID:   st.PID,
		Ready: st.Ready,
		Log:   st.Log,
		Note:  st.Note,
		Ports: st.Ports,
	}
	if st.HasExitCode {
		code := st.ExitCode
		res.ExitCode = &code
	}
	if d := elapsedFor(st); d > 0 {
		res.Elapsed = roughDuration(d)
	}
	return res
}

// elapsedFor is the wall-clock span a status result reports: since start
// for an active process, since finish for one that has exited/stopped,
// zero for a never-started one.
func elapsedFor(st process.Status) time.Duration {
	switch st.State {
	case process.StateExited, process.StateStopped:
		if st.FinishedAt.IsZero() {
			return 0
		}
		return time.Since(st.FinishedAt)
	case "":
		return 0
	default:
		if st.StartedAt.IsZero() {
			return 0
		}
		return time.Since(st.StartedAt)
	}
}

// roughDuration renders d coarsely (seconds, minutes, or hours) — a
// human-scale approximation, not a precise duration, since it is meant for
// a status line a model reads, not a log.
func roughDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func jsonResult(v any) (message.Parts, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("process: marshaling result: %w", err)
	}
	return message.Parts{&message.Text{Text: string(b)}}, nil
}

// processStatusSegment renders the ambient status block request assembly
// appends to the newest user message (see streamTurn): one token per
// declared process that has EVER been started (never-started entries are
// omitted, and the whole block is empty — never appended — until at least
// one has), each naming its state, a coarse elapsed time, and its log
// path relativized against workDir when possible.
//
// This is computed fresh on every call (cheap: an in-memory map read plus
// string formatting) so it always reflects LIVE state; nothing here is
// ever persisted (see streamTurn/withProcessStatus for the durability
// boundary).
func processStatusSegment(reg ProcessRegistry, workDir string) string {
	if reg == nil || !reg.EverStarted() {
		return ""
	}
	var tokens []string
	for _, info := range reg.List() {
		if info.Status.State == "" {
			continue
		}
		tokens = append(tokens, formatProcessStatus(info, workDir))
	}
	if len(tokens) == 0 {
		return ""
	}
	return "[processes: " + strings.Join(tokens, " | ") + "]"
}

func formatProcessStatus(info process.Info, workDir string) string {
	st := info.Status
	logPath := st.Log
	if workDir != "" && logPath != "" {
		if rel, err := filepath.Rel(workDir, logPath); err == nil && !strings.HasPrefix(rel, "..") {
			logPath = rel
		}
	}
	ports := formatPorts(info.Ports)
	switch st.State {
	case process.StateExited:
		return fmt.Sprintf("%s exited(%d)%s %s ago log=%s", info.Name, st.ExitCode, ports, roughDuration(time.Since(st.FinishedAt)), logPath)
	case process.StateStopped:
		return fmt.Sprintf("%s stopped%s %s ago log=%s", info.Name, ports, roughDuration(time.Since(st.FinishedAt)), logPath)
	default:
		return fmt.Sprintf("%s %s%s %s log=%s", info.Name, st.State, ports, roughDuration(time.Since(st.StartedAt)), logPath)
	}
}

// formatPorts renders a declared process's Ports as a leading-space
// ":port[,port...]" token (e.g. " :3000,3001"), or "" when no ports are
// declared — pure declarative metadata carried into the ambient status
// block, never a liveness signal (see Def.Ports).
func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	strs := make([]string, len(ports))
	for i, p := range ports {
		strs[i] = strconv.Itoa(p)
	}
	return " :" + strings.Join(strs, ",")
}

// withProcessStatus returns messages with seg appended as a new Text part
// on a CLONE of the newest RoleUser message — never mutating the shared
// backing Parts slice of the original (which may alias the session's own
// durable history), and never touching any message but the last user one,
// so the cached prefix (every earlier message) is untouched. A no-op if
// messages holds no user message at all.
func withProcessStatus(messages []message.Message, seg string) []message.Message {
	if seg == "" {
		return messages
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != message.RoleUser {
			continue
		}
		clone := messages[i]
		parts := make(message.Parts, len(clone.Parts), len(clone.Parts)+1)
		copy(parts, clone.Parts)
		parts = append(parts, &message.Text{Text: seg})
		clone.Parts = parts
		messages[i] = clone
		return messages
	}
	return messages
}
