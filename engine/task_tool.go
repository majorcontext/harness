package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// taskToolName is the built-in `task` tool's registered name. Installed by
// newSession whenever Config.SessionManager is set (see that field's doc
// comment), withheld or restricted afterward by SessionManager itself —
// see installTaskToolLocked and Spawn in session_manager.go.
const taskToolName = "task"

// taskToolResult is the `task` tool's immediate return: proof the child
// exists and its first turn has been launched — never the child's actual
// result. That arrives later, delivered to the PARENT as an EngineContext
// notification (taskdelivery.go) once the child reaches done or failed —
// the design doc's "queue-or-resume delivery" and its "non-blocking
// execution" locked decision.
type taskToolResult struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Note      string `json:"note"`
}

// taskTool's Description/InputSchema are STATIC strings, deliberately —
// a follow-up finding ("roster in tool description") considered and
// rejected making them dynamic per-session. taskTool() is constructed
// once per session at newSession/Spawn time (see taskToolName's own doc
// comment), strictly BEFORE Session.AgentDefs() is ever resolved — that
// resolution is lazy, first triggered from inside runTaskTool itself
// (see AgentDefs' own doc comment on the "startup budget" this
// preserves: a malformed .agents/*.md should only ever break SPAWNING a
// child, never every session's construction). Building a real per-session
// roster into this description would force eager .agents/*.md disk I/O
// and parsing for EVERY session, whether or not it ever calls `task` at
// all — a real cost this fix does not accept paying just to make the
// description marginally more helpful.
//
// The chosen, feasible compromise: the description and the agent
// property's own schema description both explicitly point the model at
// the mechanism that already IS fully live and per-session-accurate —
// sortedAgentNames, surfaced in runTaskTool's "unknown agent" error
// (below) — rather than trying to duplicate that roster here, in a
// static string that could only ever describe the three built-ins.
func taskTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name: taskToolName,
			Description: "Delegate work to a child session that runs independently in the background. Returns immediately with the child's " +
				"session id — it does NOT wait for the child to finish, and you do not need to poll for the result. The child's outcome arrives " +
				"later as engine context on one of your own future turns. agent selects the child's tool set and persona: built-in types are " +
				"\"general-purpose\" (full tool set, can itself spawn children), \"explore\" (read-only, for fast code search), and \"plan\" " +
				"(read-only, returns an implementation plan instead of edits) — a project's .agents/*.md files may define more, and this project's " +
				"current full roster (built-ins plus any custom types) is listed in the error if you call this tool with an agent name it does " +
				"not recognize. model optionally overrides which model the child uses.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"agent": {"type": "string", "description": "The agent type to spawn: general-purpose, explore, plan, or a custom .agents/*.md definition name — call with an unrecognized name to see this project's full current roster in the error"},
					"prompt": {"type": "string", "description": "The task for the child session to perform"},
					"model": {"type": "string", "description": "Optional model override, as \"provider/model\""}
				},
				"required": ["agent", "prompt"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runTaskTool(s, args)
		},
	}
}

// runTaskTool resolves in.Agent against every agent definition available
// to s (built-ins plus s.cfg.WorkDir's .agents/*.md — see
// ResolveAgentDefs) and spawns a child via s.cfg.SessionManager. It takes
// no ctx: Spawn's own goroutine, not this call, drives the child's turn
// (see SessionManager.Spawn's doc comment) — this call only needs to
// return once the child is registered and launched, which never blocks on
// I/O worth cancelling.
func runTaskTool(s *Session, raw json.RawMessage) (message.Parts, error) {
	var in struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("task: invalid arguments: %w", err)
	}
	if in.Agent == "" || in.Prompt == "" {
		return nil, fmt.Errorf("task: agent and prompt are required")
	}
	m := s.cfg.SessionManager
	if m == nil {
		return nil, fmt.Errorf("task: this session has no session manager")
	}

	defs, err := s.AgentDefs()
	if err != nil {
		return nil, fmt.Errorf("task: loading agent definitions: %w", err)
	}
	def, ok := defs[in.Agent]
	if !ok {
		return nil, fmt.Errorf("task: unknown agent %q (available: %s)", in.Agent, strings.Join(sortedAgentNames(defs), ", "))
	}

	model := def.Model
	if in.Model != "" {
		ref, err := message.ParseModelRef(in.Model)
		if err != nil {
			return nil, fmt.Errorf("task: invalid model %q: %w", in.Model, err)
		}
		model = ref
	}
	// Validate the provider is configured BEFORE Spawn, mirroring the
	// `model` tool's own identical check (runModelTool) — a live review
	// finding: ParseModelRef only checks the ref is well-formed, not that
	// its provider is registered, so an unconfigured model used to sail
	// through Spawn, consume a concurrency slot and a session log, and
	// only fail later at the child's first turn — surfacing to the parent
	// as a delayed "[tasks: ... failed: ...]" notification instead of the
	// immediate, synchronous tool error a caller-side mistake like this
	// deserves. Covers BOTH sources of model, not just in.Model: an
	// earlier revision of this fix validated only the caller's override,
	// missing that def.Model — an agent DEFINITION naming an unconfigured
	// provider — sails through exactly the same way, a live review
	// finding on the first pass at this fix. model.IsZero() (def.Model
	// unset AND no override) is deliberately exempt: Spawn treats a zero
	// Model as "inherit the parent's own, already-configured model" (see
	// its own `if !opts.Model.IsZero()` guard) — never itself a candidate
	// for an unconfigured provider.
	if !model.IsZero() && !s.ModelSupported(model) {
		return nil, fmt.Errorf("task: provider %q is not configured (%s)", model.Provider, s.modelChoicesHint())
	}

	childID, err := m.Spawn(SpawnOptions{
		ParentID:     s.ID,
		Prompt:       in.Prompt,
		Model:        model,
		SystemAppend: def.SystemAppend,
		ToolNames:    def.Tools,
		AgentType:    in.Agent,
	})
	if err != nil {
		return nil, classifyTaskToolError(err)
	}
	return jsonResult(taskToolResult{
		SessionID: childID,
		Agent:     in.Agent,
		Note:      "spawned and running in the background; its result will arrive later as engine context — no need to poll or wait for it",
	})
}

// sortedAgentNames returns defs' keys sorted, for a stable, readable
// "unknown agent" error message.
func sortedAgentNames(defs map[string]AgentDef) []string {
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// classifyTaskToolError maps a SessionManager.Spawn error into the
// model-visible tool error. Every SessionManager sentinel
// (ErrDepthLimit, ErrConcurrencyLimit, ErrSessionCanceled,
// ErrUnknownSession) is already a short, fixed, secret-free string — safe
// to surface directly, unlike a raw provider error — so this only adds
// the "task:" prefix every other error on this surface uses.
//
// The default case is reached only by restrictTools' "unknown tool %q"
// error (Spawn's doc comment enumerates every error it can return, and
// this is the one sentinel-less shape): also safe to surface directly — it
// carries nothing but a tool name from the agent definition's own tools:
// list, never provider/request data — and doing so is what actually
// diagnoses the real cause. A definition can legitimately name a real,
// known tool (agentdef.go's knownToolNames, checked at LOAD time) that
// simply isn't REGISTERED on this particular session (e.g. `mcp` on a box
// with no MCP servers configured) — restrictTools can only discover that
// mismatch at spawn time, and flattening its message to a generic
// "could not spawn" (an earlier version of this function did) left no way
// to tell that case apart from a depth/concurrency limit.
func classifyTaskToolError(err error) error {
	switch {
	case errors.Is(err, ErrDepthLimit):
		return fmt.Errorf("task: %w", ErrDepthLimit)
	case errors.Is(err, ErrConcurrencyLimit):
		return fmt.Errorf("task: %w", ErrConcurrencyLimit)
	case errors.Is(err, ErrSessionCanceled):
		return fmt.Errorf("task: %w", ErrSessionCanceled)
	case errors.Is(err, ErrUnknownSession):
		return fmt.Errorf("task: parent session no longer tracked")
	default:
		return fmt.Errorf("task: cannot spawn: %w", err)
	}
}
