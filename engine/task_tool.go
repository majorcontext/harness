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

// The task tool's five actions. "" (the JSON zero value, so an omitted
// action field) is treated as taskActionSpawn — the original, pre-verbs
// shape of this tool's arguments (agent/prompt/model, no action at all)
// keeps working unchanged, the backward-compatibility requirement behind
// this whole extension. The other four — cancel/status/send/log — are the
// model-facing sugar over SessionManager.CancelDescendant/DescendantInfo/
// SendToDescendant/DescendantTranscript (session_manager.go), inspired by
// fx's consolidated subagent tool and following this codebase's own
// action-based-tool precedent (goal_tool.go, mcp_tool.go) rather than five
// separate tools.
const (
	taskActionSpawn  = "spawn"
	taskActionCancel = "cancel"
	taskActionStatus = "status"
	taskActionSend   = "send"
	taskActionLog    = "log"
)

// taskActionNames lists every valid action, for the "unknown action"
// error's own roster — mirrors sortedAgentNames' identical role for the
// spawn action's "unknown agent" error, below.
var taskActionNames = []string{taskActionSpawn, taskActionCancel, taskActionStatus, taskActionSend, taskActionLog}

// taskToolArgs is the task tool's full input shape across all five
// actions. Only the fields a given action actually uses are validated as
// required for it — see runTaskTool's dispatch and each action's own
// runTask* function.
type taskToolArgs struct {
	Action    string `json:"action"`
	Agent     string `json:"agent"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
	// Tail is the log action's entry count: omitted (0) means
	// taskLogDefaultTail, a value over taskLogMaxTail is clamped, and a
	// NEGATIVE value is an error rather than a silent reinterpretation
	// (see runTaskLog).
	Tail int `json:"tail"`
}

// taskToolResult is the spawn action's immediate return: proof the child
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

// taskCancelResult is the cancel action's return: targetID's status
// immediately after SessionManager.CancelDescendant ran. Not always
// "canceled" — cancelOneNodeLocked leaves an ALREADY-terminal node's
// status untouched (see its own doc comment), so canceling an already
// done/failed descendant reports that real terminal status honestly
// rather than claiming a cancellation that did not actually change
// anything.
type taskCancelResult struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

// taskStatusResult is the status action's return: the engine-level
// counterpart to the wire's session.info payload for one descendant —
// status, lineage, and cumulative usage (design doc: "the child's live
// status/lineage/usage").
type taskStatusResult struct {
	SessionID  string   `json:"session_id"`
	ParentID   string   `json:"parent_id"`
	Depth      int      `json:"depth"`
	Status     string   `json:"status"`
	Children   []string `json:"children"`
	AgentType  string   `json:"agent_type"`
	Result     string   `json:"result,omitempty"`
	FailReason string   `json:"fail_reason,omitempty"`
	// FailKind classifies FailReason for a model that must branch rather
	// than parse prose — "provider_exhausted" means the account, not the
	// child, is the problem: preserve the child and resume it with the
	// send action later, never spawn a replacement.
	FailKind string         `json:"fail_kind,omitempty"`
	Usage    provider.Usage `json:"usage"`
}

// taskSendResult is the send action's return. Queued distinguishes the
// two paths SendToDescendant can take: true means text was appended to a
// still-running descendant's own prompt queue — delivered at its next
// mid-turn tool-call boundary if one comes first, or as the start of a
// fresh follow-up turn once its current one ends otherwise (see
// drainQueueAndPrompt, session_manager.go); false means a settled
// descendant was relaunched with text as a fresh turn directly (existing
// Send semantics) — either way the actual outcome arrives later via the
// ordinary completion-notification path, never synchronously from this
// call.
type taskSendResult struct {
	SessionID string `json:"session_id"`
	Queued    bool   `json:"queued"`
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
			Description: "Delegate work to a child session, or manage one you already spawned (directly or transitively). action selects the " +
				"operation and defaults to \"spawn\" if omitted. " +
				"spawn(agent, prompt, model?): starts a child session that runs independently in the background and returns immediately with its " +
				"session id — it does NOT wait for the child to finish, and you do not need to poll for the result. The child's outcome arrives " +
				"later as engine context on one of your own future turns. agent selects the child's tool set and persona: built-in types are " +
				"\"general-purpose\" (full tool set, can itself spawn children), \"explore\" (read-only, for fast code search), and \"plan\" " +
				"(read-only, returns an implementation plan instead of edits) — a project's .agents/*.md files may define more, and this project's " +
				"current full roster (built-ins plus any custom types) is listed in the error if you call this tool with an agent name it does " +
				"not recognize. model optionally overrides which model the child uses. " +
				"cancel(session_id): stops a descendant you spawned and its entire subtree — anything IT has spawned too. " +
				"status(session_id): reports a descendant's current status, lineage, and cumulative token usage. " +
				"send(session_id, prompt): delivers a message to a descendant — if it is still running, the message is queued and delivered at its " +
				"next turn boundary (you do not need to wait for it to go idle first); if it is NOT actively running (finished, or idle and never " +
				"started), it is relaunched with your message as a fresh turn, and that outcome arrives later exactly like a new spawn's would. " +
				"log(session_id, tail?): returns the last tail transcript entries of a descendant — living or dead — so you can read what it was doing and how it " +
				"ended, instead of guessing from its fail_reason. tail defaults to 20 and is capped; entries are filled newest-first under a total size " +
				"budget, and the reply reports how many of the transcript's messages it returned. " +
				"cancel/status/send/log only work on a session YOU spawned, directly or through a chain of your own children — anything else is refused.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"action": {"type": "string", "enum": ["spawn", "cancel", "status", "send", "log"], "description": "The operation to perform; defaults to \"spawn\" if omitted"},
					"agent": {"type": "string", "description": "spawn only: the agent type to spawn: general-purpose, explore, plan, or a custom .agents/*.md definition name — call with an unrecognized name to see this project's full current roster in the error"},
					"prompt": {"type": "string", "description": "The task for the child session to perform (spawn), or the message to deliver to it (send)"},
					"model": {"type": "string", "description": "spawn only: optional model override, as \"provider/model\""},
					"session_id": {"type": "string", "description": "cancel/status/send/log only: the id of a session you spawned, directly or transitively"},
					"tail": {"type": "integer", "description": "log only: how many of the descendant's most recent transcript entries to return (default 20, capped)"}
				}
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			return runTaskTool(s, args)
		},
	}
}

// runTaskTool dispatches one `task` tool call against s by in.Action,
// defaulting an omitted (empty-string) action to taskActionSpawn — the
// original, pre-verbs argument shape (agent/prompt/model, no action
// field at all) keeps working completely unchanged, which is what makes
// this extension backward compatible rather than a breaking schema
// change.
func runTaskTool(s *Session, raw json.RawMessage) (message.Parts, error) {
	var in taskToolArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("task: invalid arguments: %w", err)
	}
	action := in.Action
	if action == "" {
		action = taskActionSpawn
	}
	switch action {
	case taskActionSpawn:
		return runTaskSpawn(s, in)
	case taskActionCancel:
		return runTaskCancel(s, in)
	case taskActionStatus:
		return runTaskStatus(s, in)
	case taskActionSend:
		return runTaskSend(s, in)
	case taskActionLog:
		return runTaskLog(s, in)
	default:
		return nil, fmt.Errorf("task: unknown action %q (want one of: %s)", action, strings.Join(taskActionNames, ", "))
	}
}

// runTaskSpawn resolves in.Agent against every agent definition available
// to s (built-ins plus s.cfg.WorkDir's .agents/*.md — see
// ResolveAgentDefs) and spawns a child via s.cfg.SessionManager. It takes
// no ctx: Spawn's own goroutine, not this call, drives the child's turn
// (see SessionManager.Spawn's doc comment) — this call only needs to
// return once the child is registered and launched, which never blocks on
// I/O worth cancelling.
func runTaskSpawn(s *Session, in taskToolArgs) (message.Parts, error) {
	if in.Agent == "" || in.Prompt == "" {
		return nil, fmt.Errorf("task: agent and prompt are required for action %q", taskActionSpawn)
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

// runTaskCancel implements the cancel action: stop a descendant's entire
// subtree (SessionManager.CancelDescendant — cascade cancellation; see
// its own doc comment for why cancel_tree, not AbortTurn, is the right
// primitive for "stop this delegation").
func runTaskCancel(s *Session, in taskToolArgs) (message.Parts, error) {
	if in.SessionID == "" {
		return nil, fmt.Errorf("task: session_id is required for action %q", taskActionCancel)
	}
	m := s.cfg.SessionManager
	if m == nil {
		return nil, fmt.Errorf("task: this session has no session manager")
	}
	// status is targetID's REAL resulting status, read by CancelDescendant
	// itself inside the same locked operation that performed the
	// cancellation — never assumed StatusCanceled (canceling an
	// already-terminal done/failed descendant is a no-op on its own
	// status) and never re-derived from a separate later read, which
	// could race a caller's own periodic Reap sweep collecting an
	// already-terminal leaf in the gap — see CancelDescendant's own doc
	// comment for the live review finding this closes.
	status, err := m.CancelDescendant(s.ID, in.SessionID)
	if err != nil {
		return nil, classifyTaskVerbError(err, in.SessionID)
	}
	return jsonResult(taskCancelResult{SessionID: in.SessionID, Status: string(status)})
}

// runTaskStatus implements the status action: a descendant's live
// status/lineage/usage (SessionManager.DescendantInfo).
func runTaskStatus(s *Session, in taskToolArgs) (message.Parts, error) {
	if in.SessionID == "" {
		return nil, fmt.Errorf("task: session_id is required for action %q", taskActionStatus)
	}
	m := s.cfg.SessionManager
	if m == nil {
		return nil, fmt.Errorf("task: this session has no session manager")
	}
	node, usage, err := m.DescendantInfo(s.ID, in.SessionID)
	if err != nil {
		return nil, classifyTaskVerbError(err, in.SessionID)
	}
	// Non-nil even for a genuinely childless descendant — matches
	// lineageJSONFor's identical normalization (server/handlers.go) so
	// this serializes as "children":[] rather than "children":null.
	children := node.Children
	if children == nil {
		children = []string{}
	}
	return jsonResult(taskStatusResult{
		SessionID:  node.ID,
		ParentID:   node.ParentID,
		Depth:      node.Depth,
		Status:     string(node.Status),
		Children:   children,
		AgentType:  node.AgentType,
		Result:     node.Result,
		FailReason: node.FailReason,
		FailKind:   node.FailKind,
		Usage:      usage,
	})
}

// runTaskSend implements the send action: deliver a message to a
// descendant (SessionManager.SendToDescendant — queued for a running
// target, a fresh re-run for a settled one; see that method's own doc
// comment for the full reasoning).
func runTaskSend(s *Session, in taskToolArgs) (message.Parts, error) {
	if in.SessionID == "" {
		return nil, fmt.Errorf("task: session_id is required for action %q", taskActionSend)
	}
	// TrimSpace, not a bare == "" test: a whitespace-only prompt used to
	// behave OPPOSITELY by target state — a live review finding. A running
	// target reached SendToDescendant's own enqueue validation and came
	// back with a raw, non-sentinel error classifyTaskVerbError leaks to
	// the model verbatim ("task: engine: EnqueuePrompt requires non-empty
	// text"), while a settled target accepted the blank text and burned a
	// whole real turn on it. One validation here, before either path,
	// makes both answers the same and keeps the message model-facing.
	// The trimmed text is what gets sent, matching EnqueuePrompt's own
	// trim-then-store rule (queue.go).
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("task: prompt is required for action %q", taskActionSend)
	}
	m := s.cfg.SessionManager
	if m == nil {
		return nil, fmt.Errorf("task: this session has no session manager")
	}
	queued, err := m.SendToDescendant(s.ID, in.SessionID, prompt)
	if err != nil {
		return nil, classifyTaskVerbError(err, in.SessionID)
	}
	// "was not actively running," not "had already finished": the
	// non-queued branch also covers a StatusIdle target (adopted but
	// never yet run, or resumed to idle) — SendToDescendant's else branch
	// fires for anything that isn't StatusRunning/StatusCanceled, not
	// only done/failed — and telling the model a session "finished" when
	// it never ran a turn at all could mislead its follow-up reasoning. A
	// live review finding.
	//
	// "dispatched," not a guaranteed "started": SendToDescendant's
	// settled-target path reserves the turn synchronously
	// (reserveSendLocked, under the same m.mu hold as its own admission
	// checks) but runs it in a launched goroutine, so this call returns
	// before the re-run's first provider request. Admission failures are
	// no longer lost — an earlier revision released m.mu and let that
	// goroutine's own Send call discard ErrUnknownSession/
	// ErrSessionCanceled/ErrConcurrencyLimit, which two live review
	// findings closed — so the reservation itself is certain by the time
	// the model reads this note. "Started" would still overclaim the
	// turn's own progress, and task status on session_id remains the way
	// to observe it.
	note := "the descendant was not actively running, so this was dispatched as a fresh turn with your message; check back with task status on this session_id if you want to confirm it actually started"
	if queued {
		// Hedged with "unless it is canceled first," not an unconditional
		// "no need to poll": finalizeTurn's own re-drive re-check (the
		// mechanism that actually guarantees this delivery) is gated on
		// the descendant not being StatusCanceled — canceling it after
		// this call but before its next turn boundary drops the queued
		// entry along with everything else in its subtree (cancellation's
		// ordinary "stop, full stop" semantics — see CancelDescendant's
		// own doc comment), which this note should not paper over. A live
		// review finding.
		//
		// "interrupted ... by a cancel or an abort", not "canceled":
		// a second review finding. A cancel is not the only way a
		// running descendant loses a queued message. An external POST
		// /abort on an ancestor (AbortTurn), or a base-ctx shutdown,
		// cancels the descendant's ctx through Go's context cascade
		// while its status stays StatusRunning until its interrupted
		// Prompt returns; finalizeTurn's re-drive gate and
		// drainQueueAndPrompt both refuse to run the queue on a dead
		// ctx, and the descendant then settles StatusFailed, never
		// StatusCanceled. Naming only cancellation told the model the
		// wrong failure mode for that path.
		note = "queued for delivery at the descendant's next turn boundary — no need to poll or wait for it, unless the descendant's turn is interrupted first by a cancel or an abort (an interrupted descendant leaves anything still queued undelivered, like the rest of its own state)"
	}
	return jsonResult(taskSendResult{SessionID: in.SessionID, Queued: queued, Note: note})
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
// (ErrDepthLimit, ErrConcurrencyLimit, ErrBudgetExceeded,
// ErrSessionCanceled, ErrUnknownSession) is already a short, fixed,
// secret-free string — safe to surface directly, unlike a raw provider
// error — so this only adds the "task:" prefix every other error on this
// surface uses.
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
	case errors.Is(err, ErrBudgetExceeded):
		return fmt.Errorf("task: %w", ErrBudgetExceeded)
	case errors.Is(err, ErrSessionCanceled):
		return fmt.Errorf("task: %w", ErrSessionCanceled)
	case errors.Is(err, ErrUnknownSession):
		return fmt.Errorf("task: parent session no longer tracked")
	default:
		return fmt.Errorf("task: cannot spawn: %w", err)
	}
}

// classifyTaskVerbError maps a cancel/status/send verb's SessionManager
// error into the model-visible tool error — the cancel/status/send
// counterpart to classifyTaskToolError above, for the different
// (overlapping but not identical) sentinel set CancelDescendant/
// DescendantInfo/SendToDescendant can return. Every one of these is
// already a short, fixed, secret-free string — safe to surface directly,
// same reasoning as classifyTaskToolError's own doc comment — including
// targetID itself in the two cases that name it: it is caller-supplied
// input, never provider/request data, and naming it is what actually
// tells the caller which session_id was rejected and why.
//
// ErrUnknownSession is always attributed to targetID here, even though
// CancelDescendant/DescendantInfo/SendToDescendant can technically return
// it for either id. The caller-unknown case is unreachable in practice:
// s (the session running this very tool call) is definitionally
// StatusRunning for the duration of its own Run function, and Reap only
// ever removes a TERMINAL leaf node — s cannot have been forgotten out
// from under its own in-flight call.
//
// The ErrSessionBusy case below is currently unreachable too — a review
// finding: none of the three callers this function serves can produce it
// synchronously. SendToDescendant deliberately ENQUEUES to a running
// target rather than refusing it (see its own doc comment), and
// CancelDescendant/DescendantInfo only ever return ErrUnknownSession/
// ErrNotDescendant. Send CAN return ErrSessionBusy, but only from
// SendToDescendant's OWN fire-and-forget goroutine on the settled-restart
// path, where the error is intentionally discarded, never returned to
// this function's caller at all (see that goroutine's own comment).
// Kept anyway, deliberately, as defensive forward-compatibility: it is
// exactly as safe to surface as every other sentinel here, and dropping
// it would silently start falling through to the generic default case
// the moment any future change to these three methods legitimately
// starts returning it synchronously — a worse failure mode (a vague
// error) than one extra, currently-dead case today.
func classifyTaskVerbError(err error, targetID string) error {
	switch {
	case errors.Is(err, ErrUnknownSession):
		return fmt.Errorf("task: no such session %q", targetID)
	case errors.Is(err, ErrNotDescendant):
		return fmt.Errorf("task: %s is not a session you spawned, directly or transitively", targetID)
	case errors.Is(err, ErrSessionCanceled):
		return fmt.Errorf("task: %w", ErrSessionCanceled)
	case errors.Is(err, ErrConcurrencyLimit):
		return fmt.Errorf("task: %w", ErrConcurrencyLimit)
	case errors.Is(err, ErrSessionBusy):
		return fmt.Errorf("task: %w", ErrSessionBusy)
	case errors.Is(err, ErrEmptyPromptText):
		// runTaskSend rejects blank text before either send path runs, so
		// this arm is defense in depth for a future caller — it keeps the
		// answer model-facing instead of leaking the "engine:" layer
		// through the default arm below (a review finding).
		return fmt.Errorf("task: prompt must not be empty or whitespace-only")
	default:
		return fmt.Errorf("task: %w", err)
	}
}

// The log action's bounds. Every one of them exists because this tool's
// output lands in the PARENT's context, is replayed on every later turn
// of that parent's history, and is requested by a model that cannot see
// how big a child's transcript is before it asks.
const (
	// taskLogDefaultTail is the entry count for an omitted tail — enough
	// to cover a child's last tool loop and its final answer.
	taskLogDefaultTail = 20
	// taskLogMaxTail clamps an explicit tail. A parent that needs more
	// than this is not diagnosing a death any more; the child's own
	// durable log is the right surface for a full read.
	taskLogMaxTail = 100
	// taskLogEntryCap bounds ONE entry's rendered text, in runes. One
	// pasted file or one large tool result must not consume the whole
	// reply.
	taskLogEntryCap = 2000
	// taskLogTotalCap bounds the WHOLE reply's rendered text, in runes.
	// Entries are filled newest-first against it (see renderTaskLog), so
	// the messages nearest the child's death always survive the budget.
	taskLogTotalCap = 20000
	// taskLogTruncationMarker marks a cut entry, so a reader never
	// mistakes a truncated message for a complete one — the same rule
	// truncateTaskResult follows for a child's final text.
	taskLogTruncationMarker = "… [truncated]"
	// taskLogArgsCap bounds a rendered tool call's arguments. A tool call
	// is diagnostic gold (which tool, on what) but its arguments can carry
	// a whole file's contents.
	taskLogArgsCap = 300
)

// taskLogEntry is one rendered transcript message: its role and a flat
// text rendering of every part, since a model reading a tail wants to see
// what happened, not a part-kind union it has to reassemble.
type taskLogEntry struct {
	Role string `json:"role"`
	Text string `json:"text,omitempty"`
	// Truncated marks an entry that lost content to ANY cap — the
	// entry-level taskLogEntryCap, or an inner one (a tool call's
	// arguments at taskLogArgsCap, a tool result's own text). A reader
	// keying on this field rather than scanning for the inline marker
	// must never read a cut entry as complete, which an entry-cap-only
	// flag allowed: 5000 runes of arguments cut to 300 inside an entry
	// whose total stayed under the entry cap reported Truncated: false.
	Truncated bool `json:"truncated,omitempty"`
}

// taskLogResult is the log action's return: the descendant's lifecycle
// facts (so a reader can interpret the tail without a second status call)
// plus the tail itself, oldest first — reading order.
//
// Total is the descendant's WHOLE message count and Returned is how many
// entries came back. They differ whenever the tail bound or the size
// budget bit, and reporting both is what tells a model it is looking at a
// window rather than the whole story.
type taskLogResult struct {
	SessionID  string         `json:"session_id"`
	Status     string         `json:"status"`
	AgentType  string         `json:"agent_type,omitempty"`
	FailReason string         `json:"fail_reason,omitempty"`
	Total      int            `json:"total_messages"`
	Returned   int            `json:"returned"`
	Entries    []taskLogEntry `json:"entries"`
}

// runTaskLog implements the log action: the tail of a descendant's
// transcript, living or dead (SessionManager.DescendantTranscript).
//
// The verb exists because a fail reason is one line and a death is a
// story. A live incident had a parent guess at a dead child's cause and
// act on the guess; the child's own last messages — the tool it was
// running, what it had already found — were sitting in memory the parent
// had no in-process way to read. `task status` reports the lifecycle
// facts; this reports the evidence behind them.
//
// A separate verb rather than a bigger status result: status is a small,
// cheap, poll-shaped answer that several call sites already render, and
// folding an unbounded transcript into it would make every existing
// status call pay for a payload it never asked for.
//
// Content is NOT masked. The parent and the child are the same operator's
// sessions in one process, and a child's final text already reaches the
// parent verbatim through its completion notification (truncateTaskResult)
// — masking here would hide the tool output a parent is reading this to
// see, without closing any boundary that is actually open.
func runTaskLog(s *Session, in taskToolArgs) (message.Parts, error) {
	if in.SessionID == "" {
		return nil, fmt.Errorf("task: session_id is required for action %q", taskActionLog)
	}
	if in.Tail < 0 {
		return nil, fmt.Errorf("task: tail must not be negative for action %q", taskActionLog)
	}
	m := s.cfg.SessionManager
	if m == nil {
		return nil, fmt.Errorf("task: this session has no session manager")
	}
	tail := in.Tail
	if tail == 0 {
		tail = taskLogDefaultTail
	}
	if tail > taskLogMaxTail {
		tail = taskLogMaxTail
	}
	node, msgs, total, err := m.DescendantTranscript(s.ID, in.SessionID, tail)
	if err != nil {
		return nil, classifyTaskVerbError(err, in.SessionID)
	}
	entries := renderTaskLog(msgs)
	return jsonResult(taskLogResult{
		SessionID:  node.ID,
		Status:     string(node.Status),
		AgentType:  node.AgentType,
		FailReason: node.FailReason,
		Total:      total,
		Returned:   len(entries),
		Entries:    entries,
	})
}

// renderTaskLog renders msgs (already tail-trimmed, oldest first) into
// entries under taskLogTotalCap, filling NEWEST-first so the messages
// nearest a child's death always survive the budget, then returning them
// in reading order. An entry is always emitted for the newest message,
// even if it alone exceeds the budget: a reply that reports Returned == 0
// for a non-empty transcript would tell a reader nothing at all.
func renderTaskLog(msgs []message.Message) []taskLogEntry {
	out := make([]taskLogEntry, 0, len(msgs))
	budget := taskLogTotalCap
	for i := len(msgs) - 1; i >= 0; i-- {
		entry := renderTaskLogEntry(msgs[i])
		size := len([]rune(entry.Text))
		if size > budget && len(out) > 0 {
			break
		}
		budget -= size
		out = append(out, entry)
	}
	// Reverse into reading order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// renderTaskLogEntry flattens one message's parts into readable text: text
// verbatim, a tool call as "[tool_call name(args)]", a tool result as
// "[tool_result] <output>", a reasoning summary as "[reasoning] ...", and
// any binary blob as a count. Every non-text part is rendered rather than
// dropped, because a child that died mid-tool-loop has almost nothing BUT
// non-text parts in its last messages.
func renderTaskLogEntry(m message.Message) taskLogEntry {
	var b strings.Builder
	writeLine := func(s string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	// inner records a cut made BELOW the entry level (a tool call's
	// arguments, a tool result's text), so Truncated reports the whole
	// truth rather than only the entry cap's share of it.
	inner := false
	// capInner, not "cap": shadowing the builtin in a function this long
	// is a trap for the next reader.
	capInner := func(text string, n int) string {
		out, cut := capRunes(text, n)
		inner = inner || cut
		return out
	}
	blobs := 0
	for _, p := range m.Parts {
		switch part := p.(type) {
		case *message.Text:
			// An empty Text part writes nothing: a blank line spends
			// budget and tells a reader less than no line at all.
			if part.Text != "" {
				writeLine(part.Text)
			}
		case *message.EngineContext:
			writeLine("[engine_context] " + part.Text)
		case *message.Reasoning:
			if part.Text != "" {
				writeLine("[reasoning] " + part.Text)
			}
		case *message.ToolCall:
			writeLine("[tool_call] " + part.Name + "(" + capInner(string(part.Arguments), taskLogArgsCap) + ")")
		case *message.ToolResult:
			label := "[tool_result]"
			if part.IsError {
				label = "[tool_result error]"
			}
			// Bounded at the PART level, not only by the entry cap below:
			// a mid-loop child can hold a 200KB read_file result, and a
			// tail of up to taskLogMaxTail such messages would otherwise
			// copy tens of MB into this builder just to discard almost
			// all of it. boundedPartsText stops reading at the cap, the
			// same inline-cap shape the tool-call arguments above use.
			text, cut := boundedPartsText(part.SafeContent(), taskLogEntryCap)
			inner = inner || cut
			writeLine(label + " " + text)
			// Parts.Text() renders Text parts only, so an image a tool
			// returned (read_file's [Text, Blob] shape, and MCP's) would
			// otherwise vanish from the tail entirely. Count it with the
			// top-level blobs: a reader diagnosing a death should see
			// that the child received a picture, not silently read a
			// one-line summary as the whole result.
			blobs += countBlobs(part.SafeContent())
		case *message.Blob:
			blobs++
		}
	}
	if blobs > 0 {
		writeLine(fmt.Sprintf("[%d attachment(s)]", blobs))
	}
	text := b.String()
	capped, cut := capRunes(text, taskLogEntryCap)
	return taskLogEntry{Role: string(m.Role), Text: capped, Truncated: cut || inner}
}

// boundedPartsText renders ps's Text parts joined by newlines, exactly as
// message.Parts.Text() does, but stops once n runes are written — so a
// huge tool result is never materialized in full just to be cut
// afterwards. cut reports whether anything was dropped.
func boundedPartsText(ps message.Parts, n int) (text string, cut bool) {
	var b strings.Builder
	written := 0
	for _, p := range ps {
		t, ok := p.(*message.Text)
		if !ok {
			continue
		}
		if written >= n {
			return b.String() + taskLogTruncationMarker, true
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
			written++
		}
		r := []rune(t.Text)
		if len(r) > n-written {
			b.WriteString(string(r[:n-written]))
			return b.String() + taskLogTruncationMarker, true
		}
		b.WriteString(t.Text)
		written += len(r)
	}
	return b.String(), false
}

// countBlobs counts the Blob parts in ps — the parts Parts.Text() drops.
func countBlobs(ps message.Parts) int {
	n := 0
	for _, p := range ps {
		if _, ok := p.(*message.Blob); ok {
			n++
		}
	}
	return n
}

// capRunes cuts s to at most n runes, marking a cut in the text and
// reporting it to the caller (which folds it into taskLogEntry.Truncated).
func capRunes(s string, n int) (text string, cut bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]) + taskLogTruncationMarker, true
}
