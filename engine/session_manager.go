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
	// ErrSessionBusy is returned by Send when session id already has a
	// turn in flight. Session.Prompt must never be called concurrently
	// with itself (see the Session type's doc comment); Send is the one
	// SessionManager entry point a caller might plausibly invoke twice
	// concurrently for the same id (two overlapping session.send calls),
	// so it must refuse the second rather than starting a second
	// concurrent Prompt loop.
	ErrSessionBusy = errors.New("engine: session is already running a turn")
)

// ExternalRunner lets an outside scheduler — the server's own run-slot
// machinery (claimForPrompt/runPrompt in package server) — drive a ROOT
// session's resume turn instead of SessionManager calling Session.Prompt
// directly. Installed via NewSessionManager's caller setting the field
// directly is not possible (unexported); use SetExternalRunner.
//
// # Why this exists
//
// A root session created through a SessionManager in `harness serve` is
// ALSO independently driven by ordinary POST /session/{id}/prompt_async
// requests, through the server's own admission gate, entirely outside
// this package. If SessionManager ALSO called Session.Prompt directly to
// deliver a child's completion notification to an idle root, the two
// schedulers could race: an ordinary prompt and a SessionManager-
// initiated resume both starting a turn on the SAME session at once,
// violating Session.Prompt's "not concurrently with itself" contract —
// exactly the class of bug a live -race run against an early version of
// this package caught. A CHILD session never has this problem: it is
// never resident-tracked by anything but its SessionManager. So
// ExternalRunner is consulted ONLY for depth-0 (root) nodes — see
// triggerResumeLocked — never for a child's own turns.
//
// Run(id, text) asks the external scheduler to run a turn on id with text
// as the prompt. handled reports whether the scheduler recognizes id at
// all — true even if it did not actually START a turn this instant (e.g.
// id was already busy with something else the scheduler itself already
// knows about): in that case that ALREADY-in-flight turn's own next
// request will pick up the pending notification via the ordinary
// queue-at-next-turn-boundary path, so no further action is needed here.
// handled is false only for an id the external scheduler has never heard
// of (e.g. a nil or not-yet-wired runner); SessionManager then falls back
// to driving the turn itself, exactly as it did before ExternalRunner
// existed.
//
// The scheduler is responsible for reporting the turn's start and end
// back via ReportTurnStart/ReportTurnEnd: SessionManager has no other way
// to learn a delegated turn completed.
type ExternalRunner func(id, text string) (handled bool)

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

	// externalRunner, when set, drives a ROOT's resume turn instead of m
	// calling Session.Prompt directly — see ExternalRunner's doc comment.
	// Nil (the default, and always for a bare-engine/CLI SessionManager
	// with no server layered over it) means m drives every turn itself,
	// root or child, exactly as before ExternalRunner existed.
	externalRunner ExternalRunner
}

// SetExternalRunner installs runner as described on the ExternalRunner
// type — nil restores the default (m drives every turn itself). Safe to
// call at any time; takes effect on the next resume decision.
func (m *SessionManager) SetExternalRunner(runner ExternalRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.externalRunner = runner
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
	m.adoptRootLocked(s)
	return nil
}

// adoptRootLocked registers s as a fresh root node, installing the `task`
// tool directly if s was built before Config.SessionManager was set (a
// plain NewSession/LoadSession call, so newSession's own unconditional
// install — see that field's doc comment — never ran). Safe to call only
// when s has not yet been exposed to any concurrent user: both of this
// method's callers (AdoptRoot, and ReportTurnStart's adopt-on-first-sight)
// mutate s.tools/s.cfg here BEFORE the caller's own Session.Prompt call
// begins — sequential with respect to that call, in the same goroutine,
// never concurrent with it. Callers hold m.mu.
func (m *SessionManager) adoptRootLocked(s *Session) *sessionNode {
	s.cfg.SessionManager = m
	s.tools[taskToolName] = taskTool()
	n := m.adoptLocked(s, "", 0)
	m.installTaskToolLocked(s, 0)
	return n
}

