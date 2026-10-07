// Package claudecode delegates each turn to the Claude Code CLI. One turn
// is one --resume run of the CLI over stream-json. The CLI runs its own
// tools and compacts its own context.
package claudecode

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/turn"
)

// stateKey names the backend state of the external session.
const stateKey = "claude-code"

// continuation is the prompt of a run that continues a turn whose input the
// CLI already took: a resume after a handoff.
const continuation = "The previous turn was interrupted. " +
	"Continue the unfinished work from the saved conversation. " +
	"Check the current state before repeating actions that may already have completed."

// reportTrigger is the text of a turn that only reports of children start,
// before the segment that holds their task lines.
const reportTrigger = "A background task you started has finished. " +
	"See the engine context below for its result, and continue accordingly."

// historyDirective tells a CLI session that lacks part of the conversation to
// read it through the history tool before it answers.
const historyDirective = "You are continuing a conversation that happened on another model. " +
	"Before responding, call the " + turn.HistoryTool + " tool to read what happened so far."

// grace bounds the wait for the CLI to exit after its result or a signal.
const grace = 5 * time.Second

// ErrToolsNotRestricted reports a CLI whose init frame shows a tool outside the restriction.
var ErrToolsNotRestricted = errors.New("claudecode: the CLI did not apply the tool restriction")

// builtins are the built-in CLI tools that a restriction can name.
var builtins = []string{"Bash", "BashOutput", "Edit", "Glob", "Grep", "KillShell", "MultiEdit",
	"NotebookEdit", "Read", "Skill", "TodoWrite", "WebFetch", "WebSearch", "Write"}

// mcpPrefix starts the CLI name of each harness tool.
const mcpPrefix = "mcp__" + external.ToolServer + "__"

// toolUseMeta is the _meta field of an MCP tool call that holds the CLI tool use ID.
const toolUseMeta = "claudecode/toolUseId"

// disallowed are CLI tools that a box cannot serve: subagents bypass the
// harness child tree, and the loop and cron tools need a process that a
// hibernating box does not keep.
const disallowed = "Agent,Workflow,ScheduleWakeup,CronCreate,CronDelete,CronList"

// Backend is a turn.Backend over one claude-code-cli provider entry.
type Backend struct {
	p       config.Provider
	system  string
	workDir string
	servers map[string]config.MCPServerSpec
}

// New returns the Backend of provider entry p. The CLI keeps only the last
// --append-system-prompt, so the entries of system join into one value. The
// CLI runs in workDir, or in the process directory when workDir is empty.
// The CLI connects the MCP servers itself.
func New(p config.Provider, system []string, workDir string, servers map[string]config.MCPServerSpec) *Backend {
	return &Backend{p: p, system: strings.Join(system, "\n\n"), workDir: workDir, servers: servers}
}

// Capabilities reports that the CLI runs the loop with its built-in tools,
// owns its context and the MCP servers, and takes steer input. It reports
// the context window itself.
func (b *Backend) Capabilities(string) turn.Capabilities {
	return turn.Capabilities{OwnsLoop: true, OwnsContext: true, OwnsMCP: true, Steering: true, Tools: slices.Clone(builtins)}
}

// Run runs one turn of the CLI on the external session in the saved state. A
// session that waits on a question first gets its resolution: an answer with
// no input is this turn, and anything else is a denial in a run of its own.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	mirror, err := loadMirror(out)
	if err != nil || mirror.Parked == "" {
		return b.once(ctx, req, out, mirror, nil, err)
	}
	res, only, err := resolve(req, out, mirror.Parked)
	if err == nil && !only {
		if _, err = b.once(ctx, req, out, mirror, res, nil); err == nil {
			mirror, err = loadMirror(out)
		}
		res = nil
	}
	return b.once(ctx, req, out, mirror, res, err)
}

func loadMirror(out turn.Sink) (external.Mirror, error) {
	snap, err := out.State(stateKey)
	if err != nil {
		return external.Mirror{}, err
	}
	return external.MirrorOf(snap.Head, snap.Entries)
}

