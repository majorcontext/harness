// Package claudecode delegates each turn to the Claude Code CLI. One turn
// is one --resume run of the CLI over stream-json. The CLI runs the harness
// tools through MCP and compacts its own context.
package claudecode

import (
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

	"github.com/majorcontext/harness/protocol"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/backend/external"
	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/internal/turn"
)

// oneShotSettings keeps the CLI settings of the user that route the call, such as env
// and apiKeyHelper, and turns off the hooks and every CLAUDE.md file.
const oneShotSettings = `{"disableAllHooks":true,"claudeMdExcludes":["**/*.md"]}`

// stateKey names the backend state of the external session.
const stateKey = "claude-code"

// continuation is the prompt of a run that continues a turn whose input the
// CLI already took: a resume after a handoff.
const continuation = "The previous turn was interrupted. " +
	"Continue the unfinished work from the saved conversation. " +
	"Check the current state before repeating actions that may already have completed."

// historyDirective tells a CLI session that lacks part of the conversation to
// read it through the history tool before it answers.
const historyDirective = "You are continuing a conversation that happened on another model. " +
	"Before responding, call the " + turn.HistoryTool + " tool to read what happened so far."

// toolNames tells the CLI that its tools are the Harness tools. The CLI reads
// instructions written for its own tools.
const toolNames = "Your tools are the Harness tools. Where instructions name Read, Write, Edit, " +
	"MultiEdit, Bash, Glob, Grep, LS, or Task, use read_file, write_file, edit_file, bash, glob, grep, " +
	"ls, or task. Where they name BashOutput, KillShell, or Bash with run_in_background, use process. "

const readSkillDoc = " To use a skill, read its SKILL.md file with read_file."

// toolNamesOf returns the tool-name segment for the tools that the turn offers:
// WebFetch maps to web_fetch only when the turn offers that tool, and WebSearch
// is among the absent tools unless the turn offers the search.
func toolNamesOf(tools []protocol.ToolSpec, search bool) string {
	fetch := slices.ContainsFunc(tools, func(t protocol.ToolSpec) bool { return t.Name == turn.WebFetchTool })
	absent := []string{"TodoWrite"}
	seg := toolNames
	if fetch {
		seg += "Where they name WebFetch, use web_fetch. "
	} else {
		absent = append(absent, "WebFetch")
	}
	if !search {
		absent = append(absent, "WebSearch")
	}
	absent = append(absent, "Skill")
	return seg + "There is no " + orList(absent) + " tool." + readSkillDoc
}

// orList joins names as "a, b, or c", or "a or b" for two.
func orList(names []string) string {
	if len(names) == 2 {
		return names[0] + " or " + names[1]
	}
	return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
}

// grace bounds the wait for the CLI to exit after its result or a signal.
const grace = 5 * time.Second

// ErrToolsNotRestricted reports a CLI whose init frame shows a tool outside the restriction.
var ErrToolsNotRestricted = errors.New("claudecode: the CLI did not apply the tool restriction")

// builtins are the only built-in CLI tools that a run offers. Every tool that
// acts on the machine or the network reaches the CLI as a harness tool, so
// the hooks of the plugins see each call.
var builtins = []string{toolSearchTool}

// toolSearchTool is the CLI tool that loads the schema of a deferred MCP tool.
const toolSearchTool = "ToolSearch"

// mcpPrefix starts the CLI name of each MCP tool.
const mcpPrefix = "mcp__"

// mcpGroup splits the harness name of an MCP tool, mcp__<server>__<tool>, into
// its server. Server names hold no "__".
func mcpGroup(name string) (server string, ok bool) {
	rest, ok := strings.CutPrefix(name, mcpPrefix)
	server, tool, found := strings.Cut(rest, "__")
	return server, ok && found && server != "" && tool != ""
}

// cliName returns the name under which the CLI sees the harness tool name: an
// MCP tool keeps its name, and any other tool is a tool of the harness server.
func cliName(name string) string {
	if _, ok := mcpGroup(name); ok {
		return name
	}
	return mcpPrefix + external.ToolServer + "__" + name
}

// alwaysLoad is the _meta of a tools/list entry that makes the CLI load the
// tool upfront when ToolSearch defers the other MCP tools.
var alwaysLoad = json.RawMessage(`{"anthropic/alwaysLoad":true}`)