// ReportTurnStart tells m that an EXTERNAL scheduler (the server's own
// run-slot machinery) is about to drive a turn on sess — see
// ExternalRunner's doc comment. If sess is not yet a tracked node at all —
// a session this process is touching for the first time via a cold reload
// from disk (claimForPrompt's transparent LoadSession fallback covers a
// session evicted from residency, or one that predates this process
// entirely after a restart) — it is adopted as a fresh root here, exactly
// like AdoptRoot. This is what makes `task` and session.info's lineage
// keep working for a session resumed after a restart or eviction, not
// only one created via THIS process's own POST /session (closing the gap
// a session that already had children before the restart would otherwise
// hit: task calls failing "parent session no longer tracked" forever
// after).
//
// A no-op if sess is already tracked and already running (defensive:
// should not happen if the caller's own admission gate is sound, but
// idempotent rather than corrupting the concurrency count either way).
func (m *SessionManager) ReportTurnStart(sess *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[sess.ID]
	if !ok {
		n = m.adoptRootLocked(sess)
	}
	// Always re-attach to the LIVE object, even for an already-tracked
	// node: the server's residency system (MaxResident) can evict and
	// later reload a root into a NEW *engine.Session with the same id
	// (s.opts.LoadSession, claimForPrompt's cold-load path) — if this
	// only ran on first sight, the node would keep pointing at the OLD,
	// evicted object forever. A background child completing in the gap
	// would then enqueue its notification onto that dead object, which
	// the live reloaded session's own checkoutTaskNotificationsSegment
	// never reads: the result silently vanishes. A live review caught
	// this exact failure mode.
	n.session = sess
	n.status = StatusRunning
}

