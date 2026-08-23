package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// SessionStatus is a session's lifecycle state as tracked by a
// SessionManager. Every managed session (root or child) starts idle, moves
// to running for the duration of each Prompt-driven turn, and returns to
// idle (a root) or done/failed (a child) when that turn completes.
//
// done and failed are reserved for CHILD sessions — ones with a non-empty
// parent, spawned as a unit of delegated work via Spawn (and, model-facing,
// the `task` tool). A child's spawning turn outcome is exactly what gets
// reported to its parent as a completion notification (see the design doc's
// Stage 3, docs/plans/2026-08-23-subagent-sessions-design.md). A root
// session — the CLI/server's existing single-session flow, or any session
// created with no parent — has no assignment to complete: it cycles
// running <-> idle forever, exactly like a bare engine.Session used outside
// any SessionManager, and only ever reaches canceled, via an explicit
// Cancel call. This asymmetry is a deliberate implementation choice (the
// design doc locks the five-state enum but not this edge), made so wiring a
// SessionManager under the existing interactive flow never turns an
// ordinary provider hiccup into a permanently "failed" root session.
type SessionStatus string

const (
	StatusRunning  SessionStatus = "running"
	StatusIdle     SessionStatus = "idle"
	StatusDone     SessionStatus = "done"
	StatusFailed   SessionStatus = "failed"
	StatusCanceled SessionStatus = "canceled"
)

// DefaultMaxTaskDepth and DefaultMaxConcurrentTasks are SessionManager's
// zero-value defaults, matching the design spec's HARNESS_MAX_TASK_DEPTH /
// HARNESS_MAX_CONCURRENT_TASKS config keys. The engine itself never reads
// environment variables — config/CLI wiring (Stage 4) is what turns those
// into the ints NewSessionManager takes.
const (
	DefaultMaxTaskDepth       = 3
	DefaultMaxConcurrentTasks = 20
)

var (
	// ErrUnknownSession is returned by any SessionManager operation given an
	// id it does not manage.
	ErrUnknownSession = errors.New("engine: unknown session id")
	// ErrDepthLimit is returned by Spawn when the parent is already at the
	// manager's depth limit. In normal operation the `task` tool is simply
	// withheld at that depth (see SessionManager.TaskToolAllowed, Stage 3),
	// so this only fires on the race the design doc calls out explicitly: a
	// call that raced past the tool's own withholding.
	ErrDepthLimit = errors.New("engine: task depth limit reached")
	// ErrConcurrencyLimit is returned by Spawn when the parent's tree
	// already has the manager's maximum number of children running.
	ErrConcurrencyLimit = errors.New("engine: tree concurrency limit reached")
	// ErrSessionCanceled is returned by Spawn or Send when the target
	// session has already been canceled.
	ErrSessionCanceled = errors.New("engine: session canceled")
)

// SessionManager owns every session — one root plus its descendant
// children — spawned as a tree in one harness process. It is the
// engine-level home for the subagent-sessions primitive (see the design
// doc): lifecycle tracking, depth and concurrency limits enforced at spawn
// time, and cascade cancellation. Stage 3 layers completion-notification
// delivery to a spawning parent on top of this.
//
// A harness process may run several independent SessionManagers, or none
// at all: nothing in engine.Session requires one. Today's direct
// NewSession/LoadSession callers (cmd/harness, server) are unaffected —
// they get the degenerate case the design doc names: a lone root with no
// children.
//
// A SessionManager's methods are safe for concurrent use.
type SessionManager struct {
	mu            sync.Mutex
	baseCtx       context.Context
	maxDepth      int
	maxConcurrent int
	nodes         map[string]*sessionNode

	// runningByRoot counts, per root session id, how many of that root's
	// DESCENDANTS (never the root itself) currently have status running —
	// the tree-wide concurrency budget the design doc requires ("counted
	// tree-wide from the root, not per level"). Checked and reserved
	// atomically under mu at spawn time, so a race at the limit is answered
	// with ErrConcurrencyLimit, never a lost-update overrun.
	runningByRoot map[string]int
}