// upfront are the harness tools that the CLI loads upfront: a model asks in
// its text when ask_user_question is deferred, and a deferred file or shell
// tool costs nearly every turn a ToolSearch request.
var upfront = []string{turn.AskTool, "bash", "read_file", "edit_file", "write_file", "grep", "glob", "ls"}

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
	// toolLoading is the mcp_tool_loading mode of the config.
	toolLoading string
	routing     *config.AccountRouting
	getenv      func(string) string
}

// New returns the Backend of provider entry p. The CLI keeps only the last
// --append-system-prompt, so the entries of system join into one value. The
// CLI runs in workDir, or in the process directory when workDir is empty.
// toolLoading is mcp_tool_loading, which sets ENABLE_TOOL_SEARCH of the CLI.
func New(p config.Provider, system []string, workDir string, toolLoading string) *Backend {
	return &Backend{p: p, system: strings.Join(system, "\n\n"), workDir: workDir, toolLoading: toolLoading}
}

// Capabilities reports that the CLI runs the loop with the harness tools,
// owns its context, takes steer input, and reports the model window from metadata.
func (b *Backend) Capabilities(model string) turn.Capabilities {
	var window int
	if ref, err := modelref.ParseModelRef(model); err == nil {
		window, _ = modelmeta.ClaudeCodeContextWindow(ref.Model)
	}
	return turn.Capabilities{OwnsLoop: true, OwnsContext: true, Steering: true, Tools: slices.Clone(builtins),
		ContextWindow: window, Efforts: modelmeta.ClaudeCodeEfforts(), WebSearch: true}
}

// Run runs one turn of the CLI on the external session in the saved state.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) error {
	mirror, err := loadMirror(out)
	if err != nil {
		return err
	}
	r := &run{out: out, turnID: req.TurnID, mirror: mirror, allowed: restriction(req),
		names: map[string]string{}, unlisted: map[string]string{}, calls: map[string]bool{}, continues: b.resumes(mirror) && mirror.Turn == req.TurnID,
		askedCh: make(chan string, 1), askMsg: map[string]string{}}
	if b.p.Vendor != "" {
		r.account = req.Settings.Accounts[b.p.Vendor]
	}
	defer r.cleanup()
	cmd, err := b.command(ctx, req, r)
	if err != nil {
		return err
	}
	if r.proc, err = external.Start(cmd, grace); err != nil {
		return fmt.Errorf("claudecode: start %s: %w", cmd.Path, err)
	}
	defer context.AfterFunc(ctx, r.proc.Interrupt)()
	err = r.drive(ctx, req)
	return b.checkLogin(ctx, cmd.Env, r.finish(ctx, err))
}

func loadMirror(out turn.Sink) (external.Mirror, error) {
	snap, err := out.State(stateKey)
	if err != nil {
		return external.Mirror{}, err
	}
	return external.MirrorOf(snap.Head, snap.Entries)
}

// restriction is the set of tools that the CLI may show: ToolSearch, which
// only loads the schema of an MCP tool and touches nothing on the machine,
// WebSearch on a run that offers the search, and each harness tool of req.
// The init frame check keeps a CLI that does not apply it from running.
// req.Tools holds only the allowed harness tools.
func restriction(req turn.Request) map[string]bool {
	allowed := map[string]bool{toolSearchTool: true}
	if req.WebSearch {
		allowed[searchTool] = true
	}
	for _, t := range req.Tools {
		allowed[cliName(t.Name)] = true
	}
	return allowed
}