// ReportTurnEnd tells m that an external scheduler's turn on id (started
// via ReportTurnStart) just finished — the same finalization finalizeTurn
// performs for a turn m drove itself, exported for a caller outside this
// package. A no-op for an id m does not track.
//
// The call itself — not just firing the resume it returns — MUST be
// deferred until after the caller has released its OWN run-slot claim on
// id (the server's freeRunSlotAndEmitIdle). This finalization is what
// flips id's node to StatusIdle/StatusDone/StatusFailed, making it
// visible to every OTHER concurrent goroutine going through this same
// package (in particular finalizeTurn's nearestLiveAncestorLocked,
// delivering an unrelated child's completion here). If that visibility
// arrives before the real slot is free, such a goroutine's own resume
// attempt sees id idle, races to claim the slot, is refused because it is
// still held, and — since a refused-but-recognized claim still counts as
// "handled" — permanently strands the notification with nothing left to
// retry it. A live CI hang (a test blocked forever on a resume dropped
// exactly this way) is what caught this. See runPrompt/runGoal
// (server/handlers.go), which both call this after freeRunSlotAndEmitIdle
// and fire the returned resume immediately after, in that same tail
// position maybeDispatchQueued/maybeAutoArmGoal already occupy.
//
// Returns a resume func when id's own completion needs to immediately
// start ANOTHER turn on itself (a notification arrived too late for this
// turn's own checkout — see finalizeTurn's doc comment).
func (m *SessionManager) ReportTurnEnd(id string, msg *message.Message, err error) (resume func()) {
	return m.finalizeTurn(id, msg, err)
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

// Reap removes every LEAF (no children) node whose status is terminal
// (done, failed, canceled) and which has a parent — a root is never
// removed, since it is the tree's own address and a caller may still hold
// (or later reload) its id. Freeing a terminal child's node frees the
// *Session it was pinning in memory, message history included.
//
// This package never reaps automatically: m.nodes otherwise grows
// unbounded on a long-lived process that fans out many `task` children
// (a live review finding), each one pinned forever even once its result
// has long since been delivered and read — but this package has no way
// to know how long a caller wants a settled child's result to stay
// reachable via Info/session.info, so it leaves that retention policy
// entirely to the caller: call Reap periodically (e.g. "N minutes after
// terminal") on whatever schedule fits. Removing a leaf also drops its id
// from its parent's Children list, so a parent that has ALREADY had every
// child reaped becomes a leaf itself on the next call, letting a whole
// terminal subtree collapse bottom-up over repeated calls. Returns the
// number of nodes removed.
func (m *SessionManager) Reap() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Collect eligible ids from a SNAPSHOT of each node's state first,
	// before removing anything: Go's map iteration order is randomized
	// per run, and reaping a leaf in the same pass that also visits its
	// now-childless parent would let ONE call cascade through more than
	// one generation depending on which order the runtime happens to
	// visit them in — non-deterministic behavior a live test run caught
	// (the exact same tree reaped 1 node on one run, 2 on another). Two
	// passes over a fixed snapshot make one call always remove exactly
	// one generation of leaves, regardless of iteration order — repeated
	// calls still collapse a whole terminal subtree bottom-up, just one
	// generation per call, matching this method's documented contract.
	var eligible []string
	for id, n := range m.nodes {
		if n.parentID == "" || len(n.children) != 0 {
			continue
		}
		switch n.status {
		case StatusDone, StatusFailed, StatusCanceled:
			eligible = append(eligible, id)
		}
	}

	for _, id := range eligible {
		n := m.nodes[id]
		delete(m.nodes, id)
		if p, ok := m.nodes[n.parentID]; ok {
			kept := p.children[:0]
			for _, cid := range p.children {
				if cid != id {
					kept = append(kept, cid)
				}
			}
			p.children = kept
		}
	}
	return len(eligible)
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

	// configSnapshot (not a raw parent.session.cfg read) is required here:
	// see its doc comment — an unsynchronized cfg read races SetModel's
	// writes under the parent's own lock, and would silently inherit the
	// parent's STALE construction-time model rather than the live one the
	// design doc's inheritance rule actually means.
	childCfg := parent.session.configSnapshot()
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
		if resume := m.finalizeTurn(child.ID, msg, perr); resume != nil {
			go resume()
		}
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
//
// Send does NOT consult ExternalRunner: it always drives id's turn
// directly. For a root session under a SessionManager whose
// ExternalRunner is set (harness serve), the caller should route a
// send through that external scheduler's own admission path instead
// (see server.Server's handleSessionSend, which does exactly this) —
// calling Send for such a root would compete with the external
// scheduler's own turns on the same session. Send remains the correct,
// safe way to drive ANY session (root or child) when no ExternalRunner is
// set — the ordinary case for a CHILD always, and for a root in bare-
// engine/CLI usage with no server layered over it.
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
	if n.status == StatusRunning {
		// Refuse rather than proceed: Session.Prompt must never be called
		// concurrently with itself, and this is the one SessionManager
		// entry point a caller might plausibly invoke twice at once for
		// the same id (see ErrSessionBusy's doc comment) — a bug a live
		// -race run against an earlier version of this method caught,
		// where this check was missing and a second concurrent Send
		// fell through to a second concurrent Prompt call.
		m.mu.Unlock()
		return nil, ErrSessionBusy
	}
	if n.depth > 0 && m.runningByRoot[n.rootID] >= m.maxConcurrent {
		// A done/failed CHILD is eligible for Send (a legitimate follow-up
		// message — see the design doc's session.send), and Send must
		// enforce the SAME tree-wide concurrency budget Spawn does: without
		// this check, enough concurrent session.send calls against
		// already-settled children could push runningByRoot above
		// maxConcurrent, the same overrun Spawn's own check exists to
		// prevent.
		m.mu.Unlock()
		return nil, ErrConcurrencyLimit
	}
	n.status = StatusRunning
	if n.depth > 0 {
		m.runningByRoot[n.rootID]++
	}
	s := n.session
	nodeCtx := n.ctx
	m.mu.Unlock()

	runCtx, stop := mergeCancel(ctx, nodeCtx)
	defer stop()
	msg, err := s.Prompt(runCtx, text)
	if resume := m.finalizeTurn(id, msg, err); resume != nil {
		go resume()
	}
	return msg, err
}