// sessionNode is one managed session's lifecycle bookkeeping. Guarded by
// the owning SessionManager's mu.
type sessionNode struct {
	id        string
	session   *Session
	parentID  string // "" for a root
	rootID    string // self, for a root
	depth     int    // 0 for a root
	status    SessionStatus
	children  []string
	agentType string // the agent name Spawn was called with; "" for a root

	// ctx/cancel is this node's own lifetime, independent of any caller's
	// request context: a root's derives from the manager's baseCtx, a
	// child's from its parent node's ctx. Go's context tree does the
	// cascade-cancel fan-out for free — canceling a node's ctx cancels
	// every descendant's, since each was derived from its parent's.
	ctx    context.Context
	cancel context.CancelFunc

	// result/failReason hold a CHILD's spawning-turn outcome once status is
	// done or failed (see SessionStatus's doc comment) — result the child's
	// final assistant text, failReason a classified (#82-rule) short error
	// string. Both stay empty for a root, and for a child still running.
	result     string
	failReason string
}

// SessionNode is a read-only snapshot of one managed session's lifecycle
// bookkeeping, returned by Info. It does not alias SessionManager's
// internal state — mutating it has no effect.
type SessionNode struct {
	ID         string
	ParentID   string
	Depth      int
	Status     SessionStatus
	Children   []string
	AgentType  string
	Result     string
	FailReason string
}

func (n *sessionNode) snapshot() SessionNode {
	return SessionNode{
		ID:         n.id,
		ParentID:   n.parentID,
		Depth:      n.depth,
		Status:     n.status,
		Children:   append([]string(nil), n.children...),
		AgentType:  n.agentType,
		Result:     n.result,
		FailReason: n.failReason,
	}
}

// NewSessionManager returns a SessionManager whose root sessions derive
// their lifetime from baseCtx (a nil baseCtx defaults to
// context.Background()). maxDepth and maxConcurrent of zero or less use
// DefaultMaxTaskDepth/DefaultMaxConcurrentTasks.
func NewSessionManager(baseCtx context.Context, maxDepth, maxConcurrent int) *SessionManager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if maxDepth <= 0 {
		maxDepth = DefaultMaxTaskDepth
	}
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentTasks
	}
	return &SessionManager{
		baseCtx:       baseCtx,
		maxDepth:      maxDepth,
		maxConcurrent: maxConcurrent,
		nodes:         make(map[string]*sessionNode),
		runningByRoot: make(map[string]int),
	}
}

// NewRoot creates a fresh root session (depth 0, no parent) and registers it
// for lifecycle tracking — the degenerate case the design doc calls out:
// "today's single-session flow becomes root-with-no-children." The
// returned *Session is an ordinary engine.Session; drive it through
// SessionManager.Send (so lifecycle status stays accurate) or, for
// compatibility with existing single-session callers that never touch a
// SessionManager at all, direct Prompt calls.
func (m *SessionManager) NewRoot(cfg Config) *Session {
	cfg.SessionManager = m // installs the `task` tool unconditionally — see newSession's doc comment on Config.SessionManager
	s := NewSession(cfg)
	m.mu.Lock()
	m.adoptLocked(s, "", 0)
	m.installTaskToolLocked(s, 0)
	m.mu.Unlock()
	return s
}

// AdoptRoot registers an already-constructed session (e.g. one restored via
// LoadSession) as a root under lifecycle tracking, without creating
// anything new. It is an error to adopt a session whose id is already
// managed by m.
func (m *SessionManager) AdoptRoot(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.nodes[s.ID]; exists {
		return fmt.Errorf("engine: session %s already managed", s.ID)
	}
	s.cfg.SessionManager = m
	// s was built by a plain NewSession/LoadSession call, before
	// Config.SessionManager was set — newSession's unconditional install
	// (see its doc comment) never ran for it, so install directly here.
	s.tools[taskToolName] = taskTool()
	m.adoptLocked(s, "", 0)
	m.installTaskToolLocked(s, 0)
	return nil
}

// adoptLocked registers s as a new node. Callers hold m.mu.
func (m *SessionManager) adoptLocked(s *Session, parentID string, depth int) *sessionNode {
	parentCtx := m.baseCtx
	root := s.ID
	if parentID != "" {
		if p, ok := m.nodes[parentID]; ok {
			parentCtx = p.ctx
			root = p.rootID
		}
	}
	ctx, cancel := context.WithCancel(parentCtx)
	n := &sessionNode{
		id:       s.ID,
		session:  s,
		parentID: parentID,
		rootID:   root,
		depth:    depth,
		status:   StatusIdle,
		ctx:      ctx,
		cancel:   cancel,
	}
	m.nodes[s.ID] = n
	if parentID != "" {
		if p, ok := m.nodes[parentID]; ok {
			p.children = append(p.children, s.ID)
		}
	}
	return n
}

// Session returns the managed session by id.
func (m *SessionManager) Session(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return nil, false
	}
	return n.session, true
}