func (b *Backend) command(ctx context.Context, req turn.Request, r *run) (*exec.Cmd, error) {
	ref, err := modelref.ParseModelRef(req.Model)
	if err != nil {
		return nil, err
	}
	effort, err := modelref.ParseEffort(req.Settings.Effort)
	if err != nil {
		return nil, err
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--thinking-display", "summarized", "--disallowedTools", disallowed}
	tools := slices.DeleteFunc(slices.Sorted(maps.Keys(r.allowed)), func(n string) bool { return strings.HasPrefix(n, mcpPrefix) })
	args = append(args, "--tools", strings.Join(tools, ","), "--strict-mcp-config")
	mcpArgs, err := r.mcpArgs(ctx, req)
	if err != nil {
		return nil, err
	}
	args = append(args, allowSearch(mcpArgs, r.allowed[searchTool])...)
	if ref.Model != "" {
		args = append(args, "--model", ref.Model)
	}
	if b.resumes(r.mirror) {
		args = append(args, "--resume", r.mirror.SessionID)
	}
	if req.OneShot {
		args = append(args, "--no-session-persistence")
	}
	if b.p.PermissionMode != "" {
		args = append(args, "--permission-mode", b.p.PermissionMode)
	}
	if e, ok := effortArg(effort); ok {
		args = append(args, "--effort", e)
	}
	system := b.systemPrompt(req)
	file, err := r.writeTemp("harness-claude-system-*.txt", []byte(system))
	if err != nil {
		return nil, err
	}
	if req.OneShot {
		args = append(args, "--system-prompt-file", file, "--settings", oneShotSettings)
	} else {
		args = append(args, "--append-system-prompt-file", file)
	}
	if req.Sent != nil {
		req.Sent(system)
	}
	env := append(os.Environ(), "ENABLE_TOOL_SEARCH="+toolSearchEnv(b.toolLoading), "ENABLE_PROMPT_CACHING_1H=1")
	if account := req.Settings.Accounts[b.p.Vendor]; b.p.Vendor != "" && account != "" {
		if env, err = b.accountEnv(env, account); err != nil {
			return nil, err
		}
	}
	if b.p.SessionMirror && !req.OneShot {
		dir, err := scratch(r.mirror)
		r.dir = dir
		if err != nil {
			return nil, err
		}
		args = append(args, "--session-mirror")
		env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	}
	cmd := exec.Command(b.p.CLIBinary(), append(args, b.p.ExtraArgs...)...) //nolint:gosec // operator config
	cmd.Env = env
	cmd.Dir = b.workDir
	return cmd, nil
}

// systemPrompt returns the --append-system-prompt value of a run: the tool
// names, the entries of append_system_prompt, the skill segment of the
// session, and the history directive of a session that another model began.
// A one-shot call has the instructions of the call and the entries of append_system_prompt,
// and replaces the system prompt of the CLI.
func (b *Backend) systemPrompt(req turn.Request) string {
	if req.OneShot {
		return strings.Join(slices.DeleteFunc([]string{req.Instructions, b.system}, func(s string) bool { return s == "" }), "\n\n")
	}
	segs := []string{toolNamesOf(req.Tools, req.WebSearch), b.system, req.Skills}
	if req.Foreign {
		segs = append(segs, historyDirective)
	}
	return strings.Join(slices.DeleteFunc(segs, func(s string) bool { return s == "" }), "\n\n")
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

// toolSearchEnv maps mcp_tool_loading to ENABLE_TOOL_SEARCH: the CLI defers
// the schemas of MCP tools through ToolSearch when the value is "true", or
// when it is "auto" and the schemas exceed a share of the context window.
func toolSearchEnv(mode string) string {
	switch mode {
	case "eager":
		return "false"
	case "lazy":
		return "true"
	}
	return "auto"
}

// mcpArgs returns the MCP flags of the run. The CLI gets the harness tools
// of req through the harness-hosted endpoint: one server for each MCP server,
// under its own name, and the harness server for the other tools.
func (r *run) mcpArgs(ctx context.Context, req turn.Request) ([]string, error) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	urls, err := r.serveTools(ctx, req)
	if err != nil {
		return nil, err
	}
	servers := map[string]mcpServer{}
	for _, name := range slices.Sorted(maps.Keys(urls)) {
		server := mcpServer{Type: "http", URL: urls[name]}
		if req.MCPInstructions.SessionIDHeaderServers[name] && req.SessionID != "" {
			server.Headers = map[string]string{"Harness-Session-Id": req.SessionID}
		}
		servers[name] = server
	}
	path, err := r.writeMCPConfig(servers)
	if err != nil {
		return nil, err
	}
	allowed := slices.Sorted(maps.Keys(servers))
	for i, name := range allowed {
		allowed[i] = mcpPrefix + name
	}
	return []string{"--mcp-config", path, "--allowedTools", strings.Join(allowed, ",")}, nil
}

// groupsOf splits tools by the MCP server of each tool. The harness group
// holds the other tools and the common guidance. Each server group carries the
// instructions of its own server, because the CLI cuts the instructions of
// each server by itself, and shows them under the name of that server.
func groupsOf(tools []protocol.ToolSpec, instructions turn.MCPInstructions) []external.Group {
	harness := external.Group{Server: external.ToolServer, Meta: map[string]json.RawMessage{}, Instructions: instructions.Common}
	for _, name := range upfront {
		harness.Meta[name] = alwaysLoad
	}
	byServer := map[string]*external.Group{}
	group := func(server string) *external.Group {
		g := byServer[server]
		if g == nil {
			g = &external.Group{Server: server, Prefix: mcpPrefix + server + "__", Instructions: instructions.Servers[server]}
			byServer[server] = g
		}
		return g
	}
	for _, t := range tools {
		server, ok := mcpGroup(t.Name)
		if !ok {
			harness.Specs = append(harness.Specs, t)
			continue
		}
		g := group(server)
		g.Specs = append(g.Specs, t)
	}
	for server := range instructions.Servers {
		group(server)
	}
	groups := []external.Group{harness}
	for _, server := range slices.Sorted(maps.Keys(byServer)) {
		groups = append(groups, *byServer[server])
	}
	return groups
}

// serveTools serves the harness tools of req for this run and returns the
// URL of each server. Items record a served tool by its harness name, as
// on the other backends.
func (r *run) serveTools(ctx context.Context, req turn.Request) (map[string]string, error) {
	var err error
	call := func(ctx context.Context, c protocol.ToolCall) protocol.ToolResult {
		if c.Name == turn.AskTool {
			return r.ask(ctx, req, c)
		}
		res := req.Call(ctx, c)
		if len(res.Blobs) > 0 {
			r.keepBlobs(c.ID, res.Blobs)
		}
		return res
	}
	if r.tools, err = external.ServeTools(ctx, groupsOf(req.Tools, req.MCPInstructions), toolUseMeta, call); err != nil {
		return nil, err
	}
	r.bridged = map[string]string{}
	for _, t := range req.Tools {
		r.bridged[cliName(t.Name)] = t.Name
	}
	return r.tools.URLs, nil
}

// mcpServer is one server of an --mcp-config file.
type mcpServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// writeMCPConfig writes servers to an --mcp-config file and returns its
// path. A file keeps the headers, the env, and the endpoint URL out of the
// argv of the CLI.
func (r *run) writeMCPConfig(servers map[string]mcpServer) (string, error) {
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return "", err
	}
	return r.writeTemp("harness-claude-mcp-*.json", data)
}