// finalizeTurn records the outcome of one turn just run via Prompt (Spawn's
// launched goroutine, Send's synchronous call, triggerResumeLocked's
// goroutine, or an external scheduler's ReportTurnEnd) and decrements the
// concurrency reservation whichever of those made for it. Every call to
// finalizeTurn corresponds to EXACTLY one prior status->running transition
// (Spawn/Send/triggerResumeLocked all reserve synchronously before
// launching the turn; ReportTurnStart is a no-op decrement source since a
// root never counts toward runningByRoot regardless — see
// decrementRunningLocked) — cancelSubtreeLocked deliberately does NOT
// decrement for a running node it cancels, precisely so this remains the
// SOLE decrementer and a cancel racing a natural completion can never
// double-decrement (see cancelSubtreeLocked's doc comment).
func (m *SessionManager) finalizeTurn(id string, msg *message.Message, perr error) (resume func()) {
	m.mu.Lock()
	n, ok := m.nodes[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	alreadyCanceled := n.status == StatusCanceled
	m.decrementRunningLocked(n)

	// resume is set here (never fired directly — see below) whenever this
	// call needs to actively start another turn rather than simply
	// settling n idle or terminal — either because n itself (a root) has
	// pending work that arrived too late for its own last checkout (see
	// the root case below), or because n's completion needs to wake an
	// ancestor (set further down, after notify is built). Claiming the
	// run slot happens synchronously, in whichever branch sets this, for
	// the same reason Spawn reserves its concurrency slot before
	// launching its goroutine — see triggerResumeLocked's doc comment.
	var notify *taskNotification
	switch {
	case n.parentID == "":
		// Root sessions have no parent to notify and no assignment to
		// complete — see SessionStatus's doc comment. A root already
		// marked canceled (Cancel() raced ahead of this call) STAYS
		// canceled regardless of how the in-flight turn actually
		// concluded — cancellation is not undone by a benign race with
		// natural completion.
		switch {
		case alreadyCanceled:
			// stays canceled
		case perr == nil && n.session.hasPendingTaskNotifications():
			// A notification for n arrived AFTER n's own last
			// checkoutTaskNotificationsSegment call (inside its
			// just-finished, SUCCESSFUL turn) but before this
			// finalizeTurn call — a sibling child completing during n's
			// final model call, say. Re-trigger immediately instead of
			// settling idle and stranding it there with nothing left to
			// wake it: an idle session only ever gets woken by a NEW
			// notification arriving (finalizeTurn's ancestor-delivery
			// branch below), and this one already arrived, so nothing
			// else will ever trigger this resume if we don't do it right
			// here.
			//
			// Gated on perr == nil deliberately: on a FAILED turn,
			// requeueTaskNotifications (runAgenticLoop, engine.go) just
			// put whatever THIS attempt itself couldn't deliver back into
			// pending — checking pending here too, unconditionally, would
			// see that requeued entry and immediately retrigger another
			// attempt against the very provider call that just failed,
			// forever, on any persistent failure (a real hang an early
			// version of this fix produced under test). A failed turn's
			// root simply goes idle, same as always; the requeued
			// notification waits for the NEXT legitimate trigger — a real
			// Send, or another child's completion finding this root idle
			// through the ordinary ancestor-delivery path below.
			resume = m.triggerResumeLocked(n)
		default:
			n.status = StatusIdle
		}
	case alreadyCanceled:
		// Cancel() already set the terminal node status directly (see
		// cancelSubtreeLocked); still build a notification so the parent
		// learns the cancellation happened — the design doc lists
		// cancellation among a child's terminal outcomes a parent must be
		// told about ("A child that errors terminally (...cancellation)
		// delivers a failed notification"), never silently swallowed. The
		// reason is a fixed "canceled" (not classifySpawnError(perr)):
		// perr may even be nil here (the turn could have raced to a
		// genuine success in the same instant Cancel() marked it
		// canceled), and the node's OWN status already carries the
		// distinct StatusCanceled value — this FailReason is only ever
		// read from the notification text, never compared against status.
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusFailed, FailReason: "canceled", Usage: n.session.Usage()}
	case perr != nil:
		n.status = StatusFailed
		n.failReason = classifySpawnError(perr)
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusFailed, FailReason: n.failReason, Usage: n.session.Usage()}
	default:
		n.status = StatusDone
		n.result = msg.Parts.Text()
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusDone, Result: n.result, Usage: n.session.Usage()}
	}

	// n.parentID != "" here exactly when notify != nil was possible (the
	// three non-root cases above) — a CHILD that just went terminal
	// itself (done/failed/canceled) will never run another turn of its
	// own (see SessionStatus's doc comment), so if it was ALSO a parent
	// with its own pending notifications (from grandchildren that
	// completed too late for it to ever check out itself), those would
	// be stranded forever on a node that will never read its queue again
	// — forward them to the SAME nearest-live-ancestor target its own
	// completion notification uses, rather than dropping them. A live
	// review caught this exact gap.
	var forwarded []taskNotification
	if n.parentID != "" && n.session.hasPendingTaskNotifications() {
		forwarded = n.session.drainAllTaskNotifications()
	}

	if notify != nil || len(forwarded) > 0 {
		if target := m.nearestLiveAncestorLocked(n); target != nil {
			if notify != nil {
				target.session.enqueueTaskNotification(*notify)
			}
			for _, fn := range forwarded {
				target.session.enqueueTaskNotification(fn)
			}
			if target.status == StatusIdle {
				resume = m.triggerResumeLocked(target)
			}
			// target.status == StatusRunning: queued, picked up at
			// target's own next turn boundary — no action needed here.
		}
		// No live ancestor at all (nil): every ancestor up to and
		// including the root is already done/failed/canceled — the whole
		// tree is being torn down (or was already fully settled) and
		// there is nowhere left to deliver these notifications. Dropped,
		// not an error: nothing is listening.
	}
	m.mu.Unlock()

	// Deliberately returned, never fired here (no "go resume()"): the
	// SERVER's ReportTurnEnd caller (runPrompt) must defer firing a
	// SELF-resume (the root case above, waking id itself) until AFTER
	// its own freeRunSlotAndEmitIdle call — firing it any earlier races
	// that slot release: resumeSessionForTaskNotification's own
	// claimForPrompt would see the OLD turn's slot still held, refuse,
	// and permanently strand both the notification and the node at
	// StatusRunning (nothing else would ever call ReportTurnEnd for a
	// resume attempt that never actually started). A live review
	// reproduced this race. Every OTHER caller of finalizeTurn (Spawn's
	// goroutine, Send, triggerResumeLocked's own returned closures) has
	// no such ordering constraint and fires the returned func immediately
	// in its own new goroutine.
	return resume
}