// Info returns a snapshot of id's lifecycle bookkeeping, or ok=false if id
// is not managed by m.
func (m *SessionManager) Info(id string) (info SessionNode, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return SessionNode{}, false
	}
	return n.snapshot(), true
}

// TaskToolAllowed reports whether a session at depth may spawn a child —
// i.e. whether the `task` tool should be registered on it at all (Stage 3).
// It is exported so the tool-registration path and Spawn's own depth check
// agree on the exact same boundary by construction, never two hand-copied
// comparisons that could drift apart.
func (m *SessionManager) TaskToolAllowed(depth int) bool {
	return depth < m.maxDepth
}

// installTaskToolLocked enforces TaskToolAllowed on s: newSession already
// installs the `task` tool unconditionally whenever Config.SessionManager
// is set (see that field's doc comment) — it has no notion of depth — so
// this only ever needs to REMOVE it, for a session at or past the depth
// limit. A no-op for a session below the limit. Called with m.mu held.
func (m *SessionManager) installTaskToolLocked(s *Session, depth int) {
	if !m.TaskToolAllowed(depth) {
		delete(s.tools, taskToolName)
	}
}

// SpawnOptions configures a child session created by Spawn.
type SpawnOptions struct {
	// ParentID is the spawning session's id. Required; must already be
	// managed by m.
	ParentID string
	// Prompt is the child's first user message. Spawn starts the child's
	// turn (in a new goroutine) before returning — the design doc's
	// "non-blocking execution": Spawn itself never waits for the turn.
	Prompt string
	// Model overrides the child's model. The zero value inherits the
	// parent's.
	Model message.ModelRef
	// SystemAppend, if non-empty, is appended as one more Config.System
	// segment — how an agent definition's body extends the child's system
	// prompt (Stage 2). Empty means no addition: the child's system prompt
	// is exactly the parent's (fx-style — no distinct subagent persona).
	SystemAppend string
	// ToolNames, if non-nil, restricts the child's tool registry to exactly
	// these names (each must already exist in the parent's registry, else
	// Spawn returns an error) — how an agent definition's `tools:` list
	// applies a read-only preset (Stage 2). Nil inherits the parent's full
	// tool set unchanged.
	ToolNames []string
	// AgentType is the agent name this child was spawned as (e.g.
	// "general-purpose", "explore", a custom .agents/*.md name) — purely
	// descriptive: carried on SessionNode.AgentType and in the completion
	// notification delivered to the parent (taskdelivery.go), never
	// interpreted by Spawn itself. Empty is fine (Send never sets it; a
	// direct Spawn caller that isn't the `task` tool may leave it empty
	// too).
	AgentType string
}

// Spawn creates a child of opts.ParentID, registers it under lineage/depth
// tracking, and immediately launches its first turn in a new goroutine.
// Spawn itself returns as soon as the child session exists and that turn
// has been launched, carrying the child's id — it never waits for the turn
// to finish.
//
// Depth and concurrency are enforced synchronously, under one lock, before
// the child is created: a caller at either limit gets ErrDepthLimit or
// ErrConcurrencyLimit back and no session to clean up, and a race between
// two Spawn calls at the concurrency limit is resolved by whichever
// acquires the lock first — the other sees the reservation and fails
// cleanly, per the design doc's "a race is still answered with an error,
// not a crash."
func (m *SessionManager) Spawn(opts SpawnOptions) (childID string, err error) {
	m.mu.Lock()
	parent, ok := m.nodes[opts.ParentID]
	if !ok {
		m.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrUnknownSession, opts.ParentID)
	}
	if parent.status == StatusCanceled {
		m.mu.Unlock()
		return "", ErrSessionCanceled
	}
	childDepth := parent.depth + 1
	if !m.TaskToolAllowed(parent.depth) {
		m.mu.Unlock()
		return "", ErrDepthLimit
	}
	if m.runningByRoot[parent.rootID] >= m.maxConcurrent {
		m.mu.Unlock()
		return "", ErrConcurrencyLimit
	}

	childCfg := parent.session.cfg
	childCfg.ParentSession = parent.id
	if !opts.Model.IsZero() {
		childCfg.Model = opts.Model
	}
	if opts.SystemAppend != "" {
		childCfg.System = append(append([]string(nil), childCfg.System...), opts.SystemAppend)
	}
	child := NewSession(childCfg) // installs `task` unconditionally, since childCfg.SessionManager is inherited from the parent
	m.installTaskToolLocked(child, childDepth)

	toolNames := opts.ToolNames
	if toolNames != nil && !m.TaskToolAllowed(childDepth) {
		// The requesting agent definition asked for "task" explicitly (a
		// non-leaf custom definition), but this child is at the depth
		// limit: withheld, exactly like the general-purpose (unrestricted)
		// case above — never a load-bearing error for hitting a limit that
		// is expected to bite eventually.
		filtered := make([]string, 0, len(toolNames))
		for _, name := range toolNames {
			if name != taskToolName {
				filtered = append(filtered, name)
			}
		}
		toolNames = filtered
	}
	if toolNames != nil {
		if err := restrictTools(child, toolNames); err != nil {
			m.mu.Unlock()
			return "", err
		}
	}

	n := m.adoptLocked(child, parent.id, childDepth)
	n.agentType = opts.AgentType
	// Reserve the concurrency slot NOW, synchronously, rather than when the
	// launched goroutine gets around to running — otherwise two Spawn calls
	// racing past the check above could both pass it before either
	// goroutine marks its child running, overrunning maxConcurrent. A
	// spawned child is handed work immediately, so it is never idle before
	// running: skip straight to running instead of the idle adoptLocked
	// sets by default.
	n.status = StatusRunning
	m.runningByRoot[parent.rootID]++
	m.mu.Unlock()

	go func() {
		msg, perr := child.Prompt(n.ctx, opts.Prompt)
		m.finalizeTurn(child.ID, msg, perr)
	}()

	return child.ID, nil
}