// once runs the CLI once. A failed earlier step is err. With res, the run
// sends no prompt and answers the parked call.
func (b *Backend) once(ctx context.Context, req turn.Request, out turn.Sink, mirror external.Mirror, res *resolution, err error) (turn.Result, error) {
	if err != nil {
		return turn.Result{}, err
	}
	r := &run{out: out, turnID: req.TurnID, mirror: mirror, allowed: restriction(req), names: map[string]string{},
		continues: b.resumes(mirror) && mirror.Turn == req.TurnID, resolution: res, questions: req.Questions || res != nil}
	defer r.cleanup()
	cmd, err := b.command(ctx, req, r)
	if err != nil {
		return turn.Result{}, err
	}
	if r.proc, err = external.Start(cmd, grace); err != nil {
		return turn.Result{}, fmt.Errorf("claudecode: start %s: %w", cmd.Path, err)
	}
	defer context.AfterFunc(ctx, r.proc.Interrupt)()
	err = r.drive(ctx, req)
	return turn.Result{}, r.finish(ctx, err)
}

// restriction is the set of tools that the CLI may show, or nil to allow
// every tool. The init frame check keeps a CLI that does not apply it from
// running. req.Tools holds only the allowed harness tools.
func restriction(req turn.Request) map[string]bool {
	if req.AllowedTools == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, n := range req.AllowedTools {
		if slices.Contains(builtins, n) {
			allowed[n] = true
		}
	}
	for _, t := range req.Tools {
		allowed[mcpPrefix+t.Name] = true
	}
	return allowed
}

func (b *Backend) command(ctx context.Context, req turn.Request, r *run) (*exec.Cmd, error) {
	ref, err := message.ParseModelRef(req.Model)
	if err != nil {
		return nil, err
	}
	effort, err := message.ParseEffort(req.Settings.Effort)
	if err != nil {
		return nil, err
	}
	denied := disallowed
	if r.questions {
		denied += planMode
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--forward-subagent-text", "--thinking-display", "summarized", "--disallowedTools", denied}
	if r.questions {
		pass := ""
		if r.resolution != nil {
			pass = r.resolution.callID
		}
		args = append(args, "--permission-prompt-tool", "stdio", "--settings", questionSettings(pass))
	}
	if r.allowed != nil {
		tools := slices.DeleteFunc(slices.Sorted(maps.Keys(r.allowed)), func(n string) bool { return strings.HasPrefix(n, mcpPrefix) })
		args = append(args, "--tools", strings.Join(tools, ","), "--strict-mcp-config")
	}
	mcpArgs, err := r.mcpArgs(ctx, req, b.servers)
	if err != nil {
		return nil, err
	}
	args = append(args, mcpArgs...)
	if ref.Model != "" {
		args = append(args, "--model", ref.Model)
	}
	if b.resumes(r.mirror) {
		args = append(args, "--resume", r.mirror.SessionID)
	}
	if b.p.PermissionMode != "" {
		args = append(args, "--permission-mode", b.p.PermissionMode)
	}
	if e, ok := effortArg(effort); ok {
		args = append(args, "--effort", e)
	}
	system := b.system
	if req.Foreign {
		system = strings.Trim(system+"\n\n"+historyDirective, "\n")
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	env := os.Environ()
	if b.p.SessionMirror {
		dir, err := scratch(r.mirror)
		r.dir = dir
		if err != nil {
			return nil, err
		}
		args = append(args, "--session-mirror")
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	cmd := exec.Command(cmp.Or(b.p.BinaryPath, "claude"), append(args, b.p.ExtraArgs...)...) //nolint:gosec // operator config
	cmd.Env = env
	cmd.Dir = b.workDir
	return cmd, nil
}

// resumes reports whether a run resumes the external session of m. A
// mirrored session resumes only a saved transcript.
func (b *Backend) resumes(m external.Mirror) bool {
	return m.SessionID != "" && (m.Path != "" || !b.p.SessionMirror)
}

// scratch returns a new config directory that holds the restored transcript.
func scratch(m external.Mirror) (string, error) {
	dir, err := os.MkdirTemp("", "harness-claude-config-")
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return dir, m.Restore(dir)
}

// mcpArgs returns the MCP flags of the run. An unrestricted run gives the
// CLI the configured servers, beside the harness tools of req.
func (r *run) mcpArgs(ctx context.Context, req turn.Request, configured map[string]config.MCPServerSpec) ([]string, error) {
	servers := map[string]mcpServer{}
	if r.allowed == nil {
		for name, spec := range configured {
			servers[name] = serverOf(spec)
		}
	}
	if len(req.Tools) > 0 {
		url, err := r.serveTools(ctx, req)
		if err != nil {
			return nil, err
		}
		servers[external.ToolServer] = mcpServer{Type: "http", URL: url}
	}
	if len(servers) == 0 {
		return nil, nil
	}
	path, err := r.writeMCPConfig(servers)
	if err != nil {
		return nil, err
	}
	args := []string{"--mcp-config", path}
	if r.allowed == nil {
		args = append(args, "--strict-mcp-config")
	}
	if len(req.Tools) > 0 {
		args = append(args, "--allowedTools", "mcp__"+external.ToolServer)
	}
	return args, nil
}

// serveTools serves the harness tools of req for this run and returns the
// URL of the endpoint. Items record a served tool by its harness name, as
// on the other backends.
func (r *run) serveTools(ctx context.Context, req turn.Request) (string, error) {
	var err error
	if r.tools, err = external.ServeTools(ctx, req.Tools, toolUseMeta, req.Call); err != nil {
		return "", err
	}
	r.bridged = map[string]bool{}
	for _, t := range req.Tools {
		r.bridged[t.Name] = true
	}
	return r.tools.URL, nil
}

// mcpServer is one server of an --mcp-config file. A stdio server has no Type.
type mcpServer struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// serverOf returns the --mcp-config entry of spec. It skips an env entry
// with no "=".
func serverOf(spec config.MCPServerSpec) mcpServer {
	if spec.URL != "" {
		return mcpServer{Type: "http", URL: spec.URL, Headers: spec.Headers}
	}
	out := mcpServer{Command: spec.Command[0], Args: spec.Command[1:]}
	for _, kv := range spec.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if out.Env == nil {
				out.Env = map[string]string{}
			}
			out.Env[k] = v
		}
	}
	return out
}

// writeMCPConfig writes servers to an --mcp-config file and returns its
// path. A file keeps the headers, the env, and the endpoint URL out of the
// argv of the CLI.
func (r *run) writeMCPConfig(servers map[string]mcpServer) (string, error) {
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "harness-claude-mcp-*.json")
	if err != nil {
		return "", err
	}
	r.mcpConfig = f.Name()
	_, err = f.Write(data)
	return r.mcpConfig, errors.Join(err, f.Close())
}