// nearestLiveAncestorLocked walks n's ancestor chain, starting at its
// direct parent, and returns the first one that is NOT done, failed, or
// canceled — i.e. still able to receive and eventually act on a
// notification (running or idle). Returns nil if every ancestor up to and
// including the root is terminal.
//
// This is what makes nesting past one level actually deliver (design
// doc's locked decision #5, generalized): a child settles done/failed
// after its own unit of work and never runs again on its own initiative
// (see SessionStatus's doc comment) — so a GRANDCHILD's completion,
// arriving after its direct parent has already gone done/failed, is
// reparented to the nearest ancestor that can still act on it, rather
// than being enqueued onto a node that will never read its queue again
// (silently dropping the result — the bug this closes). Called with m.mu
// held.
func (m *SessionManager) nearestLiveAncestorLocked(n *sessionNode) *sessionNode {
	id := n.parentID
	for id != "" {
		p, ok := m.nodes[id]
		if !ok {
			return nil
		}
		switch p.status {
		case StatusDone, StatusFailed, StatusCanceled:
			id = p.parentID
			continue
		default:
			return p
		}
	}
	return nil
}

// triggerResumeLocked claims node's running slot (node MUST currently be
// idle) and returns a function that actually drives the resume turn — the
// design doc's "engine-initiated resume turn," a new engine capability.
// Call the returned function AFTER releasing m.mu, in a new goroutine.
//
// The claim (status flip + concurrency reservation) happens here,
// synchronously, for the same reason Spawn reserves its concurrency slot
// before launching its goroutine: two notifications arriving for the same
// idle target in the same locked critical section (finalizeTurn) must
// result in exactly ONE resume turn, not two concurrent Prompt calls on
// the same session.
//
// For a depth-0 (root) node with an ExternalRunner set, the returned
// function delegates to it instead of calling Session.Prompt directly —
// see ExternalRunner's doc comment for why: a root can ALSO be driven by
// an entirely separate scheduler (the server's ordinary prompt_async
// path), and calling Prompt here too would race it. A child never has an
// external scheduler, so this delegation never applies to one.
func (m *SessionManager) triggerResumeLocked(node *sessionNode) func() {
	node.status = StatusRunning
	if node.depth > 0 {
		m.runningByRoot[node.rootID]++
	}
	s := node.session
	ctx := node.ctx
	id := node.id
	if node.depth == 0 && m.externalRunner != nil {
		runner := m.externalRunner
		return func() {
			if runner(id, taskResumeTriggerText) {
				// The external scheduler now owns this turn and is
				// responsible for reporting its completion back via
				// ReportTurnStart/ReportTurnEnd itself (and for deferring
				// any further resume THAT call returns past its own
				// run-slot release — see ReportTurnEnd's doc comment).
				return
			}
			// The scheduler doesn't recognize this id at all: fall back
			// to driving it directly rather than losing the resume. This
			// call owns the WHOLE turn itself (no server run-slot
			// involved), so firing any further resume immediately is
			// safe — no release to race.
			msg, err := s.Prompt(ctx, taskResumeTriggerText)
			if resume := m.finalizeTurn(id, msg, err); resume != nil {
				go resume()
			}
		}
	}
	return func() {
		msg, err := s.Prompt(ctx, taskResumeTriggerText)
		if resume := m.finalizeTurn(id, msg, err); resume != nil {
			go resume()
		}
	}
}