// writeTemp writes data to a new private file, which cleanup removes, and
// returns its path. A file keeps a long value out of the argv, where Linux
// caps one argument at 128 KiB.
func (r *run) writeTemp(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	r.temps = append(r.temps, f.Name())
	_, err = f.Write(data)
	return f.Name(), errors.Join(err, f.Close())
}

func effortArg(e modelref.Effort) (string, bool) {
	switch e {
	case modelref.EffortOff, modelref.EffortMinimal, modelref.EffortLow:
		return "low", true
	case modelref.EffortMedium:
		return "medium", true
	case modelref.EffortHigh:
		return "high", true
	case modelref.EffortXHigh:
		return "xhigh", true
	case modelref.EffortMax:
		return "max", true
	}
	return "", false
}

// prompt is the stdin line that starts the run. A run that continues the
// turn sends continuation: the resumed session already holds the input.
func (r *run) prompt(req turn.Request) (input, error) {
	m := protocol.LogMessage{Parts: []protocol.LogPart{{Type: protocol.LogPartText, Text: continuation}}}
	if text, ok := answered(req); ok {
		m.Parts = []protocol.LogPart{{Type: protocol.LogPartText, Text: text}}
	} else if req.OneShot {
		m.Parts = startParts(req.History)
	} else if !r.continues {
		m.Parts = startParts(req.Input)
	}
	return userLine(m, r.read)
}

// startParts returns the parts of the line that starts a turn. The task lines
// of the reports of children follow in one segment, after the text of the
// other inputs;
// the text of a report is left out, because its task line holds the result.
// When only reports start the turn, the segment follows the trigger sentence
// of the runtime.
func startParts(inputs []protocol.LogMessage) []protocol.LogPart {
	report := func(m protocol.LogMessage) bool {
		return slices.ContainsFunc(m.Parts, func(p protocol.LogPart) bool { return p.Type == protocol.LogPartTaskReport })
	}
	var parts []protocol.LogPart
	var tasks []string
	for _, in := range inputs {
		for _, p := range in.Parts {
			switch {
			case p.Type == protocol.LogPartTaskReport:
				tasks = append(tasks, p.Text)
			case !report(in):
				parts = append(parts, p)
			}
		}
	}
	if len(parts) == 0 && len(tasks) > 0 {
		parts = append(parts, protocol.LogPart{Type: protocol.LogPartText, Text: eventlog.ReportTrigger})
	}
	if len(tasks) > 0 {
		parts = append(parts, protocol.LogPart{Type: protocol.LogPartEngineContext, Text: eventlog.TaskSegment(tasks)})
	}
	return parts
}