func effortArg(e message.Effort) (string, bool) {
	switch e {
	case message.EffortOff, message.EffortMinimal, message.EffortLow:
		return "low", true
	case message.EffortMedium:
		return "medium", true
	case message.EffortHigh:
		return "high", true
	}
	return "", false
}

// prompt is the stdin line that starts the run. A run that continues the
// turn sends continuation: the resumed session already holds the input.
func (r *run) prompt(req turn.Request) (input, error) {
	m := eventlog.Message{Parts: []eventlog.Part{{Type: eventlog.PartText, Text: continuation}}}
	if !r.continues {
		m.Parts = startParts(req.Input)
	}
	return userLine(m, r.read)
}

// startParts returns the parts of the line that starts a turn. The task lines
// of the reports of children follow in one segment, after the text of the
// other inputs, as the engine appended the reports that waited for the turn;
// the text of a report is left out, because its task line holds the result.
// When only reports start the turn, the segment follows the trigger sentence
// of the engine.
func startParts(inputs []eventlog.Message) []eventlog.Part {
	report := func(m eventlog.Message) bool {
		return slices.ContainsFunc(m.Parts, func(p eventlog.Part) bool { return p.Type == eventlog.PartTaskReport })
	}
	var parts []eventlog.Part
	var tasks []string
	for _, in := range inputs {
		for _, p := range in.Parts {
			switch {
			case p.Type == eventlog.PartTaskReport:
				tasks = append(tasks, p.Text)
			case !report(in):
				parts = append(parts, p)
			}
		}
	}
	if len(parts) == 0 && len(tasks) > 0 {
		parts = append(parts, eventlog.Part{Type: eventlog.PartText, Text: reportTrigger})
	}
	if len(tasks) > 0 {
		parts = append(parts, eventlog.Part{Type: eventlog.PartEngineContext, Text: eventlog.TaskSegment(tasks)})
	}
	return parts
}