// decrementRunningLocked releases the concurrency reservation a running
// node holds. A depth-0 (root) node never counted toward runningByRoot in
// the first place (only DESCENDANTS count — see runningByRoot's doc
// comment), so this is a no-op for one; safe to call unconditionally.
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
//
// For a ROOT session driven by an external scheduler (harness serve), this
// only cancels node.ctx — which an external-scheduler-driven turn does NOT
// use for its actual Session.Prompt call (see ExternalRunner's doc
// comment: that turn runs on the scheduler's OWN context). Aborting such a
// turn requires the external scheduler's own abort path too; see
// server.Server's handleCancelTree, which calls both.
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

// cancelSubtreeLocked marks n and its descendants canceled and cancels
// each one's context. It deliberately does NOT call decrementRunningLocked
// for a node it finds running: that node's own in-flight turn is still
// live (a goroutine somewhere is blocked in Session.Prompt, or about to
// be handed to one) and WILL eventually call finalizeTurn once it
// actually returns — finalizeTurn is the sole decrementer for that
// reservation (see its own doc comment). Decrementing here too, for a
// node whose finalizeTurn call hasn't happened yet, would double-release
// the same slot the moment that call finally lands — exactly the
// "corollary" double-decrement a live -race/logic review caught in an
// earlier version of this method, which let the tree-wide concurrency cap
// be exceeded.
func (m *SessionManager) cancelSubtreeLocked(n *sessionNode) {
	if n.status != StatusDone && n.status != StatusFailed && n.status != StatusCanceled {
		n.status = StatusCanceled
	}
	// Canceling the context aborts an in-flight Prompt call driven
	// DIRECTLY by this package (Spawn's goroutine, Send, or a
	// SessionManager-driven resume) regardless of this node's status —
	// always safe, and a no-op if already canceled. It does NOT abort a
	// turn an ExternalRunner is driving on its own context — see Cancel's
	// doc comment.
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