// Send delivers text to session id as its next turn and blocks until that
// turn completes. It is the mechanism behind the design doc's
// session.send (Stage 4): SessionManager offers exactly one non-blocking
// entry point, Spawn (see its doc comment and the design doc's
// "Non-blocking execution" locked decision) — Send, like a bare
// engine.Session.Prompt, runs synchronously.
//
// The turn is bounded by whichever of ctx or id's own cascade-cancel
// lifetime ends first, so a Cancel call on an ancestor interrupts an
// in-flight Send exactly like it interrupts a Spawn-launched goroutine.
func (m *SessionManager) Send(ctx context.Context, id, text string) (*message.Message, error) {
	m.mu.Lock()
	n, ok := m.nodes[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	if n.status == StatusCanceled {
		m.mu.Unlock()
		return nil, ErrSessionCanceled
	}
	if n.status != StatusRunning {
		n.status = StatusRunning
		if n.depth > 0 {
			m.runningByRoot[n.rootID]++
		}
	}
	s := n.session
	nodeCtx := n.ctx
	m.mu.Unlock()

	runCtx, stop := mergeCancel(ctx, nodeCtx)
	defer stop()
	msg, err := s.Prompt(runCtx, text)
	m.finalizeTurn(id, msg, err)
	return msg, err
}

// finalizeTurn records the outcome of one turn just run via Prompt (Spawn's
// launched goroutine or Send's synchronous call) and decrements the
// concurrency reservation Spawn/Send made for it.
func (m *SessionManager) finalizeTurn(id string, msg *message.Message, perr error) {
	m.mu.Lock()
	n, ok := m.nodes[id]
	if !ok || n.status == StatusCanceled {
		// Cancel already finalized (or the node was reaped); leave its
		// recorded terminal state alone.
		m.mu.Unlock()
		return
	}
	m.decrementRunningLocked(n)

	var notify *taskNotification
	switch {
	case n.parentID == "":
		// Root sessions have no parent to notify and no assignment to
		// complete — see SessionStatus's doc comment. This also covers a
		// resume turn's own completion (triggerResumeLocked only ever
		// targets an idle node, and only a root ever reaches idle — see
		// below), so a resume turn delivering a notification never itself
		// produces a new one: no cascade up the tree.
		n.status = StatusIdle
	case perr != nil:
		n.status = StatusFailed
		n.failReason = classifySpawnError(perr)
		notify = &taskNotification{ChildID: n.id, Status: StatusFailed, FailReason: n.failReason, Usage: n.session.Usage()}
	default:
		n.status = StatusDone
		n.result = msg.Parts.Text()
		notify = &taskNotification{ChildID: n.id, Status: StatusDone, Result: n.result, Usage: n.session.Usage()}
	}

	// resume is set (and run AFTER releasing m.mu, below) only when this
	// completion needs to actively wake an idle parent — see
	// triggerResumeLocked's doc comment for why claiming the parent's
	// running slot must happen in THIS same critical section, not in a
	// separately-locked follow-up call, to avoid two children finishing at
	// once both observing "idle" and both starting a turn on the same
	// parent session (which Session.Prompt's own contract forbids).
	var resume func()
	if notify != nil {
		if parent, ok := m.nodes[n.parentID]; ok {
			notify.Agent = n.agentType
			parent.session.enqueueTaskNotification(*notify)
			if parent.status == StatusIdle {
				resume = m.triggerResumeLocked(parent)
			}
		}
	}
	m.mu.Unlock()

	if resume != nil {
		go resume()
	}
}

// triggerResumeLocked claims node's running slot (node MUST currently be
// idle) and returns a function that actually drives the resume turn — the
// design doc's "engine-initiated resume turn," a new engine capability.
// Call the returned function AFTER releasing m.mu, in a new goroutine: it
// blocks on a full Session.Prompt call and must never run under the lock.
//
// The claim (status flip + concurrency reservation) happens here,
// synchronously, for the same reason Spawn reserves its concurrency slot
// before launching its goroutine: two notifications arriving for the same
// idle parent in the same locked critical section (finalizeTurn) must
// result in exactly ONE resume turn, not two concurrent Prompt calls on
// the same session.
func (m *SessionManager) triggerResumeLocked(node *sessionNode) func() {
	node.status = StatusRunning
	if node.depth > 0 {
		m.runningByRoot[node.rootID]++
	}
	s := node.session
	ctx := node.ctx
	return func() {
		msg, err := s.Prompt(ctx, taskResumeTriggerText)
		m.finalizeTurn(s.ID, msg, err)
	}
}

func (m *SessionManager) decrementRunningLocked(n *sessionNode) {
	if n.depth == 0 {
		return
	}
	if c := m.runningByRoot[n.rootID]; c > 0 {
		m.runningByRoot[n.rootID] = c - 1
	}
}

// Cancel cancels id and its entire subtree, marking every non-terminal node
// canceled before returning — the design doc's "canceling a parent cancels
// its entire subtree before the parent finalizes." Canceling an
// already-terminal node (done, failed, or canceled) leaves ITS OWN recorded
// outcome alone, but Cancel still walks into its children: a child keeps
// running independently of its parent's own turn outcome (a parent can go
// done while a task it spawned is still in flight), so a done or failed
// parent can still have live descendants that need tearing down.
func (m *SessionManager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	m.cancelSubtreeLocked(n)
	return nil
}

func (m *SessionManager) cancelSubtreeLocked(n *sessionNode) {
	switch n.status {
	case StatusDone, StatusFailed, StatusCanceled:
		// Leave the recorded terminal outcome alone.
	default:
		m.decrementRunningLocked(n)
		n.status = StatusCanceled
	}
	// Canceling the context aborts an in-flight Prompt call (Spawn's
	// goroutine or a concurrent Send) regardless of this node's status —
	// always safe, and a no-op if already canceled.
	n.cancel()
	for _, cid := range n.children {
		if c, ok := m.nodes[cid]; ok {
			m.cancelSubtreeLocked(c)
		}
	}
}

// mergeCancel returns a context canceled when either a or b is done,
// releasing its resources via stop. Used so a caller-supplied context (an
// HTTP request's deadline) and a session node's own cascade-cancel
// lifetime both bound one turn.
func mergeCancel(a, b context.Context) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancel(a)
	stopAfter := context.AfterFunc(b, cancel)
	return ctx, func() {
		stopAfter()
		cancel()
	}
}

// restrictTools narrows s's tool registry in place to exactly names,
// returning an error if any name is not present in the registry s was
// already built with. This is the single enforcement point an agent
// definition's `tools:` list uses (Stage 2) to apply a read-only preset —
// reused here directly rather than duplicated.
func restrictTools(s *Session, names []string) error {
	keep := make(map[string]Tool, len(names))
	for _, name := range names {
		t, ok := s.tools[name]
		if !ok {
			return fmt.Errorf("engine: unknown tool %q", name)
		}
		keep[name] = t
	}
	s.tools = keep
	return nil
}

// classifySpawnError maps a raw Prompt error from a managed session's turn
// into a short, secret-free reason for a child's completion notification —
// the #82 leak rule (see classifyGoalWorkerError in goal.go, the sibling
// this mirrors): never surface err.Error() verbatim, since a provider error
// can carry request or response bytes with API keys or endpoint URLs baked
// in.
func classifySpawnError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if class, retryable := provider.AsRetryable(err); retryable {
		return fmt.Sprintf("provider %s errors exhausted the retry budget", class)
	}
	if provider.AsPermanent(err) {
		return "turn failed with a permanent provider error and cannot succeed on retry"
	}
	return "turn failed and did not recover"
}
