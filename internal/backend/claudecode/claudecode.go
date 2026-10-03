// Package claudecode delegates each turn to the Claude Code CLI. One turn
// is one --resume run of the CLI over stream-json. The CLI runs its own
// tools and compacts its own context.
package claudecode

import (
	"cmp"
	"context"
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
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/message"
)

// stateKey names the state blob of the external session.
const stateKey = "claude-code"

// Continuation is the prompt of a run that continues a turn whose input the
// CLI already took: a resume after a handoff, or a retry.
const Continuation = "The session moved to a new host, which interrupted the previous turn. " +
	"Continue the unfinished work from the saved conversation. " +
	"Check the current state before repeating actions that may already have completed."

// grace bounds the wait for the CLI to exit after its result or a signal.
const grace = 5 * time.Second

// ErrUnknownTool reports a tool restriction that the CLI cannot apply.
var ErrUnknownTool = errors.New("claudecode: unknown tool in restriction")

// ErrToolsNotRestricted reports a CLI whose init frame shows a tool outside the restriction.
var ErrToolsNotRestricted = errors.New("claudecode: the CLI did not apply the tool restriction")

// builtins are the built-in CLI tools that a restriction can name.
var builtins = []string{"Bash", "BashOutput", "Edit", "Glob", "Grep", "KillShell", "MultiEdit",
	"NotebookEdit", "Read", "Skill", "TodoWrite", "WebFetch", "WebSearch", "Write"}

// disallowed are CLI tools that a box cannot serve: subagents bypass the
// harness child tree, and the loop and cron tools need a process that a
// hibernating box does not keep.
const disallowed = "Agent,Workflow,ScheduleWakeup,CronCreate,CronDelete,CronList"

// Backend is a turn.Backend over one claude-code-cli provider entry.
type Backend struct {
	p config.Provider
}

// New returns the Backend of provider entry p.
func New(p config.Provider) *Backend { return &Backend{p: p} }

// Capabilities reports that the CLI runs the loop, owns its context, and
// takes steer input. It reports the context window itself.
func (b *Backend) Capabilities(string) turn.Capabilities {
	return turn.Capabilities{OwnsLoop: true, OwnsContext: true, Steering: true}
}

// Run runs one turn of the CLI on the external session in the state blob.
func (b *Backend) Run(ctx context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	allowed, err := restriction(req.AllowedTools)
	if err != nil {
		return turn.Result{}, err
	}
	blob, err := out.State(stateKey)
	if err != nil {
		return turn.Result{}, err
	}
	mirror, err := external.LoadMirror(blob)
	if err != nil {
		return turn.Result{}, err
	}
	r := &run{out: out, turnID: req.TurnID, mirror: mirror, saved: blob, allowed: allowed, names: map[string]string{},
		continues: b.resumes(mirror) && mirror.Turn == req.TurnID}
	cmd, err := b.command(req, r)
	defer r.cleanup()
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

func restriction(names []string) (map[string]bool, error) {
	if names == nil {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, n := range names {
		if !slices.Contains(builtins, n) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownTool, n)
		}
		allowed[n] = true
	}
	return allowed, nil
}

func (b *Backend) command(req turn.Request, r *run) (*exec.Cmd, error) {
	ref, err := message.ParseModelRef(req.Model)
	if err != nil {
		return nil, err
	}
	effort, err := message.ParseEffort(req.Settings.Effort)
	if err != nil {
		return nil, err
	}
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--thinking-display", "summarized", "--disallowedTools", disallowed}
	if r.allowed != nil {
		args = append(args, "--tools", strings.Join(slices.Sorted(maps.Keys(r.allowed)), ","), "--strict-mcp-config")
	}
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
// turn sends Continuation: the resumed session already holds the input.
func (r *run) prompt(req turn.Request) input {
	m := eventlog.Message{Parts: []eventlog.Part{{Type: eventlog.PartText, Text: Continuation}}}
	if !r.continues {
		m.Parts = nil
		for _, in := range req.Input {
			m.Parts = append(m.Parts, in.Parts...)
		}
	}
	return userLine(m)
}
