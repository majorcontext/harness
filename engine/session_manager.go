package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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

// treeTokenTotal is the single shared definition of "how many tokens does
// this tree's usage count as" for SetMaxTreeTokens purposes — all four
// provider.Usage fields (input, output, cache read, cache write) summed,
// never just a subset. Used by Spawn's own ErrBudgetExceeded gate (see
// that error's own doc comment for the live review finding this closes:
// an earlier gate compared only input+output, silently exempting a
// cache-heavy tree's largest cost component from its own budget), and
// exists specifically so the gate and usageByRoot's own accumulation
// (which already folded all four fields, correctly, before the gate was
// fixed to match) can never drift apart into two different ideas of
// "spend" again.
func treeTokenTotal(u provider.Usage) int {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

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
	// ErrBudgetExceeded is returned by Spawn when the parent's tree has
	// already accumulated at least SetMaxTreeTokens' configured budget in
	// cumulative token usage — ALL FOUR provider.Usage fields summed
	// (input, output, cache read, cache write), matching usageByRoot's
	// own accumulation exactly (see that field's doc comment) — a
	// follow-up finding ("per-tree budgets"), mirroring ErrDepthLimit/
	// ErrConcurrencyLimit's identical shape and Spawn call-site placement
	// otherwise. Unset (SetMaxTreeTokens never called, or called with a
	// non-positive value) disables the check entirely — this is an
	// opt-in limit, unlike depth/concurrency, which always have a
	// product default (DefaultMaxTaskDepth/DefaultMaxConcurrentTasks).
	//
	// A live review finding: an earlier version of this gate compared
	// only input+output against the budget, while usageByRoot itself
	// already accumulated all four fields — a cache-heavy tree (the
	// openaicompat/Fireworks and anthropic routes AGENTS.md calls out,
	// where a large prompt resent every turn reads mostly from cache)
	// could keep spawning children well past the operator's real
	// intended ceiling, because the largest component of its actual
	// spend was never measured by the gate that is supposed to enforce
	// it. Bill what costs money: cache read/write tokens are billed too
	// (typically at a discount versus a fresh input token, never free),
	// so they count toward the same budget the raw input/output tokens
	// do.
	ErrBudgetExceeded = errors.New("engine: task tree token budget exceeded")
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
	// ErrNotDescendant is returned by CancelDescendant, DescendantInfo, and
	// SendToDescendant when the named target is not actually a descendant
	// of the calling session in m's tree — the "only ancestors may act on
	// their descendants" rule the model-facing `task` tool's cancel/
	// status/send verbs all share (see isDescendantLocked's own doc
	// comment for exactly what counts). Deliberately indistinguishable, on
	// the wire, from "session_id names something entirely unrelated to
	// you" — an unrelated tree and a sibling's subtree are the same
	// authorization failure from the caller's point of view, and neither
	// is a hint worth leaking about the wider tree's shape.
	ErrNotDescendant = errors.New("engine: target session is not a descendant of the calling session")
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
// as the prompt, and reports one of three outcomes — a live review
// finding: an earlier revision returned a plain bool, folding two
// genuinely different cases into a single `true` and leaving a THIRD
// call (RevertResumeIfStillRunning) as the only thing distinguishing
// them — nothing in the type system stopped a future ExternalRunner
// implementation from forgetting that second call for its own refusal
// cases, and RevertResumeIfStillRunning's own doc comment had to flag
// this exact gotcha explicitly rather than the compiler enforcing it.
//
//   - RunnerHandled: the scheduler recognizes id and SOME bracketed turn
//     will eventually settle this resume's commitment — either it just
//     started one, or id was already busy with something the scheduler
//     itself already knows about (that already-in-flight turn's own next
//     request will pick up the pending notification via the ordinary
//     queue-at-next-turn-boundary path). No further action needed.
//   - RunnerRefused: the scheduler recognizes id but is refusing this
//     specific resume attempt right now (e.g. a workdir conflict, or the
//     scheduler draining) — no bracketed turn will ever settle it.
//     triggerResumeLocked itself now calls RevertResumeIfStillRunning
//     centrally for this case (see its own call site below) — an
//     ExternalRunner implementation no longer needs to remember to call
//     it itself.
//   - RunnerUnknown: the scheduler has never heard of id at all (e.g. a
//     nil or not-yet-wired runner). SessionManager falls back to driving
//     the turn itself, exactly as it did before ExternalRunner existed.
//
// A scheduler that reports RunnerHandled or RunnerRefused is responsible
// for reporting any turn it actually started back via
// ReportTurnStart/ReportTurnEnd: SessionManager has no other way to learn
// a delegated turn completed.
type RunnerOutcome int

const (
	// RunnerUnknown is the zero value — deliberately, so a caller that
	// forgets to set a return value (or an old bool-returning stub caught
	// by the compiler needing a real migration) fails closed into the
	// SAFEST case (SessionManager drives the turn itself) rather than
	// silently behaving like RunnerHandled (drop the resume) or
	// RunnerRefused (revert it).
	RunnerUnknown RunnerOutcome = iota
	RunnerHandled
	RunnerRefused
)

type ExternalRunner func(id, text string) RunnerOutcome

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

	// changed is closed and replaced on every m.mu release once
	// watchChanges is set, waking Changed observers. See Changed.
	changed      chan struct{}
	watchChanges bool

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

	// maxTreeTokens is the opt-in per-tree token budget (see
	// ErrBudgetExceeded's own doc comment) — 0 (the default; SetMaxTreeTokens
	// never called) disables the check entirely, unlike maxDepth/
	// maxConcurrent, which always have a positive product default applied
	// in NewSessionManager.
	maxTreeTokens int
	// usageByRoot accumulates cumulative token usage (all four
	// provider.Usage fields — see treeTokenTotal) per root session id,
	// summed across every child in that root's tree as each one
	// completes (see finalizeTurn's three notify-building branches, the
	// single point every terminal child outcome already passes through)
	// — never re-derived by re-summing every node on each check,
	// mirroring runningByRoot's own incremental-accumulator shape
	// exactly.
	//
	// Deliberately PROCESS-MEMORY ONLY, never persisted — a live review
	// note, not a bug: SetMaxTreeTokens is a per-process budget by
	// design (this whole map, like runningByRoot, is rebuilt from
	// scratch on every process start), so a tree that spends most of a
	// large budget across children that are later Reaped, then restarts,
	// comes back with an empty usageByRoot — those reaped-and-never-
	// touched-again children contribute nothing, and Spawn's own
	// treeTokenTotal(u) >= m.maxTreeTokens gate under-enforces the
	// operator's real ceiling after a respawn/restart. Out of scope for
	// v1: the whole tree is process-memory (see this struct's own
	// design), and restart semantics already deliver a lost-to-restart
	// notification for any child genuinely interrupted by the crash — a
	// durable, cross-restart budget would need its own design (a
	// separate durable counter, reconciled against Reap/eviction) this
	// PR does not attempt.
	usageByRoot map[string]provider.Usage
	// budgetedByChild is usageByRoot's per-CHILD counterpart: how much of
	// n.session.Usage() THIS manager has already folded into usageByRoot
	// for child session id, keyed by id rather than living only on the
	// sessionNode (see sessionNode.budgetedUsage's own doc comment for
	// the base problem this solves within one node's lifetime). Needed
	// because a child's sessionNode does not survive a Reap — but
	// usageByRoot[rootID] does — so a later same-manager re-adopt of the
	// SAME child id (adoptLocked builds a brand-new sessionNode) must
	// still know how much of that child's spend THIS manager already
	// credited, or its next finalizeTurn would re-add the full amount on
	// top of what usageByRoot already carries across the reap.
	//
	// A live review finding caught this the hard way: an earlier version
	// seeded sessionNode.budgetedUsage from n.session.Usage() directly at
	// adoptLocked time instead of tracking credit here — which fixed the
	// same-manager reap+re-adopt case but broke the DIFFERENT case
	// TestReloadedChildWithDanglingTurnFoldsUsageIntoTreeBudget covers: a
	// process restart's brand-new SessionManager (a fresh, empty
	// usageByRoot) reloading a child whose session.Usage() already
	// carries substantial history from a PRIOR process. That fresh
	// manager has credited nothing yet, so session.Usage() is exactly
	// the right amount to fold in — but seeding budgetedUsage to
	// session.Usage() unconditionally made recoverInterruptedTurnLocked
	// compute a zero delta (total - budgetedUsage, both equal to the
	// same session.Usage() value), silently dropping the child's entire
	// real spend from the budget instead of double-counting it. Keying
	// credit on THIS manager's own map, not the session's own portable
	// cumulative total, is what tells the two cases apart: absent here
	// (the zero value) correctly means "this manager has never credited
	// this child," true for both a genuinely fresh child AND a
	// cross-process reload, and only ever becomes non-zero once
	// finalizeTurn/recoverInterruptedTurnLocked has actually run under
	// THIS manager.
	//
	// Deliberately never pruned per-child (mirrors usageByRoot/
	// runningByRoot's own "only ever cleared for a forgotten ROOT"
	// shape, not a per-child one) — a bounded, modest per-process cost
	// proportional to total children ever finalized in this process's
	// life, accepted for the same reason ForgetRoot's own root-level
	// cleanup was judged sufficient rather than tracking every child
	// individually.
	budgetedByChild map[string]provider.Usage

	// pendingPersist queues durable-write thunks registered via
	// deferPersist while m.mu is held, drained and run by
	// unlockAndFlushPersist once m.mu is released — see that method's
	// own doc comment for the full mechanism and the live review finding
	// it closes. Guarded by m.mu itself (only ever appended to or
	// drained while m.mu is held); never read or written any other way.
	pendingPersist []func()

	// testSweepUnlockedHook, if non-nil, is called by
	// recoverCrashedChildrenLocked exactly once m.mu has been released
	// for its own disk-bound LoadSession replay (see that method's own
	// doc comment) — a test-only synchronization seam, nil in
	// production, mirroring server.Server's own identical
	// queueDispatchRace hook. Lets a test deterministically land a
	// concurrent operation (another adoption, a Reap, a Send) inside the
	// exact window this method's own revalidation-on-reacquire logic
	// exists to handle correctly, rather than relying on incidental
	// goroutine-scheduling luck.
	testSweepUnlockedHook func()

	// testFlushDoneHook, if non-nil, is called by unlockAndFlushPersist
	// immediately after every one of its queued durable-write thunks has
	// actually run (see that method's own doc comment for the unlock-
	// THEN-flush ordering this exists to let a test observe) — a
	// test-only synchronization seam, nil in production, mirroring
	// testSweepUnlockedHook's identical shape just above.
	//
	// Exists because a status transition becoming visible to another
	// goroutine (via Info/Session, which only need m.mu) is NOT proof
	// that an associated durable write queued in the SAME critical
	// section — a forwarded task notification's persistQueuedTaskNotification,
	// notably — has completed: m.mu is released BEFORE the queued thunks
	// run (see unlockAndFlushPersist's own doc comment for why that
	// order, not the reverse, is required), so there is a real window
	// where a concurrent Info() poll observes e.g. StatusDone while the
	// notification it implies was delivered is still only in memory on
	// the target ancestor, not yet on disk. A live test (TestRecoverInterrupted
	// TurnForwardsGrandchildNotifications) polling status as a durability
	// proxy hit exactly this window under load — reworked to block on
	// this hook and re-check the real on-disk condition instead of
	// inferring it from status.
	testFlushDoneHook func()

	// testResumeClaimedHook, if non-nil, is called by fireIdleResumeAsync
	// with targetID the INSTANT it claims that target's run slot (right
	// after triggerResumeLocked returns, still under m.mu) — a test-only
	// synchronization seam, nil in production, mirroring
	// testFlushDoneHook's identical shape just above.
	//
	// Exists because "target's pending notifications read back as a
	// stable count with nothing in flight" is NOT by itself proof that a
	// triggered active resume has run to completion — it is equally true
	// the INSTANT after delivery, before fireIdleResumeAsync's spawned
	// goroutine has even been scheduled to run at all. Both states are
	// observationally identical (same count, same StatusIdle) from
	// outside; only the transition THROUGH StatusRunning in between
	// distinguishes "not yet started" from "already finished". A test
	// that cannot reliably observe that transition (the goroutine may run
	// to completion before the poller's own next scheduler slice) has no
	// way to tell them apart by polling status/counts alone — this hook
	// gives it an explicit, unmissable signal for "the resume has
	// definitely been claimed", so a subsequent wait for the SAME
	// target's status to read Idle again is then unambiguous.
	testResumeClaimedHook func(targetID string)
}

// Changed returns a channel that closes the next time any node's
// observable state settles — a status transition, a finalized flag, or a
// node entering or leaving the tree. A caller re-reads the state it cares
// about (Info, Reap, List) after the channel closes, and blocks on a fresh
// Changed if that state has not settled the way it wants yet.
//
// It is the observation seam for "wait until this node's status settles".
// Every such transition runs markChangedLocked under m.mu before the lock
// is released, so a waiter that arms Changed BEFORE its own read can never
// miss one: either the read already sees the new state, or the wakeup is
// still pending. Waking on a transition the waiter does not care about is
// harmless, because its own re-read is the authority, never the wakeup.
//
// Without it the only way to observe a settle from outside is to sample
// Info on an interval and guess how long to sleep between samples. That is
// a guessed deadline: it flakes under load, and it turns a real hang into
// a slow pass. Notification costs one bool test per transition until the
// first Changed call arms it, and nothing at all before that.
func (m *SessionManager) Changed() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock() // deliberately NOT markChangedLocked: arming an
	// observer is not a state change, and self-waking every waiter here
	// would turn the wait into a spin.
	m.watchChanges = true
	if m.changed == nil {
		m.changed = make(chan struct{})
	}
	return m.changed
}

// markChangedLocked wakes every Changed observer. Call it under m.mu,
// immediately after a node's status, finalized flag, or membership in
// m.nodes changes — never on a pure read, which would make a waiter's own
// re-read wake itself in a loop.
//
// It is a no-op until some caller has actually called Changed, so a
// production process — which never observes this seam — allocates nothing.
func (m *SessionManager) markChangedLocked() {
	if !m.watchChanges || m.changed == nil {
		return
	}
	close(m.changed)
	m.changed = make(chan struct{})
}

// deferPersist queues fn to run once m.mu is released via
// unlockAndFlushPersist — see that method's own doc comment. Caller
// holds m.mu. fn itself must not touch m or take m.mu (it runs AFTER
// m.mu is released, from unlockAndFlushPersist's own goroutine, never
// concurrently with anything else queued in the SAME flush — see that
// method's own doc comment for why queue order is preserved).
func (m *SessionManager) deferPersist(fn func()) {
	m.pendingPersist = append(m.pendingPersist, fn)
}

// deferQueueRecordFlush queues the flush half of the parked-record
// sequence both m.mu-held prompt-queue mutations use: SendToDescendant's
// running-target enqueue and finalizeTurn's queued-message re-drive. Each
// site parks its own record under the same s.mu hold as its memory
// mutation (queueRecordDeferredLocked, queue.go), then calls this so the
// write lands after m.mu releases.
//
// One helper, not two hand-maintained copies — a review finding. The
// ordering this sequence protects is the whole point of parking a record
// on the session (see queueRecordDeferredLocked's own doc comment for the
// double-delivery defect a closure-held record caused), so the flush must
// not be re-derived per call site. Caller holds m.mu.
func (m *SessionManager) deferQueueRecordFlush(s *Session) {
	m.deferPersist(func() {
		s.mu.Lock()
		s.flushQueueRecordsLocked()
		s.mu.Unlock()
	})
}

// unlockAndFlushPersist is the m.mu.Unlock() every SessionManager entry
// point that might have queued a durable write via deferPersist must use
// instead of a plain m.mu.Unlock() — a live review finding: session-log
// disk writes (task-notification queued/delivered records, the
// task-spawn audit record) used to run WHILE m.mu — the single lock
// guarding every session in the tree, taken by Info/Reap/Spawn/Send/
// finalize alike — was held, on finalizeTurn/Spawn/recoverInterruptedTurnLocked's
// own hot paths. A slow or contended disk on ONE session's notification
// could stall every OTHER session's own Info/Reap/Spawn/finalize call in
// the same process, in tension with AGENTS.md's "a hung component can't
// wedge other sessions."
//
// Drains m.pendingPersist into a local slice while STILL holding m.mu
// (cheap — moving a few closure pointers, no I/O), unlocks, THEN runs
// each thunk — so every actual disk write happens entirely outside the
// critical section, in this SAME goroutine, in the exact order the
// thunks were queued. Order and non-interleaving are preserved for free:
// nothing else can drive a second turn (and therefore queue a competing
// write) against any of the SAME sessions these thunks touch until
// EACH session's own node bookkeeping — already fully applied to
// m.nodes before this unlock runs — says it is free to; two DIFFERENT
// SessionManager entry points queuing writes for the SAME session
// concurrently is exactly what m.mu already serialized before this
// change, and still does, for everything except the disk write itself.
//
// A plain m.mu.Unlock() on a path that CAN reach deferPersist would
// silently drop every queued write — always use this instead. Safe (and
// cheap: an empty-slice no-op) to use as the standard unlock helper on
// any SessionManager method, whether or not that specific call path
// happens to queue anything.
//
// Observability caveat this unlock-then-flush order creates: m.mu releases
// BEFORE the thunks run, so a status/state change this same critical
// section already applied to m.nodes (e.g. finalizeTurn setting a child
// StatusDone) becomes visible to any OTHER goroutine's Info()/Session()
// call strictly BEFORE the durable write(s) that same critical section
// queued — a forwarded task notification's persistQueuedTaskNotification,
// notably — have actually landed on disk. A caller must never treat a
// status transition as proof that an associated durable write has
// completed; only that it has been memory-committed and QUEUED to run,
// in order, on this same goroutine, momentarily. testFlushDoneHook (see
// its own doc comment) is the test-only seam for a caller that genuinely
// needs to wait for the flush itself, not just the status.
func (m *SessionManager) unlockAndFlushPersist() {
	pending := m.pendingPersist
	m.pendingPersist = nil
	m.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
	if m.testFlushDoneHook != nil {
		m.testFlushDoneHook()
	}
}

// commitOutcomeLocked records n as s's authoritative committed turn
// outcome (in memory, immediately) and queues its durable write to run
// after m.mu releases — the ONE helper both finalizeTurn and
// recoverInterruptedTurnLocked call, in exactly the same position in
// their own deferPersist sequence (BEFORE attempting delivery), so a
// crash anywhere after this point always leaves a later recovery attempt
// with the identical payload to replay — see Session.committedOutcome's
// own doc comment and the crash-window table on
// recoverInterruptedTurnLocked's own doc comment for the full mechanism.
// Caller holds m.mu.
func (m *SessionManager) commitOutcomeLocked(s *Session, n taskNotification) {
	s.commitTurnOutcome(n)
	m.deferPersist(func() { s.persistCommittedTurnOutcome(n) })
}

// SetExternalRunner installs runner as described on the ExternalRunner
// type — nil restores the default (m drives every turn itself). Safe to
// call at any time; takes effect on the next resume decision.
func (m *SessionManager) SetExternalRunner(runner ExternalRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.externalRunner = runner
}

// SetMaxTreeTokens installs n as the opt-in per-tree token budget — see
// ErrBudgetExceeded's own doc comment. n <= 0 disables the check (the
// default: never called). Safe to call at any time; takes effect on the
// next Spawn call. Deliberately a setter, not a NewSessionManager
// constructor parameter: maxDepth/maxConcurrent are positional
// constructor args because every caller has ALWAYS had to pick a value
// for them (or accept the product default) since this type's very first
// version; threading a new required positional parameter through would
// break every existing NewSessionManager call site across this repo
// (dozens, in tests alone) for a feature every one of them is opting
// OUT of by default. Mirrors SetExternalRunner's identical
// setter-not-constructor-arg reasoning for the same kind of
// genuinely-optional, off-by-default capability.
func (m *SessionManager) SetMaxTreeTokens(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxTreeTokens = n
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

	// finalized reports whether n's concurrency-slot bookkeeping is
	// fully settled: for a depth>0 node, whether decrementRunningLocked
	// has run (or will never need to, because n was never running at
	// all when it went terminal). Zero value false is correct for every
	// freshly running node. Set true by finalizeTurn (which always runs
	// decrementRunningLocked first) for done/failed/already-canceled,
	// and directly by cancelSubtreeLocked for a node canceled while NOT
	// running (idle — no in-flight Prompt call, so nothing will ever
	// call finalizeTurn for it; there is no slot to wait for). Left
	// FALSE by cancelSubtreeLocked for a node canceled WHILE running:
	// its Prompt goroutine is still unwinding and will call finalizeTurn
	// itself once the canceled context actually returns, which is what
	// eventually flips this to true. Reap must never remove a
	// StatusCanceled node with finalized == false: doing so would delete
	// the node before that eventual finalizeTurn call can find it,
	// silently leaking its runningByRoot reservation forever (finalizeTurn
	// is a no-op for an id no longer in m.nodes) — a live review caught
	// this exact race between a slow-unwinding canceled turn and the
	// periodic Reap ticker.
	finalized bool

	// budgetedUsage is the portion of n.session.Usage() (CUMULATIVE
	// across all of n's turns — see that method's own doc comment)
	// already folded into its root's usageByRoot total, as of the last
	// finalizeTurn call for n. Required because a child is NOT
	// single-turn: session.send can restart an already-done/failed
	// child for a legitimate follow-up (SessionManager.Send), and
	// finalizeTurn runs again on ITS completion too — without tracking
	// what was already counted, each subsequent finalizeTurn call would
	// re-add n's FULL cumulative usage (not just the new turn's delta),
	// double- (or triple-, quadruple-...) counting every prior turn on
	// every follow-up. finalizeTurn adds exactly
	// n.session.Usage()-budgetedUsage each time, then updates this to
	// match.
	//
	// adoptLocked seeds this to n.session.Usage() at construction, NOT
	// its zero value — a live review finding: usageByRoot[rootID]
	// SURVIVES a child being reaped (Reap only clears usageByRoot for a
	// root-shaped node, session_manager.go's own Reap doc comment), but
	// a reaped-then-re-adopted child (Send-ing a follow-up to a
	// done/failed child Reap already collected, or a cold LoadSession
	// reload) always gets a BRAND NEW sessionNode via adoptLocked. If
	// that new node started at budgetedUsage's zero value while
	// n.session.Usage() already carries the CUMULATIVE total from the
	// child's prior life (a warm *Session object's own running total, or
	// LoadSession's replay-summed total — either way, already-spent
	// tokens this same child's earlier finalizeTurn call already folded
	// into usageByRoot once), the next finalizeTurn's delta computation
	// would re-add that already-counted prior total on top of the
	// survived usageByRoot entry — double-counting it a second time and
	// letting a later Spawn hit ErrBudgetExceeded well below the real
	// SetMaxTreeTokens ceiling.
	budgetedUsage provider.Usage

	// pendingForget marks a "root-shaped" node (parentID == "") that is
	// NOT actually a protected root in the sense Reap's own doc comment
	// means — "the tree's own address" a caller may still want to
	// reload. It is set in exactly two places, both live review
	// findings on the first version of ForgetRoot/recoverInterrupted-
	// TurnLocked:
	//
	//  1. ForgetRoot, when called on a genuine root that still has live
	//     children: Cancel (already run by the caller — see
	//     endSubagentLineage, server/handlers.go) leaves those children
	//     in m.nodes, canceled, until Reap's own bottom-up sweep
	//     collects them one generation at a time; ForgetRoot itself
	//     correctly refuses to remove their parent's address out from
	//     under that still-in-flight cleanup. Without this flag, NOTHING
	//     ever revisits this root once it finally goes childless — Reap
	//     unconditionally skips every parentID == "" node — leaking it
	//     for the rest of the process's life despite the caller having
	//     explicitly asked to forget it.
	//  2. recoverInterruptedTurnLocked, for an interrupted child whose
	//     OWN parent could not be found tracked (adoptReloadedLocked's
	//     "true depth is unrecoverable" case — attachTo == ""): this
	//     node ends up parentID == "" purely as a bookkeeping side
	//     effect, not because it is a real root anyone might reload by
	//     id, and there is provably no live ancestor left to ever
	//     deliver its notification to (nearestLiveAncestorLocked already
	//     returned nil). Leaving it un-reapable would leak a Failed
	//     pseudo-root forever.
	//
	// Reap's own eligibility check treats pendingForget as the ONE
	// exception to "a root is never reaped" — see its doc comment.
	pendingForget bool
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
		baseCtx:         baseCtx,
		maxDepth:        maxDepth,
		maxConcurrent:   maxConcurrent,
		nodes:           make(map[string]*sessionNode),
		runningByRoot:   make(map[string]int),
		usageByRoot:     make(map[string]provider.Usage),
		budgetedByChild: make(map[string]provider.Usage),
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
//
// Use this ONLY when s is KNOWN to be a genuine root — freshly created
// with no parent_id (handleCreate's ordinary path), or a bare CLI
// session at process start. For a session cold-loaded by id where that
// is NOT known (a caller resolving an arbitrary, possibly-a-child id —
// see handleSpawnChild's parent lookup fallback), use AdoptReloaded
// instead: adopting a reaped or restart-forgotten CHILD as a root here
// would silently discard its true depth and hand it back an unrestricted
// `task` tool, a live-reproduced depth-limit bypass. See
// adoptReloadedLocked's doc comment.
func (m *SessionManager) AdoptRoot(s *Session) error {
	m.mu.Lock()
	// unlockAndFlushPersist, not a plain m.mu.Unlock() — see that
	// method's own doc comment for the convention. adoptRootLocked now
	// calls recoverCrashedChildrenLocked, which can genuinely queue
	// deferred persists (recovering a crashed child durably commits its
	// outcome, delivers, and settles it — all via m.deferPersist) — a
	// plain Unlock() here would silently drop every one of those writes
	// for the one caller (handleCreate) that reaches this method. Today
	// that caller only ever adopts a brand-new, childless session, so
	// the sweep is a harmless no-op in production as of this writing —
	// but AdoptRoot is public API, and "safe by coincidence of the one
	// current caller" is exactly the trap this convention exists to
	// close before a future caller (or a test) adopts a root that DOES
	// already have spawned children on disk.
	defer m.unlockAndFlushPersist()
	if _, exists := m.nodes[s.ID]; exists {
		return fmt.Errorf("engine: session %s already managed", s.ID)
	}
	m.adoptRootLocked(s)
	return nil
}

// AdoptReloaded registers an already-constructed session under lifecycle
// tracking like AdoptRoot, but for a session whose id is resolved from a
// caller-supplied string rather than known in advance to be a root — see
// adoptReloadedLocked, which this wraps with the same already-managed
// guard AdoptRoot uses. It is an error to adopt a session whose id is
// already managed by m.
func (m *SessionManager) AdoptReloaded(s *Session) error {
	m.mu.Lock()
	defer m.unlockAndFlushPersist()
	if _, exists := m.nodes[s.ID]; exists {
		return fmt.Errorf("engine: session %s already managed", s.ID)
	}
	m.adoptReloadedLocked(s, true)
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
	// See recoverCrashedChildrenLocked's own doc comment: a root is the
	// single most common node ANY caller (a box's own restart, a plain
	// GET-triggered ReportTurnStart) adopts fresh, so this is the
	// primary place a crashed subtree actually gets discovered again.
	m.recoverCrashedChildrenLocked(n)
	return n
}

// adoptReloadedLocked registers s as a node the first time THIS
// SessionManager sees it via a caller-resolved id — a cold reload after
// eviction, a process restart, or a legitimate follow-up turn on a child
// whose own tree entry Reap() already removed (Reap only ever removes a
// terminal, momentarily-childless LEAF — a shape a child hits the instant
// its first turn finishes, long before anyone sends it a follow-up;
// session.send's own doc comment explicitly permits messaging a
// done/failed child). s.TaskParentID() — SessionManager's OWN durably
// persisted tree-lineage pointer, set only by Spawn, a COMPLETELY
// DIFFERENT concept from the unrelated, unvalidated s.ParentSession()
// (see Config.TaskParentID's doc comment) — is the only signal available
// to tell a reloaded child apart from a genuine root:
//
//   - Empty: s has no recorded task-tree parent — a genuine root, or a
//     session that predates this field. Adopted as a fresh root (depth
//     0, task tool installed), identical to adoptRootLocked.
//
//   - Non-empty and that parent id is STILL a tracked node: s WAS a
//     child SessionManager had forgotten. Re-attached at its TRUE depth
//     (parent's depth + 1) under its true parent, restoring exactly the
//     lineage Reap or a restart discarded — including the depth-based
//     `task` tool restriction that lineage implies. An earlier revision
//     of this method (via adoptRootLocked's own adopt-on-first-sight)
//     re-adopted a reaped child as a depth-0 ROOT here instead — a
//     live-reproduced depth-limit bypass: the child regained an
//     unrestricted `task` tool and used it to spawn further children
//     with no depth ceiling at all, silently escaping the tree entirely.
//
//   - Non-empty but that parent id is NOT tracked (also forgotten, or a
//     fresh process with no in-memory tree at all — the "task tool
//     broken after restart" gap ReportTurnStart's adopt-on-first-sight
//     otherwise closes): the true depth is unrecoverable. Adopted at
//     m.maxDepth — the one depth value TaskToolAllowed always refuses —
//     rather than depth 0, which would reopen the exact same bypass with
//     no way to rule it out. The cost (a legitimately shallow child
//     loses its own ability to spawn further children after a full
//     process restart with no surviving tree) is deliberate: refusing an
//     unverifiable case is always safer than guessing permissively.
//
// recover gates whether recoverInterruptedTurnLocked runs for a non-root
// adopt (see that method's own doc comment for what it does). A live
// review finding: ReportTurnStart's adopt-on-first-sight call is
// UNCONDITIONALLY followed, a few lines later in that same function, by
// setting n.status = StatusRunning and n.finalized = false to actually
// drive a fresh turn — so firing recovery here first is self-
// contradicting within one call: it would mark the node StatusFailed,
// append a "lost to restart" message to its own transcript, and (worse)
// durably notify a live ancestor that this child died, mere moments
// before that exact child goes on to run — and very possibly finish
// successfully, or fail for a real, unrelated reason finalizeTurn then
// reports AGAIN. ReportTurnStart passes recover=false for exactly this
// reason: it already knows, unconditionally, that it is about to reuse
// n's current turn, so recovery would never be reporting anything true.
// AdoptReloaded (the public wrapper used by handleSpawnChild's
// parent-lookup fallback and cmd/harness's -resume) still passes
// recover=true, for two different reasons that both avoid this same
// failure mode:
//
//   - cmd/harness -resume: a genuine root (s.TaskParentID() == "") never
//     reaches recovery at all — the early return above skips straight to
//     adoptRootLocked. The one case that DOES reach recovery here (s is
//     a former task-tool child, resumed standalone) can never have a
//     live tracked ancestor either way: sessMgr is a brand-new, empty
//     tree for this one-shot run, so nearestLiveAncestorLocked always
//     returns nil regardless of recover — no false notification is ever
//     possible, only an honest "this turn really was interrupted"
//     message appended to the session's own transcript before it
//     continues, which is simply true.
//   - handleSpawnChild: the caller isn't driving a fresh turn on the
//     PARENT at all — Spawn drives the brand-new CHILD it attaches under
//     that parent, a different node entirely. A live grandparent CAN
//     genuinely be notified "this parent's last turn was lost" here,
//     which is not self-contradicting the way ReportTurnStart's case is
//     (nothing here claims the parent's OWN turn is being resumed) — it
//     is, however, a little confusing in combination with the healthy
//     new child Spawn attaches moments later under that "reported dead"
//     parent. Left as-is, deliberately out of scope for this pass (see
//     PR #146 review discussion): fixing it needs a design decision
//     (should Spawn refuse a StatusFailed parent the way it already
//     refuses StatusCanceled?) this fix does not make.
func (m *SessionManager) adoptReloadedLocked(s *Session, recover bool) *sessionNode {
	// s.hasTaskParent() — the SAME predicate finalizeTurn's own
	// settled-marker/commit-outcome gate uses (session_manager.go's
	// finalizeTurn, and see hasTaskParent's own doc comment) — a live
	// review finding: an earlier version of finalizeTurn re-derived this
	// "is s a non-root tree member" question from the in-memory
	// sessionNode.parentID instead, which disagrees with THIS check for
	// exactly the "true depth is unrecoverable" case below (a node
	// adopted here with attachTo=="" despite a non-empty TaskParentID) —
	// unifying both onto one helper is what makes it impossible for the
	// two ends of a crash window to disagree about which nodes recovery
	// covers.
	if !s.hasTaskParent() {
		return m.adoptRootLocked(s)
	}
	parentID := s.TaskParentID()
	s.cfg.SessionManager = m
	s.tools[taskToolName] = taskTool()
	// depth's default is the REFUSAL sentinel: m.maxDepth, the one value
	// TaskToolAllowed always refuses. See this method's own doc comment
	// for why an unrecoverable depth must never guess permissively. The
	// sentinel applies only when neither better source below does.
	// Preferred, in order:
	//   1. s durably recorded its own TaskDepth at spawn time (see
	//      Config.TaskDepth's own doc comment). This is s's OWN true
	//      depth. It is authoritative regardless of whether the live
	//      parent chain is trustworthy right now, so this check runs
	//      FIRST — even when the parent IS currently tracked. A live
	//      review finding: an earlier revision preferred the live
	//      parent.depth+1 whenever the parent was tracked. That silently
	//      propagates the PARENT's own wrong depth forward whenever THAT
	//      parent was itself adopted via case 3 below (its own parent
	//      untracked, no durable TaskDepth of its own — a legacy
	//      grandparent, say). The child then computed m.maxDepth+1,
	//      discarding its own known-correct durable value.
	//      TaskToolAllowed(maxDepth+1) is always false, so the child was
	//      silently denied the task tool even though its true depth was
	//      under the limit. Checking s's own durable value first makes
	//      that impossible — an upstream node's bad depth can never
	//      shadow it.
	//   2. The parent IS currently tracked and s has no durable TaskDepth
	//      of its own — a legacy child predating the field: depth =
	//      p.depth + 1. This is the best available signal. The live
	//      parent chain, however imperfect, beats guessing the sentinel
	//      outright.
	//   3. Neither applies: the parent is not tracked AND s's own depth
	//      was never durably recorded. Use the sentinel. This closes a
	//      live-audited bug: a direct child (true depth 1) whose parent
	//      was not tracked at reload time reported lineage.depth 3 (==
	//      DefaultMaxTaskDepth) — indistinguishable from a session
	//      genuinely refused at the limit. Case 1 above now catches this
	//      before it ever reaches here, for any child with a durably
	//      recorded depth. This case remains, unchanged, for the legacy
	//      fallback it originally existed for.
	depth := m.maxDepth
	attachTo := ""
	p, tracked := m.nodes[parentID]
	if tracked {
		attachTo = parentID
	}
	if d := s.TaskDepth(); d > 0 {
		depth = d
	} else if tracked {
		depth = p.depth + 1
	}
	n := m.adoptLocked(s, attachTo, depth)
	// TaskAgentType survives a reload durably (see its own doc comment)
	// even though SessionManager's in-memory node.agentType does not —
	// without this, lineage.agent_type on the wire went blank the moment
	// a child was reaped and later re-touched, even though it was e.g.
	// still very much an "explore" child.
	n.agentType = s.TaskAgentType()
	m.installTaskToolLocked(s, depth)
	m.restoreTaskToolRestrictionLocked(s, depth)
	if recover {
		m.recoverInterruptedTurnLocked(n, s)
		// Restores n.status/n.result/n.failReason for the case
		// recoverInterruptedTurnLocked's own guard just above does NOT
		// handle: s was ALREADY settled before this specific adoption —
		// see restoreKnownStatusLocked's own doc comment. A no-op,
		// harmlessly, whenever recoverInterruptedTurnLocked just ran to
		// completion for a genuine crash instead (committedOutcome is
		// still whatever IT just set, so restoring from it here is
		// redundant but correct).
		//
		// Gated on recover, deliberately NOT run unconditionally: the
		// recover=false caller (ReportTurnStart's own adopt-on-first-sight,
		// self-contradicting-recovery case — see its own doc comment) is
		// about to drive a FRESH turn on n immediately, and that turn's
		// own eventual finalizeTurn call does not necessarily overwrite
		// EVERY field this method would restore (n.result specifically —
		// finalizeTurn's own nil-msg branch, reachable from
		// runGoal/ReportTurnEnd(id, nil, err), leaves n.result untouched
		// rather than clearing it) — a live regression an earlier version
		// of this change reproduced: a reloaded child's PRIOR turn's real
		// result leaked into a brand-new turn's own nil-msg completion,
		// which should have reported empty.
		m.restoreKnownStatusLocked(n, s)
	}
	m.recoverCrashedChildrenLocked(n)
	return n
}

// restoreKnownStatusLocked restores n.status (and n.result/n.failReason)
// from s's own last committed outcome (Session.committedOutcome,
// engine.go) for a node that is ALREADY settled by the time this specific
// adoption runs — a live prod finding: adoptLocked always constructs a
// freshly-adopted node with the bare StatusIdle default (see its own doc
// comment), and — before this method existed — NOTHING ever corrected
// that default for an already-settled reload; only recoverInterruptedTurnLocked
// did, and only for a node it ACTIVELY recovered from a genuine crash.
// SessionManager.recoverCrashedChildrenLocked's own sweep made this
// suddenly common instead of rare (it now adopts EVERY spawned child, not
// only crashed ones, to reach a crashed descendant more than one level
// down — see its own doc comment) — a wire caller reading Info()/lineage
// for a long-done child right after its ancestor's own adoption would
// otherwise see a flatly wrong "idle" status.
//
// A no-op in exactly one case now: s still has an unfinalized turn (not
// this method's job — recoverInterruptedTurnLocked's own guard already
// covers that, whether or not it actually ran here). "No committed
// outcome at all" used to be a second no-op case too, on the assumption
// that meant either "predates this whole mechanism" or "a genuinely
// fresh node with no terminal turn yet" — both left honestly at
// adoptLocked's StatusIdle default. A live review finding: those two
// cases are NOT actually indistinguishable, and conflating them was a
// real bug. A node with at least one entry in its own SpawnedChildIDs
// PROVES it already ran a turn (Spawn/the task tool is only ever
// callable from WITHIN one) — so a settled-but-committed-outcome-less
// node that HAS spawned children can only be the legacy case, never the
// genuinely-fresh one. Leaving it at StatusIdle was actively harmful:
// nearestLiveAncestorLocked's own walk treats StatusIdle as still LIVE
// (its only terminal cases are Done/Failed/Canceled), so a crashed
// GRANDCHILD recovered underneath this exact node was delivered directly
// onto it instead of correctly reparented past it — and delivery to an
// apparently-idle target also fires fireIdleResumeAsync, spuriously
// re-running a real turn on a node that had already finished, purely to
// relay someone else's notification onward. See
// TestAdoptRootReparentsGrandchildPastSettledIntermediateWithoutCommittedOutcome
// for the reproduction (a bogus THIRD notification from mid's own
// spurious re-run, on top of the two legitimate ones). Fixed by treating
// this sub-case as an honest "unknown outcome" terminal failure instead
// — enough to make nearestLiveAncestorLocked walk past it like any other
// terminal node, without falsely claiming a specific result. A node with
// NO spawned children and no committed outcome is still left at
// StatusIdle: nothing about it proves it is anything other than
// genuinely fresh, and nothing downstream (no descendant to reparent
// past it) depends on its status being corrected.
//
// Callers only ever reach this when recover is true (see the one call
// site, adoptReloadedLocked) — deliberately NOT unconditional: a
// recover=false caller (ReportTurnStart's own adopt-on-first-sight) is
// about to drive a FRESH turn on n immediately, and letting this method
// touch n.result there leaked a PRIOR turn's real result into a brand-new
// turn's own nil-msg completion (finalizeTurn's nil-msg branch leaves
// n.result untouched rather than clearing it) — a live regression an
// earlier version of this change reproduced.
//
// Caller holds m.mu.
func (m *SessionManager) restoreKnownStatusLocked(n *sessionNode, s *Session) {
	if s.hasUnfinalizedTurn() {
		return
	}
	committed, ok := s.committedTurnOutcome()
	switch {
	case ok:
		n.finalized = true
		switch nodeStatusForOutcome(committed) {
		case StatusCanceled:
			// Mirrors cancelOneNodeLocked's own live-path bookkeeping
			// exactly: a canceled node's n.result/n.failReason stay
			// untouched (empty) — the ONLY thing that ever marked this
			// outcome canceled, live, was n.status itself. Restoring
			// committed.FailReason ("canceled", the fixed text
			// finalizeTurn's alreadyCanceled branch puts in the
			// PARENT-facing notification) into n.failReason here would
			// invent a value a live cancellation never actually sets.
			n.status = StatusCanceled
		case StatusDone:
			n.status = StatusDone
			n.result = committed.Result
		default:
			n.status = StatusFailed
			n.failReason = committed.FailReason
		}
		m.markChangedLocked()
	case s.HasHistoryOrSpawnedChildren():
		// No committed outcome, but s has definitely already run a turn
		// — the legacy case, not the genuinely-fresh one. Proven by
		// EITHER signal: a non-empty SpawnedChildIDs (Spawn/the task
		// tool is only ever callable from WITHIN a turn), or — a live
		// review finding this OR-clause itself closes — non-empty
		// History, needed because the FIRST signal alone only proves
		// "definitely not fresh" for a node that happened to spawn
		// something; a legacy CHILDLESS node that ran a real turn and
		// simply never called task leaves SpawnedChildIDs empty too,
		// which — before this fix — fell all the way through to the
		// default case below and was left at adoptLocked's bare
		// StatusIdle forever: not just a wrong status, but a real leak,
		// since Reap only ever collects a FINALIZED, terminal node
		// (StatusDone/Failed/Canceled) — an idle node is never
		// Reap-eligible, so a swept legacy childless settled node used
		// to pin itself in m.nodes permanently, reporting idle forever
		// for a turn that had already long since ended. Every node that
		// spawned anything also necessarily has non-empty History (a
		// turn must already be in progress, with at least a user
		// message and the assistant's own tool call already appended,
		// before Spawn can ever run) — so this OR-clause's second arm is
		// a strict superset of the first; the first is kept explicit
		// anyway as the stronger, more specific proof for a reader.
		//
		// Before giving up and durably marking it an unreconstructable
		// failure, check s's own trailing history for unambiguous
		// evidence of a genuine, natural success — settledSuccessResult
		// (engine.go), the SAME step-2 fallback
		// recoverInterruptedTurnLocked's own crash-window table already
		// uses for the identical "nothing was ever committed for this
		// turn" gap. A live review finding: a legacy node that plainly
		// succeeded (its own last message is a real assistant answer, no
		// dangling tool call) must never be rewritten to StatusFailed
		// just because the newer committedOutcome mechanism postdates
		// it — that is exactly the "successful child durably rewritten
		// as failed" class of bug the analogous :835 fix
		// (nodeStatusForOutcome's Canceled case) already closed for the
		// OTHER direction, and a fail_reason claiming "cannot be
		// reconstructed" would be a straightforward lie the instant the
		// log actually reconstructs it.
		//
		// Only a node whose history genuinely does NOT end in an
		// unambiguous natural success (a trailing tool call still
		// awaiting its result, a non-assistant trailing message) falls
		// through to the honest unknown-outcome failure below — marked
		// terminal specifically so nearestLiveAncestorLocked walks past
		// it like any other terminal node instead of treating it as a
		// live delivery target (and so delivery elsewhere never mistakes
		// it for an idle node worth an async resume), AND so Reap can
		// finally collect it, closing the leak described above.
		n.finalized = true
		if result, ok := s.settledSuccessResult(); ok {
			n.status = StatusDone
			n.result = result
		} else {
			n.status = StatusFailed
			n.failReason = unknownLegacyOutcomeFailReason
		}
		m.markChangedLocked()
	default:
		// Both signals empty: nothing proves this node ever ran a turn
		// at all — genuinely fresh, and adoptLocked's StatusIdle default
		// is already the honest answer (correctly NOT Reap-eligible: a
		// node about to run its first turn must stay pinned). Falls
		// through to the usage fold below regardless (idempotent and
		// safe even if s.Usage() is zero, as it will be here) rather
		// than returning early, so this switch stays the ONLY branch
		// point in this method.
	}
	// Fold this child's already-spent tokens into its root's tree-wide
	// budget total too — the exact same delta-accounting
	// recoverInterruptedTurnLocked/finalizeTurn both do, and for the
	// identical reason (usageByRoot's own doc comment): without this, a
	// long-settled child merely ADOPTED here (never actively recovered,
	// since it was never crashed) would have its real spend silently
	// excluded from the tree budget forever, letting a later Spawn
	// exceed SetMaxTreeTokens by exactly this child's own total. Safe to
	// run every time this method finds a committed outcome, including
	// redundantly on a later re-adoption of the SAME already-credited
	// node: n.budgetedUsage/m.budgetedByChild make the delta zero on any
	// re-run, the same idempotency guarantee those two callers already
	// rely on.
	total := s.Usage()
	delta := provider.Usage{
		InputTokens:      total.InputTokens - n.budgetedUsage.InputTokens,
		OutputTokens:     total.OutputTokens - n.budgetedUsage.OutputTokens,
		CacheReadTokens:  total.CacheReadTokens - n.budgetedUsage.CacheReadTokens,
		CacheWriteTokens: total.CacheWriteTokens - n.budgetedUsage.CacheWriteTokens,
	}
	n.budgetedUsage = total
	m.budgetedByChild[n.id] = total
	u := m.usageByRoot[n.rootID]
	u.InputTokens += delta.InputTokens
	u.OutputTokens += delta.OutputTokens
	u.CacheReadTokens += delta.CacheReadTokens
	u.CacheWriteTokens += delta.CacheWriteTokens
	m.usageByRoot[n.rootID] = u
}

// recoverCrashedChildrenLocked sweeps n's own durably-recorded children
// (Session.SpawnedChildIDs, engine.go) for any whose turn crashed and was
// never recovered, adopting (and thereby recovering) each one found — a
// live prod finding: recoverInterruptedTurnLocked only ever fires
// reactively, on next touch of the CRASHED node's own id (see its own
// "purely reactive" doc section) — a box whose only post-restart traffic
// touches an ANCESTOR never independently touches the crashed child's own
// id, so that trigger never fires. The child's parent then waits forever
// for a notification that was always detectable the moment this parent
// itself was adopted again — the exact sequence a live e2e run on a
// restartPolicy:Always box reproduced: kill -9 mid-child, restart, GET
// the child (cold lineage renders fine — see lineageJSONFor,
// server/handlers.go, unaffected by this — and this GET does NOT reach
// this method either; see below), then the user prompts the root, which
// still had no independent reason to reload the child and so never
// receives a lost-to-restart notification for it.
//
// NOT triggered by a read-only GET, on the child OR the ancestor:
// server.Server.lookup (server/handlers.go), the resolver behind every
// read endpoint (session info, transcript, lineage), returns a resident
// session or a raw s.opts.LoadSession disk read and never calls
// AdoptRoot/AdoptReloaded/ReportTurnStart — so viewing a dead box after a
// restart, on its own, never runs this sweep. What DOES reach it:
// ReportTurnStart, hit the moment a turn actually RUNS on the ancestor —
// this is the live e2e sequence's own second half: the boxes console GETs
// the crashed child first (finds nothing wrong there, cold reads don't
// adopt), then the user prompts the root, which calls ReportTurnStart,
// which adopts and sweeps; and any explicit AdoptRoot/AdoptReloaded call
// (handleCreate's adopt-on-create, handleSpawnChild's parent-lookup
// fallback). A box that is only ever VIEWED after a restart — never
// prompted again on any ancestor — never adopts anything and this sweep
// never runs; the crashed child's notification then sits exactly as
// stranded as it was before this method existed. That is an accepted,
// known trigger boundary of this fix, not a gap it silently claims to
// close.
//
// Called every time n is adopted (adoptRootLocked, and this method's own
// non-root branch above) — n is ALREADY registered in m.nodes by the
// time this runs, so nearestLiveAncestorLocked can find n itself as the
// delivery target for anything recovered here. Recurses naturally:
// adopting a crashed child via adoptReloadedLocked(recover=true) runs
// THIS SAME sweep for that child's own children too, so a whole crashed
// subtree converges from one ancestor touch, not just n's immediate
// children.
//
// Cost/scope note: one LoadSession, AND one m.nodes adoption, per spawned
// child not ALREADY tracked — every child n has EVER spawned, including
// ones settled long ago, since spawnedChildIDs is an append-only audit
// trail with no "already confirmed settled" watermark to narrow the
// sweep, and EVERY child (not only a crashed one) must be adopted for the
// recursion above to reach a crashed descendant more than one level down
// — see the loop body's own comment for why a settled intermediate
// cannot just be skipped. This pins every live descendant in memory
// until the next Reap() call, on the first ancestor touch after a
// restart. Accepted for v1 (a session with a large lifetime child count
// pays a real, one-time-per-process cost the first time it — or an
// ancestor — is touched after a restart); narrowing this (e.g. a
// persisted per-child "already confirmed settled" watermark, so a
// long-done subtree can be skipped without loading it) is a real
// follow-up, deliberately out of scope here.
//
// # Not actually held throughout, despite the name
//
// A live review finding: an earlier version of this method ran every
// LoadSession call (real disk reads — open, stat, read, JSON-decode a
// whole log) while m.mu — the single lock guarding every session in the
// tree, taken by Info/Reap/Spawn/Send/finalize alike — was held, exactly
// the class of problem deferPersist/unlockAndFlushPersist already closed
// for durable WRITES on this same set of call paths (see that
// mechanism's own doc comment). A slow or contended disk while recovering
// a large crashed subtree could stall every OTHER session in the process
// for the whole sweep.
//
// This method now releases m.mu itself partway through — snapshotting
// which of n's spawned children are not yet tracked while STILL holding
// the lock (cheap: no I/O, just a map/slice scan), unlocking, running
// every LoadSession call OUTSIDE the lock, then re-acquiring before
// integrating any result. Safe for every caller to keep treating this as
// an ordinary *Locked method (called with m.mu held, returns with m.mu
// held again) — but the tree is NOT frozen for the method's own
// duration, so integration must explicitly revalidate against whatever
// ran in the gap:
//
//   - n itself may no longer be the live node for its id (reaped, in the
//     one shape that is possible for an already-terminal, already-
//     finalized node a concurrent Reap() call could legitimately collect
//     while unlocked). Checked at the top of EVERY loop iteration below,
//     not merely once up front — a live review finding: this method's
//     own recursion (adoptReloadedLocked, called per candidate, calls
//     this same method again for the child it just adopted) can release
//     and reacquire m.mu again, mid-loop, so a check only before the
//     loop covers candidate #1 but misses n going stale during THAT
//     nested call, before candidate #2 is reached. Wherever this finds n
//     stale, the WHOLE remaining integration is abandoned — attaching a
//     recovered child under a node that has left the tree makes no
//     sense, and whatever concurrent path removed it is authoritative.
//   - Each individual candidate child may have been adopted by someone
//     else in the meantime — another ancestor's own concurrent sweep
//     sharing this same child (a grandchild reachable from two different
//     surviving ancestors after a partial reload), an explicit
//     AdoptReloaded/ReportTurnStart racing this one, or simply a second
//     concurrent touch of the same ancestor. Checked again, per child,
//     right before adopting it: if it is now tracked, this method skips
//     it rather than adopting a second time — adoptLocked has no dedup
//     of its own (a double-adopt would corrupt m.nodes/runningByRoot
//     bookkeeping), and whichever path won the race is treated as
//     authoritative for that child, exactly like the n-itself case
//     above.
//
// Neither race is a correctness gap this method needs to CLOSE, only one
// it must not make worse: the loser of either race simply does less work
// this time around — the winner's own adoption already recovers (or
// otherwise correctly establishes) the child in question, and a FUTURE
// ancestor touch would rediscover anything this specific race genuinely
// caused to be skipped, the same reactive guarantee the whole feature
// already rests on.
func (m *SessionManager) recoverCrashedChildrenLocked(n *sessionNode) {
	// Step 1 (locked): snapshot which spawned children are not yet
	// tracked. No I/O — cheap enough to do without releasing m.mu.
	var candidates []string
	for _, childID := range n.session.SpawnedChildIDs() {
		if _, tracked := m.nodes[childID]; !tracked {
			candidates = append(candidates, childID)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// configSnapshot (not a hand-picked Config{Providers, SessionDir}
	// subset an earlier version of this method used), for the SAME
	// reason Spawn calls it on a live parent when constructing a BRAND
	// NEW child's own Config: a session reloaded here can go on to
	// become the LIVE, turn-driving object for its id — SessionManager.Send
	// (session.send's own sole scheduler for a child, server/session_tree.go's
	// handleSessionSend) reads n.session and calls Prompt on it DIRECTLY,
	// with no reload/re-attach step of any kind (unlike a ROOT, where
	// ReportTurnStart's own "always re-attach to the live object"
	// migration replaces n.session with the server's own fully-configured
	// reload on every turn — see that method's doc comment). A minimal
	// Config{Providers, SessionDir} reload — this method's own earlier
	// version — would silently strand a recovered child with no WorkDir,
	// no OnEvent (its turn would run invisibly, never reaching the
	// server's own SSE journal), no Hooks/MCP/Processes, no Instructions/
	// SkillsDirs/AgentDefsDirs, the moment a caller sent it a genuinely
	// ordinary session.send follow-up. configSnapshot() returns n's OWN
	// full, live Config (safe to read here — see its own doc comment on
	// why it, not a raw s.cfg read, is required under a concurrent
	// SetModel) — every field this reload needs that is NOT itself
	// durably recorded on the child's own log (Model/TaskParentID/
	// TaskAgentType/TaskToolNames/ParentSession all ARE, and LoadSession's
	// own replay correctly overrides whatever this snapshot carries for
	// them with the child's OWN true values) inherited from the live
	// ancestor currently being adopted, exactly like a freshly-Spawn'd
	// child already inherits from its live parent.
	cfg := n.session.configSnapshot()
	nID := n.id

	// Step 2 (unlocked): the actual disk-bound replay.
	m.mu.Unlock()
	if m.testSweepUnlockedHook != nil {
		// Test-only synchronization seam — see its own doc comment
		// (nil, so a no-op, in production).
		m.testSweepUnlockedHook()
	}
	loaded := make(map[string]*Session, len(candidates))
	for _, childID := range candidates {
		childSess, err := LoadSession(cfg, childID)
		if err != nil {
			// Most commonly: this child never survived its own FIRST
			// message append (see store.go's package doc comment —
			// nothing touches disk until then, so a child killed THAT
			// early has no log file at all to load) — an even narrower
			// crash window than this sweep targets, with genuinely
			// nothing durable to recover from. Any other LoadSession
			// failure is equally unrecoverable here: skip this one
			// child rather than aborting the whole sweep over it.
			continue
		}
		loaded[childID] = childSess
	}
	m.mu.Lock()

	// Step 3 (locked again): integrate, revalidating against whatever
	// ran while unlocked — see this method's own doc comment for the
	// exact race semantics decided here.
	//
	// The n-itself-still-live check is re-run at the TOP OF EVERY
	// iteration below, not just once before the loop starts — a live
	// review finding: adoptReloadedLocked's own call (last line of this
	// loop body) recurses into THIS SAME method for the child it just
	// adopted, which can release and reacquire m.mu AGAIN, mid-loop. A
	// single check before the loop only covers candidate #1; n can go
	// stale (reaped — the one shape this method's own doc comment already
	// documents as reachable) during THAT nested call, and a check only
	// before the loop would then let candidate #2 (and any after it) be
	// adopted as a child of a node that has already left the tree.
	for _, childID := range candidates {
		if m.nodes[nID] != n {
			return
		}
		childSess, ok := loaded[childID]
		if !ok {
			continue
		}
		if _, tracked := m.nodes[childID]; tracked {
			continue
		}
		// Adopted unconditionally — NOT gated on childSess.hasUnfinalizedTurn()
		// here, deliberately: adoptReloadedLocked's own non-root branch
		// already re-runs THIS SAME sweep for childSess's own children once
		// it is registered (that is the recursion this method's own doc
		// comment describes), so a SETTLED intermediate child must still
		// be adopted — not just skipped — for a crashed GRANDCHILD beneath
		// it to ever be discovered at all: nearestLiveAncestorLocked needs
		// every ancestor between n and a recovered descendant actually
		// tracked in m.nodes to walk the chain, so a crashed grandchild
		// under an un-adopted (merely "peeked at") settled child would
		// wrongly look like it has no live ancestor to deliver to. recover
		// itself stays true unconditionally too — recoverInterruptedTurnLocked's
		// own top guard (hasUnfinalizedTurn()) already makes that a safe,
		// harmless no-op for a settled child, so there is no reason to
		// duplicate that check here.
		m.adoptReloadedLocked(childSess, true)
	}
}

// recoverInterruptedTurnLocked closes the "in-flight-children restart
// semantics" gap — a follow-up finding, decided and documented here (see
// also docs/plans/2026-08-23-subagent-sessions-design.md's "Process-
// restart recovery" section): every OTHER terminal outcome a child can
// reach (provider failure, tool crash, cancellation, natural completion)
// durably notifies its parent via finalizeTurn — but a child whose turn
// was genuinely IN FLIGHT when the process crashed or was killed has NO
// live goroutine left, in the new process, to ever call finalizeTurn for
// it. Before this fix, such a child cold-reloaded as StatusIdle —
// indistinguishable from a child that simply never received a turn — and
// its parent, if it ever queried or auto-resumed based on this child's
// outcome, waited forever for a notification that could never arrive.
//
// Detection: n was just reconstructed by adoptLocked, so its status is
// still the freshly-adopted default (StatusIdle) — this checks s's own
// durable signature instead (see turnUnsettled's own doc comment,
// engine.go, for the full mechanism: true from the moment a turn starts
// until finalizeTurn — or this very method — explicitly marks it
// settled, regardless of what trailing message shape resulted).
//
// On detection: finalized=true directly, always — nothing is left to
// settle in THIS process (no in-flight goroutine's own finalizeTurn will
// ever run for it, unlike a node this process itself is driving), so it
// is immediately Reap-eligible, exactly mirroring cancelOneNodeLocked's
// identical `!wasRunning -> finalized = true` reasoning for a node with
// no live unwinding goroutine. n.status is USUALLY set StatusFailed — but
// see settledSuccessResult's own doc comment (engine.go) for the one
// unambiguous case this reports StatusDone instead, with the child's real
// result: a crash can strike after a turn genuinely finished but before
// finalizeTurn's own bookkeeping durably landed, and reporting that as a
// failure would be a false, permanent misstatement to the parent, judged
// by a live review as worse than the notification simply being late. A
// notification — synthetic FAILED or reconstructed DONE, whichever this
// turned out to be — is built and delivered through the EXACT SAME path
// finalizeTurn's own cancellation branch uses (nearestLiveAncestorLocked
// + enqueueTaskNotification), so a live ancestor learns about this
// exactly as it would learn about any other terminal child — never a
// second-class delivery mechanism. If that ancestor is currently idle,
// an active resume is fired to wake it — see fireIdleResumeAsync's own
// doc comment for why that happens asynchronously, outside this
// (already-locked) call, rather than being threaded back through
// AdoptReloaded/ReportTurnStart's public signatures.
//
// This IS a purely reactive fix: it only ever fires when something
// ACTUALLY reloads this specific child's id again (a legitimate
// follow-up session.send, ReportTurnStart's adopt-on-first-sight, or
// handleSpawnChild's parent-lookup fallback all already reach here). If
// nothing ever touches the lost child's id again, its parent still waits
// forever — closing THAT fully requires a proactive startup sweep across
// every session on disk, deliberately out of scope for this fix (see the
// design doc section referenced above for why the reactive version is
// the documented, accepted answer for now).
//
// # Crash-window inventory
//
// A live review found that the earlier "deliver first, mark settled
// last" reordering above is necessary but NOT sufficient: it stops a
// notification from being permanently LOST, but a crash landing INSIDE
// either finalizeTurn's OR this method's OWN deliver-then-settle
// sequence could still make a LATER recovery attempt reconstruct a
// DIFFERENT payload than the one already (partially) delivered —
// producing a DIVERGENT duplicate the exact-`==` dedup
// (enqueueTaskNotificationMemoryOnlyDeduped) cannot catch, since it only
// recognizes a byte-identical repeat. Two concrete shapes: a failed
// turn's real classified reason (finalizeTurn) vs. this method's own
// generic "lost to restart" reconstruction; and a genuinely-interrupted
// turn's own synthetic lostToRestartText closer being misread, on a
// SECOND recovery pass, as a spurious natural completion by
// settledSuccessResult().
//
// commitOutcomeLocked/Session.committedOutcome closes this: both
// finalizeTurn and this method persist the EXACT computed notify to the
// child's OWN log, BEFORE attempting delivery — so any later recovery
// attempt that finds one replays it VERBATIM instead of re-deriving a
// possibly-different guess. The table below enumerates every crash point
// across BOTH methods' shared step sequence (steps 1-5 are the terminal-
// completion path both funnel into: build/commit outcome, deliver,
// close, settle) and the durable state — and this method's own resulting
// behavior — a crash at each point leaves behind. "This method" in the
// last column means whichever of finalizeTurn/recoverInterruptedTurnLocked
// runs NEXT for this child, reactively, per this function's own doc
// comment above.
//
//	Step | Durable write (child's own log, unless noted)     | hasUnfinalizedTurn | committedOutcome | Next recovery attempt does
//	-----|-----------------------------------------------------|---------------------|-------------------|---------------------------------------------
//	  0  | (turn's own messages only — recMessage)              | true                | nil               | reconstruct fresh: settledSuccessResult(),
//	     |                                                       |                     |                   | else the generic "lost to restart" fallback
//	     |                                                       |                     |                   | — nothing was ever computed or delivered to
//	     |                                                       |                     |                   | diverge from, so a fresh guess is safe.
//	  1  | + recTaskOutcomeCommitted (commitOutcomeLocked)      | true                | SET               | replay the committed notify verbatim.
//	  2  | + recTaskNotifyQueued (on the ANCESTOR's log)        | true                | SET               | replay the committed notify verbatim — dedup
//	     |                                                       |                     |                   | recognizes the exact match already queued on
//	     |                                                       |                     |                   | the ancestor's log; a fresh delivery if step 2
//	     |                                                       |                     |                   | itself never durably landed.
//	  3  | + recMessage (the synthetic closing message —       | true                | SET (step 3's own | replay the committed notify verbatim; the
//	     |   FAILED outcomes only, see isLostToRestartMarker)   |                     | fold does NOT     | closing message is recognized as already
//	     |                                                       |                     | clear it)         | appended (isLostToRestartMarker) and not
//	     |                                                       |                     |                   | duplicated.
//	  4  | + recChildTurnSettled (markTurnSettled)              | false               | nil (cleared)     | never fires again — the guard at the top of
//	     |                                                       |                     |                   | this method returns immediately.
//
// Step 3 only applies to a FAILED outcome (settled successfully-reported
// turns never append a closing message — see the "Skipped entirely when
// notify.Status == StatusDone" note below). Every step's own durable
// write is queued via m.deferPersist and flushed, in this exact order,
// by unlockAndFlushPersist AFTER m.mu releases — see that mechanism's
// own doc comment for why the write itself never runs while m.mu is
// held, and deferPersist's own doc comment for why FIFO queue order is
// what makes "step N landed before step N+1 could" a meaningful,
// checkable property in the first place.
func (m *SessionManager) recoverInterruptedTurnLocked(n *sessionNode, s *Session) {
	if !s.hasUnfinalizedTurn() {
		return
	}
	n.finalized = true
	m.markChangedLocked()

	// Fold this child's spend into its root's tree-wide budget total —
	// see finalizeTurn's own identical delta-accounting block (and
	// budgetedUsage's own doc comment for why a delta, not the full
	// cumulative total, is required) for the full reasoning; duplicated
	// here rather than shared because the two call sites' surrounding
	// control flow differs enough (this one returns early above on the
	// common "nothing to recover" case) that factoring out a shared
	// helper would cost more clarity than it saves for four lines. A
	// live review finding: an interrupted child's spend used to escape
	// the tree budget entirely, since only finalizeTurn (never this
	// method) touched usageByRoot — a child that spent real tokens
	// before crashing was fully reconstructed here with no accounting
	// for it, letting a later Spawn silently exceed SetMaxTreeTokens.
	//
	// Safe against this whole method re-running for the same n across a
	// crash-and-retry (see the delivery-then-close reordering below):
	// n.budgetedUsage/m.budgetedByChild already provide their own
	// idempotency independent of retry timing — a genuinely fresh
	// process's m.budgetedByChild starts empty regardless (so crediting
	// s's full total, once, in THIS process's own in-memory tracking, is
	// correct and intended), and a same-process re-adopt (Reap then
	// re-touch) always seeds n.budgetedUsage from what THIS manager
	// already credited, making delta zero on the retry. No extra gating
	// needed here beyond what already existed.
	total := s.Usage()
	delta := provider.Usage{
		InputTokens:      total.InputTokens - n.budgetedUsage.InputTokens,
		OutputTokens:     total.OutputTokens - n.budgetedUsage.OutputTokens,
		CacheReadTokens:  total.CacheReadTokens - n.budgetedUsage.CacheReadTokens,
		CacheWriteTokens: total.CacheWriteTokens - n.budgetedUsage.CacheWriteTokens,
	}
	n.budgetedUsage = total
	m.budgetedByChild[n.id] = total
	u := m.usageByRoot[n.rootID]
	u.InputTokens += delta.InputTokens
	u.OutputTokens += delta.OutputTokens
	u.CacheReadTokens += delta.CacheReadTokens
	u.CacheWriteTokens += delta.CacheWriteTokens
	m.usageByRoot[n.rootID] = u

	// Reconstruct — or, when possible, REPLAY — this turn's outcome. See
	// the crash-window table on this method's own doc comment (step 1)
	// for the full reasoning: a prior finalizeTurn run, or an earlier
	// call to this very method, may already have computed the exact
	// outcome for this turn and durably committed it before crashing
	// somewhere in ITS OWN delivery/settle sequence — replaying that
	// verbatim, rather than re-deriving a possibly-DIFFERENT guess, is
	// what makes a retry idempotent-by-content even for a FAILED outcome
	// (whose real classified reason committedTurnOutcome preserves,
	// unlike the generic "lost to restart" fallback below) and for the
	// rarer wedge-shaped SUCCESS settledSuccessResult's own doc comment
	// describes as a residual gap it does not cover.
	//
	// Only when NO committed outcome exists at all (step 0 — nothing was
	// ever computed for this turn before the crash) does this fall back
	// to settledSuccessResult()/the generic failure text, exactly as
	// before.
	var notify taskNotification
	if committed, ok := s.committedTurnOutcome(); ok {
		notify = committed
	} else if settledResult, settledOK := s.settledSuccessResult(); settledOK {
		notify = taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusDone, Result: settledResult}
	} else {
		notify = taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusFailed, FailReason: "lost to restart: turn was in flight when the process last stopped"}
	}
	// Usage: always the freshly recomputed total (the full cumulative
	// spend), not whatever a committed record happened to carry, and not
	// delta — matching finalizeTurn's own three notify-building branches
	// exactly (each sets Usage: n.session.Usage()). delta is the right
	// value to fold into usageByRoot just above (the tree budget only
	// ever wants the NOT-yet-credited portion), but the WRONG value to
	// report to the parent: the parent-facing notification is meant to
	// say how much this child spent in TOTAL, the same number
	// finalizeTurn would report for any other terminally failed child. A
	// live review finding: a Send-restarted child interrupted on its
	// follow-up turn would otherwise under-report its total usage in the
	// parent's [tasks:] line relative to an ordinarily-failed child.
	// Recomputing here rather than trusting a committed record's own
	// Usage field is deliberate, not merely redundant: total is a
	// deterministic function of s.history, which cannot have changed
	// between when that record was written and now (this turn is still
	// unsettled, so nothing new has been appended to it since) — the two
	// are ALWAYS numerically identical, so always computing fresh keeps
	// one single source of truth instead of two that merely happen to
	// agree.
	notify.Usage = total

	// See nodeStatusForOutcome's own doc comment (taskdelivery.go) — a
	// replayed committed outcome (the notify = committed branch above)
	// can legitimately carry Canceled: true (this turn was Cancel()ed,
	// then crashed before finishing its own delivery/settle sequence
	// last time), and collapsing that into StatusFailed here would be
	// the exact same history-rewriting bug restoreKnownStatusLocked had.
	// The settledSuccessResult/generic-fallback branches above never set
	// Canceled, so this is a correctly-scoped no-op for both of those.
	switch nodeStatusForOutcome(notify) {
	case StatusCanceled:
		n.status = StatusCanceled // n.result/n.failReason left untouched — mirrors cancelOneNodeLocked's own live bookkeeping.
	case StatusDone:
		n.status = StatusDone
		n.result = notify.Result
	default:
		n.status = StatusFailed
		n.failReason = notify.FailReason
	}
	m.markChangedLocked()

	// Commit THIS notify durably BEFORE attempting delivery — mirrors
	// finalizeTurn's own identical step (SessionManager.commitOutcomeLocked)
	// exactly, so a crash inside the delivery/settle sequence below
	// leaves a LATER recovery attempt this same replay guarantee (step 1
	// of the crash-window table). A harmless, idempotent re-write on the
	// branch above that already found an existing committed record — by
	// construction, notify already equals it exactly.
	m.commitOutcomeLocked(s, notify)

	target := m.nearestLiveAncestorLocked(n)

	// n may ALSO have been a parent with its own pending notifications —
	// a grandchild that completed and was queued on n while n's turn was
	// still in flight, never checked out before the crash. n is now
	// StatusFailed/finalized and will never run another turn of its own
	// (see SessionStatus's doc comment), so without forwarding these they
	// are stranded on a node that will never read its queue again —
	// mirrors finalizeTurn's own identical "forward a terminal child's
	// pending notifications to the same target its own notify uses"
	// block exactly. A live review finding: an earlier version of this
	// method delivered only notify, silently dropping any grandchild
	// results n itself had not yet forwarded.
	var forwarded []taskNotification
	if s.hasPendingTaskNotifications() {
		forwarded = s.drainAllTaskNotifications() // memory-only — see its own doc comment
	}

	// DELIVER FIRST, mark the turn settled LAST — a live review finding
	// on an earlier version of this method, which did the opposite: the
	// idempotency mechanism at the time (appending a closing message to
	// history, before turnUnsettled/markTurnSettled existed) ran BEFORE
	// delivering notify/forwarded to target. That earlier idempotency
	// signal became false the instant its append durably landed — which
	// is what made a SECOND call to this method for the same n a safe
	// no-op (the guard at the top returns immediately) once recovery had
	// genuinely finished. But the SAME guard is what turned a crash
	// INSIDE this method into total, silent loss: if the process died
	// after that append but before target's own durable write, the next
	// restart's idempotency check already read "settled" — recovery
	// never re-fired, and no recTaskNotifyQueued was ever written on
	// target's log. The parent waits forever for a notification a crash
	// ate in transit — precisely the "waits forever" outcome this whole
	// reactive-recovery feature exists to close, reachable through its
	// own narrow crash window.
	//
	// Reordering so delivery happens first turns that same crash window
	// into a safe RETRY instead of a loss: if the process dies before
	// markTurnSettled runs (see below), hasUnfinalizedTurn() is STILL
	// true on the next restart, and this method runs again for the same
	// child. A naive retry would recompute the identical notify/forwarded
	// values (s's history has not changed) and re-deliver them — a
	// duplicate, not a loss, but still wrong. enqueueTaskNotificationMemoryOnlyDeduped
	// (taskNotification is a plain comparable struct, so == is a real
	// deep-equality check) makes the retry idempotent instead: an exact
	// repeat is recognized and skipped, both in memory and in what gets
	// durably persisted below.
	var toPersist []taskNotification
	var delivered []taskNotification
	if target != nil {
		if target.session.enqueueTaskNotificationMemoryOnlyDeduped(notify) {
			toPersist = append(toPersist, notify)
		}
		for _, fn := range forwarded {
			if target.session.enqueueTaskNotificationMemoryOnlyDeduped(fn) {
				toPersist = append(toPersist, fn)
			}
		}
		// forwarded is only genuinely "delivered" when there was a live
		// target to hand it to — see the else branch below, mirroring
		// finalizeTurn's own identical target!=nil gating around its
		// sibling persistDeliveredTaskNotifications call exactly. A live
		// review finding on an earlier version of this method: it called
		// persistDeliveredTaskNotifications(forwarded) unconditionally,
		// so a target==nil crash-window run (see below) durably marked a
		// GRANDCHILD's notification as recTaskNotifyDelivered even though
		// it was never actually enqueued anywhere — silently and durably
		// LYING that undelivered work was delivered, permanently hiding
		// the drop from anyone auditing the journal afterward.
		if len(forwarded) > 0 {
			delivered = forwarded
		}
	} else {
		// No live ancestor to deliver to — either every ancestor up to
		// the root is already terminal (the whole tree is being torn
		// down), or n.parentID == "" because adoptReloadedLocked could
		// not find ITS OWN parent tracked (the "true depth is
		// unrecoverable" case its own doc comment describes) — n now
		// LOOKS like a root at the tree-bookkeeping level, even though
		// it durably remembers a real TaskParentID. A live review noted
		// this second case is a genuine, accepted degraded outcome for
		// an already-degraded situation (a broken lineage chain AND an
		// interrupted turn): no ancestor is ever told this child died —
		// there IS no reachable ancestor to tell — but n itself does not
		// leak: see Reap's own pendingForget handling, which this method
		// also arms here so a "root-shaped" node with no real subtree
		// beneath it (already true: n is a leaf, just adopted) is
		// collected on the very next Reap() call instead of sitting
		// forever in m.nodes looking like a protected root. forwarded is
		// simply dropped here the same way finalizeTurn drops its own
		// forwarded set when no live ancestor exists: nothing is
		// listening, and there is nothing on target's side to persist.
		n.pendingForget = true
	}

	// Queue the actual durable writes to run AFTER m.mu is released (see
	// SessionManager.deferPersist/unlockAndFlushPersist's own doc
	// comment) — a live review finding: this method runs entirely under
	// m.mu (the single lock guarding every session in the tree), and used
	// to call target.session.enqueueTaskNotification (which persists
	// inline) directly, running disk I/O while that global lock was
	// held. target is captured by this closure, not m.mu — safe to touch
	// after unlock, since target.session's own s.mu is a DIFFERENT lock
	// this closure still correctly acquires internally via
	// persistQueuedTaskNotification.
	if len(toPersist) > 0 {
		targetSess := target.session
		m.deferPersist(func() {
			for _, tn := range toPersist {
				targetSess.persistQueuedTaskNotification(tn)
			}
		})
	}
	if len(delivered) > 0 {
		m.deferPersist(func() { s.persistDeliveredTaskNotifications(delivered) })
	}

	// Close the dangling turn in HISTORY too — readability only now, NOT
	// idempotency (see below): the transcript should show SOMETHING
	// closing the turn a reader (or the parent's own model, via the
	// notification text) can make sense of, not a conversation that
	// silently stops after the last user message with no visible
	// explanation. Its own durable write is queued via m.deferPersist,
	// same as everything else in this method, so it never runs
	// synchronously under m.mu — but its ORDER relative to the delivery
	// thunks above no longer matters for correctness (unlike an earlier
	// version of this method, before turnUnsettled/markTurnSettled
	// existed): idempotency is now markTurnSettled's job below, not
	// this append's.
	//
	// Skipped entirely when notify.Status == StatusDone: the turn was NOT
	// actually interrupted — s's own trailing message already IS the
	// real, natural close of this turn (whether detected fresh via
	// settledSuccessResult, or replayed from an earlier commit) —
	// appending a synthetic "this turn was interrupted" message after a
	// genuine final answer would corrupt an otherwise-clean transcript
	// with a flatly false claim.
	//
	// ALSO skipped when the synthetic closer is already the trailing
	// message (step 3 of the crash-window table: an earlier call to this
	// SAME method already appended it and crashed before settling) —
	// otherwise a recovery-of-recovery retry would append a SECOND
	// "[harness: this turn was interrupted…]" message into history on
	// every retry until the settled marker finally lands.
	//
	// Role: RoleAssistant, not RoleTool — this is a genuine INTERRUPTED
	// MODEL TURN (no tool call to pair a synthetic RoleTool result with,
	// unlike interruptedToolResults' narrower case), so an assistant-role
	// message is what actually closes a turn in this transcript's own
	// vocabulary; the text itself is unambiguous synthetic-marker
	// language, never presented as if the model actually said it.
	//
	// closingText picks canceledInterruptedText over the generic
	// lostToRestartText when notify.Canceled — see that const's own doc
	// comment: this turn's real cause was cancellation, and the closer
	// must say so, not attribute it to the restart that merely
	// interrupted recording that fact.
	closingText := lostToRestartText
	if notify.Canceled {
		closingText = canceledInterruptedText
	}
	if notify.Status != StatusDone && !s.hasTrailingSyntheticCloser() {
		closing := s.appendMemoryOnly(message.Message{
			ID:        newID("msg"),
			Role:      message.RoleAssistant,
			Parts:     message.Parts{&message.Text{Text: closingText}},
			CreatedAt: time.Now().UTC(),
		})
		m.deferPersist(func() { s.persistAppendedMessage(closing) })
	}

	// Mark this turn settled — the actual idempotency mechanism now (see
	// turnUnsettled's own doc comment, engine.go): a LATER, unrelated
	// re-adoption of this same id (Reap removes a StatusFailed leaf just
	// like any other terminal one, and a legitimate follow-up touching
	// this id again re-triggers adoptReloadedLocked) finds
	// hasUnfinalizedTurn() already false and returns at the guard above
	// instead of re-running this whole method and re-enqueueing a
	// duplicate notification. Called AFTER the closing append above (in
	// BOTH the in-memory call order and this method's own deferPersist
	// queue order) — appendMemoryOnly also sets turnUnsettled=true on
	// its own append, so this call's turnUnsettled=false must run
	// strictly after it to be the value that actually sticks.
	s.markTurnSettled()
	m.deferPersist(func() { s.persistTurnSettled() })

	if target != nil && target.status == StatusIdle {
		go m.fireIdleResumeAsync(target.id)
	}
}

// lostToRestartText is the synthetic, clearly-labeled assistant-role
// message recoverInterruptedTurnLocked appends to close a dangling turn
// in HISTORY for readability — see that method's own doc comment.
// Idempotency (recovery must not re-fire for the same settled turn) is
// markTurnSettled's job, not this message's. Used for every interrupted
// turn EXCEPT the one canceledInterruptedText covers — see that const's
// own doc comment for why that one case needs different wording.
const lostToRestartText = "[harness: this turn was interrupted by a process restart and could not complete]"

// canceledInterruptedText is recoverInterruptedTurnLocked's OTHER
// synthetic closer — used instead of lostToRestartText specifically when
// notify.Canceled is true (see that field's own doc comment,
// taskdelivery.go): the turn was explicitly Cancel()ed, and ONLY the
// bookkeeping that records that — commitOutcomeLocked's own durable
// write, delivery to the ancestor, markTurnSettled — was what the
// process restart actually interrupted; the turn itself was not
// "interrupted by a process restart," it was deliberately stopped. A
// live review finding: lostToRestartText's wording, applied
// unconditionally, durably recorded a false cause in the transcript for
// this one case — cancellation demoted to a mere side-effect of a
// restart that had nothing to do with why the turn actually ended.
const canceledInterruptedText = "[harness: this turn was canceled; the process restarted before that could be fully recorded]"

// unknownLegacyOutcomeFailReason is restoreKnownStatusLocked's own
// fail_reason for a node it can prove already ran a turn (a non-empty
// SpawnedChildIDs) but has no committedOutcome to restore the real
// result from — see that method's own doc comment for the full
// mechanism this closes. Deliberately distinct text from
// lostToRestartText: this is NOT a crash-in-flight (hasUnfinalizedTurn()
// is false here — the turn genuinely finished), it is a genuinely
// missing historical record, and a wire caller or a parent's own model
// reading this in a [tasks:] line deserves an accurate distinction
// between the two.
const unknownLegacyOutcomeFailReason = "outcome not recorded: this turn completed before its result could be durably committed, and its real outcome cannot be reconstructed"

// isLostToRestartMarker reports whether m is recoverInterruptedTurnLocked's
// own synthetic closing message (RoleAssistant, exactly lostToRestartText,
// nothing else) — used to distinguish that ONE specific append from a
// genuinely new turn's real message, both live (implicitly — see
// appendMemoryOnly's own doc comment, engine.go) and on replay (see the
// recMessage fold's use of this, store.go). Content-based rather than a
// separate durable flag: lostToRestartText is already an established,
// unambiguous, clearly-labeled synthetic marker elsewhere in this package
// (see its own doc comment) — a real model turn does not coincidentally
// produce this exact string verbatim as its entire response.
func isLostToRestartMarker(m message.Message) bool {
	return m.Role == message.RoleAssistant && m.Parts.Text() == lostToRestartText
}

// isCanceledInterruptedMarker is isLostToRestartMarker's counterpart for
// canceledInterruptedText — see that const's own doc comment for why a
// canceled-then-crashed turn gets different closing wording. Same
// content-based matching rationale as isLostToRestartMarker.
func isCanceledInterruptedMarker(m message.Message) bool {
	return m.Role == message.RoleAssistant && m.Parts.Text() == canceledInterruptedText
}

// isRecoverySyntheticCloser reports whether m is EITHER of
// recoverInterruptedTurnLocked's own synthetic closing messages — never a
// genuine new turn's own real message. Every consumer that needs "is this
// recovery's own synthetic annotation, whichever kind" (the closing-append
// idempotency guard below, the recMessage fold's committedOutcome-
// invalidation exception in store.go, Session.hasTrailingSyntheticCloser)
// calls this ONE function, not either individual check, so a future third
// closer variant only needs to be added here once.
func isRecoverySyntheticCloser(m message.Message) bool {
	return isLostToRestartMarker(m) || isCanceledInterruptedMarker(m)
}

// fireIdleResumeAsync independently re-acquires m.mu and fires a resume
// turn for targetID if it is STILL idle by the time this runs — called
// via `go` from recoverInterruptedTurnLocked, deliberately OUTSIDE that
// call's own already-held lock, rather than threading a resume func()
// back through AdoptReloaded/ReportTurnStart's public signatures (both
// currently return nothing; propagating a resume closure through them
// would touch every one of their several call sites across server/ and
// cmd/harness, each with its own ordering constraints around exactly
// when a returned resume is safe to fire — see finalizeTurn's own doc
// comment on why ONE of ITS callers, runPrompt, has a real ordering
// requirement the others don't). Re-checking target.status here, under a
// FRESH lock acquisition, is required, not defensive: scheduling via `go`
// means arbitrary time may pass before this runs, during which target
// could have started a turn through any other ordinary path.
func (m *SessionManager) fireIdleResumeAsync(targetID string) {
	m.mu.Lock()
	n, ok := m.nodes[targetID]
	if !ok || n.status != StatusIdle {
		m.unlockAndFlushPersist()
		return
	}
	resume := m.triggerResumeLocked(n)
	if m.testResumeClaimedHook != nil {
		m.testResumeClaimedHook(targetID)
	}
	m.unlockAndFlushPersist()
	if resume != nil {
		resume()
	}
}

// restoreTaskToolRestrictionLocked re-applies the tool restriction a
// reloaded child was originally spawned with — see
// Config.TaskAgentType/TaskToolNames's own doc comment for why this is
// necessary at all: LoadSession/newSession reconstruct s.tools from
// scratch via the unconditional full-registry install, with no memory of
// whatever restrictTools narrowed it to at spawn time. Called AFTER
// installTaskToolLocked, so depth-based task-tool removal is already
// applied and this only ever narrows further, never re-adds task.
//
// TaskToolNames present (the common, post-fix case): applied directly
// and exactly — durable ground truth, no re-derivation, no dependency
// on whether the named agent definition still exists.
//
// TaskToolNames absent but TaskAgentType present (a log written between
// this field's introduction and TaskParentID's, or otherwise
// incomplete): best-effort recovery by re-resolving the named
// definition against the CURRENT agent-def set. If it still resolves,
// its CURRENT Tools list is applied (may differ narrowly from what was
// actually used at spawn time if the definition changed since — an
// accepted residual). If it does NOT resolve (the named .agents/*.md
// file was deleted or renamed), fail closed: restrict to the empty set
// — restrictTools([]string{}) cannot itself fail (no names to look
// up), so this is guaranteed to actually take effect, unlike falling
// back to some OTHER named tool that might itself be missing from the
// registry — rather than leaving an ambiguous record unrestricted. A
// live review named this exact case.
//
// Both fields absent: nothing recorded at all (a session predating both
// fields, or a genuinely unrestricted general-purpose child) — no
// change, the same already-accepted "legacy record" gap TaskParentID's
// own doc comment describes.
func (m *SessionManager) restoreTaskToolRestrictionLocked(s *Session, depth int) {
	names, restrict := s.TaskToolNames(), s.TaskToolNames() != nil
	if !restrict && s.TaskAgentType() != "" {
		resolved := false
		if defs, err := s.AgentDefs(); err == nil {
			if def, ok := defs[s.TaskAgentType()]; ok {
				names, restrict, resolved = def.Tools, def.Tools != nil, true
			}
		}
		if !resolved {
			names, restrict = []string{}, true
		}
	}
	if !restrict {
		return
	}
	if !m.TaskToolAllowed(depth) {
		filtered := make([]string, 0, len(names))
		for _, name := range names {
			if name != taskToolName {
				filtered = append(filtered, name)
			}
		}
		names = filtered
	}
	// Best-effort: the only failure mode is a requested name no longer
	// existing in s.tools at all (the registry itself changed shape
	// since spawn — a tool removed from the build). Nothing a caller
	// can act on; the full registry installTaskToolLocked already
	// applied stays in place rather than this call panicking or
	// aborting the reload over it. The guaranteed-safe fail-closed path
	// above (names = []string{}) never hits this.
	_ = restrictTools(s, names)
}

// ReportTurnStart tells m that an EXTERNAL scheduler (the server's own
// run-slot machinery) is about to drive a turn on sess — see
// ExternalRunner's doc comment. If sess is not yet a tracked node at all —
// a session this process is touching for the first time via a cold reload
// from disk (claimForPrompt's transparent LoadSession fallback covers a
// session evicted from residency, one that predates this process entirely
// after a restart, or a child SessionManager's own Reap already
// forgot) — it is adopted here via adoptReloadedLocked, which restores
// its true depth/parent when recoverable rather than always defaulting to
// a fresh root (adoptReloadedLocked's own doc comment covers exactly why:
// a live-reproduced depth-limit bypass an earlier revision of this method
// had, by always adopting as a root here). This is what makes `task` and
// session.info's lineage keep working for a session resumed after a
// restart or eviction, not only one created via THIS process's own POST
// /session (closing the gap a session that already had children before
// the restart would otherwise hit: task calls failing "parent session no
// longer tracked" forever after).
//
// A no-op if sess is already tracked and already running (defensive:
// should not happen if the caller's own admission gate is sound, but
// idempotent rather than corrupting the concurrency count either way).
func (m *SessionManager) ReportTurnStart(sess *Session) {
	m.mu.Lock()
	// unlockAndFlushPersist, not a plain m.mu.Unlock() — see that
	// method's own doc comment for the full convention every entry
	// point in this file that MIGHT queue a durable write via
	// m.deferPersist must follow. adoptReloadedLocked below is called
	// with recover=false, so recoverInterruptedTurnLocked (the only
	// deferPersist source currently reachable from it) never actually
	// runs on this path today — but a plain Unlock() here is a silent-
	// drop trap for any future change that adds one, the same class of
	// finding a live review already caught and fixed for
	// fireIdleResumeAsync.
	defer m.unlockAndFlushPersist()
	n, ok := m.nodes[sess.ID]
	if !ok {
		// recover=false: this function unconditionally sets n.status =
		// StatusRunning and n.finalized = false a few lines below,
		// regardless of what recovery would have set — see
		// adoptReloadedLocked's own doc comment for why firing recovery
		// here would be self-contradicting (report this exact node dead,
		// then immediately run it).
		n = m.adoptReloadedLocked(sess, false)
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
	//
	// Migrate any notification already enqueued on the OLD object before
	// swapping it out — including, critically, the very notification
	// that is driving THIS resume: an idle root can be evicted while a
	// background child is still running (evictResidentLocked only
	// protects a RUNNING session, and SessionManager's own idea of
	// "running" is a different bit than the server's resident-eviction
	// check); the child then finishes, enqueues onto old, and triggers
	// this resume; the resume cold-loads a FRESH object here BEFORE this
	// line runs. Without this migration, old's queue — including that
	// exact notification — is silently orphaned: the fresh object's
	// checkoutTaskNotificationsSegment renders nothing, the resume turn
	// runs with no engine context to act on, and finalizeTurn sees no
	// pending work and settles idle, having "handled" a resume that
	// delivered nothing. A live review caught this exact failure mode
	// too, distinct from (and layered on top of) the object-reattachment
	// fix above.
	if old := n.session; old != nil && old != sess {
		// drainAllTaskNotifications is memory-only (see its own doc
		// comment) — correct here unconditionally, since old and sess
		// are two in-memory objects for the SAME durable session id,
		// sharing the SAME log: nothing new ever needs persisting on
		// this path (the notification's ORIGINAL recTaskNotifyQueued
		// record, written by old's own earlier enqueue, already backs
		// it), and no I/O runs under m.mu regardless.
		//
		// enqueueTaskNotificationMigrated, not enqueueTaskNotification —
		// sess, freshly cold-loaded, already restored via LoadSession's
		// own durable queued-minus-delivered fold anything old could
		// durably have queued BEFORE that load ran; migrating
		// unconditionally here double-delivered exactly that overlap.
		// Dedup keeps this loop still correct for the narrower race it
		// exists to cover (something enqueued on old in the gap between
		// sess's load and this reattachment).
		for _, notif := range old.drainAllTaskNotifications() {
			sess.enqueueTaskNotificationMigrated(notif)
		}
	}
	n.session = sess
	// Balance decrementRunningLocked's unconditional decrement for ANY
	// depth>0 node: increment runningByRoot here too, on an ACTUAL
	// transition into running (guarded by n.status != StatusRunning, so
	// the idempotent already-running no-op case this method documents
	// above never double-counts), mirroring Spawn/Send's own
	// reservation. Only depth>0 — a root never counts (see
	// decrementRunningLocked). Without this, adoptReloadedLocked's
	// depth>0 re-attach (a former child cold-loaded and driven through
	// the ordinary resident-session HTTP path — POST
	// /session/{childID}/prompt_async or /goal — rather than
	// Spawn/Send) would decrement on completion with no matching
	// increment, corrupting the tree-wide concurrency count below the
	// true in-flight total and eventually letting Spawn/Send overrun
	// maxConcurrent. A live review caught this.
	if n.depth > 0 && n.status != StatusRunning {
		m.runningByRoot[n.rootID]++
	}
	n.status = StatusRunning
	// finalized must be cleared on every transition INTO StatusRunning —
	// not just implicitly left at its zero value for a brand-new node —
	// because a node reused for a SECOND (or later) turn (a session.send
	// follow-up to a done/failed child; a reload adopted here) still
	// carries finalized=true from finishing its PRIOR turn. Left
	// uncleared, Reap's !n.finalized guard would wrongly treat THIS
	// turn's still-unsettled concurrency reservation as already safe to
	// remove — see finalizeTurn/cancelOneNodeLocked's own doc comments
	// for what finalized actually tracks. A live review caught this.
	n.finalized = false
	m.markChangedLocked()
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
	// external=true: this turn was driven by the caller's own scheduler,
	// which still holds its run slot and drains any queued prompt itself
	// — see finalizeTurnFrom's own doc comment for the two concrete
	// defects an in-package re-drive caused on this path.
	return m.finalizeTurnFrom(id, msg, err, true)
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
		// See budgetedByChild's own doc comment: seeded from THIS
		// manager's own per-child credit record, not s.Usage() directly —
		// zero (the map's own zero value) for both a genuinely fresh
		// session AND a session this manager has never credited before
		// (a cross-process reload), and the correct already-credited
		// amount for a same-manager reap+re-adopt, where budgetedByChild
		// survived the reap even though the sessionNode itself did not.
		budgetedUsage: m.budgetedByChild[s.ID],
	}
	m.nodes[s.ID] = n
	m.markChangedLocked()
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

// SessionAndInfo is Session and Info's combined form: both the managed
// *Session and its lifecycle snapshot, under ONE m.mu hold. A caller that
// needs both would otherwise call Session then Info separately — two m.mu
// acquisitions, and (a live review finding on Server.lookup, the first
// caller) a correct-today-by-coincidence TOCTOU. Nothing currently reaps a
// node between two such calls: Reap only removes terminal leaves, so a
// RUNNING child's status can never flip to "gone" in that gap. But that
// reasoning had to be re-verified by every future two-call caller rather
// than being structurally impossible. One lock hold here removes the gap
// entirely, for this and any future caller.
func (m *SessionManager) SessionAndInfo(id string) (sess *Session, info SessionNode, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return nil, SessionNode{}, false
	}
	return n.session, n.snapshot(), true
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
		if len(n.children) != 0 {
			continue
		}
		// A "root-shaped" node (parentID == "") is normally NEVER
		// reaped — see this method's own doc comment ("a root is never
		// removed, since it is the tree's own address") — with exactly
		// one exception: pendingForget, armed by ForgetRoot (a caller
		// explicitly asked to forget this root, refused only because it
		// still had live children at the time) or by
		// recoverInterruptedTurnLocked (an interrupted child whose own
		// parent could not be found tracked, provably with no live
		// ancestor left to ever deliver to) — see pendingForget's own
		// doc comment for both cases in full. A live review finding:
		// without this exception, either case leaked the node forever.
		if n.parentID == "" && !n.pendingForget {
			continue
		}
		// !n.finalized excludes a StatusCanceled leaf whose own
		// finalizeTurn hasn't run yet (a still-unwinding canceled
		// Prompt goroutine — see sessionNode.finalized's doc comment):
		// removing it now would delete the node before that eventual
		// call can find it, permanently leaking its runningByRoot
		// reservation. done/failed only ever reach this switch already
		// finalized (finalizeTurn is their sole setter), so this guard
		// is a no-op for them.
		if !n.finalized {
			continue
		}
		switch n.status {
		case StatusDone, StatusFailed, StatusCanceled:
			eligible = append(eligible, id)
		}
	}

	for _, id := range eligible {
		n := m.nodes[id]
		// A canceled node already had its context canceled by
		// cancelSubtreeLocked; a naturally done/failed node never has —
		// nothing in that path calls n.cancel(). Every child ctx is
		// context.WithCancel(parent.ctx), which registers itself in the
		// parent cancelCtx's internal children map; without this call,
		// deleting the node here drops the last REFERENCE to n.cancel
		// without ever invoking it, leaking that registration for the
		// rest of the root's (or the standalone orphan's, for an
		// unrecoverable-parent adoption — see adoptReloadedLocked)
		// lifetime — one leaked cancelCtx per completed task on a
		// long-lived server, defeating part of the reclamation Reap
		// exists to provide. Safe unconditionally: the node is already
		// terminal and its turn long finished, and cancel is idempotent.
		n.cancel()
		delete(m.nodes, id)
		m.markChangedLocked()
		if p, ok := m.nodes[n.parentID]; ok {
			kept := p.children[:0]
			for _, cid := range p.children {
				if cid != id {
					kept = append(kept, cid)
				}
			}
			p.children = kept
		}
		if n.parentID == "" {
			// A pendingForget root-shaped node collected above — mirror
			// ForgetRoot's own identical cleanup (see its doc comment):
			// usageByRoot/runningByRoot are keyed by root id and written
			// to by every turn anywhere in the tree, so deleting only
			// m.nodes would leave one stale entry in each behind per
			// collected root, forever.
			delete(m.usageByRoot, id)
			delete(m.runningByRoot, id)
		}
	}
	return len(eligible)
}

// ForgetRoot removes id from m.nodes, freeing the *sessionNode it pins —
// the ONE, explicit, caller-invoked escape hatch for the leak Reap's own
// doc comment describes as a deliberate v1 scope cut: "a root is never
// removed, since it is the tree's own address and a caller may still hold
// (or later reload) its id." That's still true for the AUTOMATIC sweep
// Reap performs — this does NOT reinterpret Reap's own contract, which
// stays root-blind — but a caller that has independently decided a root
// is truly done (its own DELETE /session/{id}, say) has no such ambiguity
// and needs a way to say so explicitly. Without this, EVERY root a
// process has ever created accumulates in m.nodes (and the *Session it
// pins — full message history, ctx) for the process's entire lifetime,
// even for roots the caller has already deleted at the server-residency
// layer (see server/handlers.go's handleEnd, which tears down s.sessions
// but — before this method existed — had nothing to call on sessMgr for
// the other half of that leak).
//
// Refuses (returns an error, removes nothing) rather than guessing safe
// in three cases: id is not a tracked root at all (ErrUnknownSession, or
// id names a CHILD — a child is never this method's job, Reap already
// handles a terminal leaf child and a non-terminal one has its own
// in-flight turn to protect); id still has live children (removing a
// root out from under them would orphan the whole subtree with no
// address left to reach it by — matches Cancel's own cascade philosophy:
// tear down the subtree first, via Cancel, if that's really the intent);
// id is currently StatusRunning (an in-flight turn still has a goroutine
// that will eventually call finalizeTurn expecting to find this node).
func (m *SessionManager) ForgetRoot(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	if n.parentID != "" {
		return fmt.Errorf("engine: %s is not a root", id)
	}
	if n.status == StatusRunning {
		return fmt.Errorf("engine: root %s is busy", id)
	}
	if len(n.children) != 0 {
		// Arm pendingForget so Reap's own bottom-up sweep — which will
		// eventually collect these still-live (but presumably already
		// canceled, per endSubagentLineage's own cascade-then-forget
		// ordering) children one generation at a time — also collects
		// THIS root, once it finally goes childless, instead of leaking
		// it forever the moment this call returns. A live review finding:
		// without this, nothing ever revisited a root ForgetRoot refused
		// for exactly this reason.
		//
		// Also make n itself terminal + finalized HERE, rather than
		// depending on the caller having ALSO called Cancel first —
		// endSubagentLineage (this method's only current caller) always
		// does, but ForgetRoot's own contract should not silently
		// depend on that: a future direct caller that skips Cancel would
		// otherwise leave n StatusIdle/finalized=false forever, and
		// Reap's eligibility switch only ever matches a terminal status
		// with finalized=true. Confirmed not running just above, so
		// mirroring cancelOneNodeLocked's identical "nothing left to
		// settle" reasoning is safe here.
		if n.status != StatusDone && n.status != StatusFailed && n.status != StatusCanceled {
			n.status = StatusCanceled
		}
		n.finalized = true
		n.pendingForget = true
		m.markChangedLocked()
		return fmt.Errorf("engine: root %s still has live children", id)
	}
	n.cancel() // see Reap's identical call for why this is required, not optional
	delete(m.nodes, id)
	m.markChangedLocked()
	// A live review finding: usageByRoot/runningByRoot are keyed by root
	// id and written to by every turn anywhere in this root's tree (see
	// their own doc comments) — deleting only m.nodes left one stale
	// entry in each behind per forgotten root, forever, on a long-lived
	// process that creates and deletes many roots. Reap's own removal
	// loop does the identical cleanup for a pendingForget root it
	// collects instead of this direct path.
	delete(m.usageByRoot, id)
	delete(m.runningByRoot, id)
	return nil
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
// Depth, concurrency, and (if SetMaxTreeTokens configured one) the
// per-tree token budget are enforced synchronously, under one lock,
// before the child is created: a caller at any limit gets ErrDepthLimit,
// ErrConcurrencyLimit, or ErrBudgetExceeded back and no session to clean
// up, and a race between two Spawn calls at a limit is resolved by
// whichever acquires the lock first — the other sees the reservation (or
// the accumulated usage) and fails cleanly, per the design doc's "a race
// is still answered with an error, not a crash." Unlike depth/
// concurrency, the budget check is a SPAWN-TIME gate only — a tree
// already over budget cannot spawn further children, but nothing here
// interrupts a turn already in flight when the tree crosses the budget
// mid-run.
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
	if m.maxTreeTokens > 0 {
		// All four provider.Usage fields, matching usageByRoot's own
		// accumulation exactly — see ErrBudgetExceeded's own doc comment
		// for why input+output alone under-measured a cache-heavy tree's
		// real spend. treeTokenTotal is the single shared definition of
		// "how many tokens does this tree's usage count as" — used here
		// AND by usageByRoot's own accumulation, so the gate and the
		// accumulator can never drift apart again.
		u := m.usageByRoot[parent.rootID]
		if treeTokenTotal(u) >= m.maxTreeTokens {
			m.mu.Unlock()
			return "", ErrBudgetExceeded
		}
	}

	// configSnapshot (not a raw parent.session.cfg read) is required here:
	// see its doc comment — an unsynchronized cfg read races SetModel's
	// writes under the parent's own lock, and would silently inherit the
	// parent's STALE construction-time model rather than the live one the
	// design doc's inheritance rule actually means.
	childCfg := parent.session.configSnapshot()
	childCfg.ParentSession = parent.id
	// TaskParentID is SEPARATE from ParentSession just above — see its
	// doc comment. This is the durable record adoptReloadedLocked
	// consults to restore this child's true depth/parent if
	// SessionManager's in-memory tree ever forgets it (Reap, or a
	// process restart) before session.send's own "a done/failed CHILD
	// is eligible for Send" contract lets a legitimate follow-up touch
	// it again.
	childCfg.TaskParentID = parent.id
	// Durable depth record — see Config.TaskDepth's own doc comment for why
	// this exists: without it, adoptReloadedLocked's "true depth is
	// unrecoverable" fallback (parent not currently tracked) had nothing
	// but m.maxDepth, a REFUSAL sentinel indistinguishable on the wire from
	// a session genuinely at that depth.
	childCfg.TaskDepth = childDepth
	if !opts.Model.IsZero() {
		childCfg.Model = opts.Model
	}
	if opts.SystemAppend != "" {
		childCfg.System = append(append([]string(nil), childCfg.System...), opts.SystemAppend)
	}
	child := NewSession(childCfg) // installs `task` unconditionally, since childCfg.SessionManager is inherited from the parent

	// Validate opts.ToolNames against the child's OWN full registry —
	// BEFORE installTaskToolLocked below can remove "task" from it (a
	// definition may legitimately name "task" explicitly even at the
	// depth limit; see the depth-filter step further down, which
	// withholds it without erroring) and BEFORE intersecting against the
	// parent's effective set further below. An unknown name here is a
	// genuine agent-definition typo/config mistake (e.g. tools:
	// read_fiel), which must still surface as a clean Spawn error
	// exactly like it always did (restrictTools's own validation, moved
	// earlier): the intersection below would otherwise silently DROP an
	// unknown name identically to a real but parent-doesn't-have-it
	// name, masking the mistake entirely instead of erroring on it.
	for _, name := range opts.ToolNames {
		if _, ok := child.tools[name]; !ok {
			m.mu.Unlock()
			return "", fmt.Errorf("engine: unknown tool %q", name)
		}
	}
	m.installTaskToolLocked(child, childDepth)

	// toolNames is the child's EFFECTIVE tool set: the PARENT's own
	// effective set, intersected with the agent definition's own
	// restriction (opts.ToolNames — nil means "no ADDITIONAL restriction
	// beyond whatever the child would otherwise inherit"). Deriving it
	// from parent.session.tools rather than opts.ToolNames alone closes
	// a privilege-escalation edge a live review caught: a RESTRICTED
	// parent (e.g. a custom def with tools: read_file, task — read-only
	// plus the ability to spawn) spawning a general-purpose child
	// (opts.ToolNames == nil for that built-in) used to hand that child
	// the FULL, unrestricted session-default registry — including
	// bash/write_file the restricted parent itself could never reach.
	// The spec's own table says general-purpose gets "the parent's full
	// tool set," not "the session's" — this is what makes that true.
	parentEffective := make([]string, 0, len(parent.session.tools))
	for name := range parent.session.tools {
		parentEffective = append(parentEffective, name)
	}
	toolNames := parentEffective
	if opts.ToolNames != nil {
		defSet := make(map[string]bool, len(opts.ToolNames))
		for _, name := range opts.ToolNames {
			defSet[name] = true
		}
		narrowed := make([]string, 0, len(opts.ToolNames))
		for _, name := range parentEffective {
			if defSet[name] {
				narrowed = append(narrowed, name)
			}
		}
		toolNames = narrowed
	}
	if !m.TaskToolAllowed(childDepth) {
		// The computed set above may still include "task" (the parent
		// itself has it, and the definition didn't explicitly exclude
		// it), but this child is at the depth limit: withheld
		// unconditionally, exactly like the general-purpose
		// (unrestricted) case always did — never a load-bearing error
		// for hitting a limit that is expected to bite eventually. Depth
		// is absolute and never overridable by inheritance.
		filtered := make([]string, 0, len(toolNames))
		for _, name := range toolNames {
			if name != taskToolName {
				filtered = append(filtered, name)
			}
		}
		toolNames = filtered
	}
	if err := restrictTools(child, toolNames); err != nil {
		m.mu.Unlock()
		return "", err
	}
	// Durable record of the restriction just applied — see
	// Config.TaskAgentType/TaskToolNames's own doc comment for why: a
	// reload (adoptReloadedLocked) has nothing else to restore this
	// child's tool set from once SessionManager's in-memory tree (and
	// the in-memory tools map restrictTools just narrowed) is forgotten
	// — Reap, or a process restart — and a legitimate session.send
	// follow-up touches this child again. child.cfg, not childCfg (the
	// local NewSession already copied by value): safe to write directly,
	// unsynchronized — child was JUST constructed above and has not
	// been exposed to any other goroutine yet (its own Prompt-driving
	// goroutine launches further below), matching adoptRootLocked's
	// identical reasoning for mutating a session's cfg/tools before it
	// is live. The very first Persist call journals this on the header
	// record.
	child.cfg.TaskAgentType = opts.AgentType
	child.cfg.TaskToolNames = toolNames

	n := m.adoptLocked(child, parent.id, childDepth)
	n.agentType = opts.AgentType
	// Durable audit trail: "I spawned this child" — see recTaskSpawned's
	// own doc comment (store.go) for the follow-up this closes ("child
	// journal records"). Written on the PARENT's log, symmetric with
	// commitTaskNotifications' recTaskNotifyDelivered write landing on
	// the SAME log later, for a single-log audit trail of everything
	// this session's `task` tool did.
	//
	// Deferred via m.deferPersist, not written inline here — a live
	// review finding: this whole method runs under m.mu (the single lock
	// guarding every session in the tree), and persistTaskSpawnLocked
	// does real disk I/O (ensureLog's MkdirAll/OpenFile/Stat on a cold
	// log, then writeRecord's append) — see SessionManager.deferPersist/
	// unlockAndFlushPersist's own doc comment for the full mechanism and
	// why holding m.mu across it was a problem. parentSess.mu (a
	// DIFFERENT lock from m.mu) still guards the actual write, exactly
	// as persistTaskSpawnLocked's own doc comment requires — just
	// acquired later, from unlockAndFlushPersist's own goroutine, after
	// m.mu has already been released by this method's own caller.
	parentSess := parent.session
	// Memory-only update now (synchronously, still under m.mu, before
	// this method returns) — see spawnedChildIDs' own doc comment
	// (engine.go) for why this list exists at all
	// (recoverCrashedChildrenLocked's own "which children did I spawn"
	// question). Cheap and disk-free, unlike persistTaskSpawnLocked just
	// below, which is why only THAT call is deferred.
	parentSess.mu.Lock()
	parentSess.recordSpawnedChildLocked(child.ID)
	parentSess.mu.Unlock()
	m.deferPersist(func() {
		parentSess.mu.Lock()
		parentSess.persistTaskSpawnLocked(child.ID, opts.AgentType)
		parentSess.mu.Unlock()
	})
	// Reserve the concurrency slot NOW, synchronously, rather than when the
	// launched goroutine gets around to running — otherwise two Spawn calls
	// racing past the check above could both pass it before either
	// goroutine marks its child running, overrunning maxConcurrent. A
	// spawned child is handed work immediately, so it is never idle before
	// running: skip straight to running instead of the idle adoptLocked
	// sets by default.
	n.status = StatusRunning
	m.markChangedLocked()
	m.runningByRoot[parent.rootID]++
	m.unlockAndFlushPersist()

	go func() {
		msg, perr := drainQueueAndPrompt(n.ctx, child, opts.Prompt)
		if resume := m.finalizeTurn(child.ID, msg, perr); resume != nil {
			go resume()
		}
	}()

	return child.ID, nil
}

// drainQueueAndPrompt runs one Prompt call on s with text, then — for as
// long as s's durable prompt queue is non-empty once that call returns —
// dequeues the next entry (FIFO, one at a time, mirroring the server's
// own post-turn tail dispatch for a root: dispatchQueueHead,
// server/handlers.go) and runs ANOTHER Prompt call with it, repeating
// until the queue is empty. Returns the LAST Prompt call's own (msg, err)
// — the pair the caller's finalizeTurn call should be given, exactly as
// if that had been the only call made.
//
// This is the CHILD-side counterpart to a root's post-turn tail dispatch
// (maybeDispatchQueued, called from runPrompt's own tail once the run
// slot frees): engine.go's Prompt loop only drains the queue AT A
// MID-TURN TOOL-CALL BOUNDARY (DequeueAllPrompts, right after a tool
// result is appended, before the next provider request in the SAME
// turn) — a turn that ends with no further tool-call boundary (its
// final provider response has a non-tool-use stop reason) never reaches
// that drain at all, by that code path's own design (see
// operatorMessagesBlock's doc comment, queue.go). A ROOT session
// tolerates this because the SERVER'S OWN residency layer picks the
// queue back up the instant Prompt returns, as a brand-new turn
// (maybeDispatchQueued). A CHILD, driven directly by Spawn's launched
// goroutine or by Send, has no such external tail — without this,
// SendToDescendant's promise to the model ("queued for delivery at the
// descendant's next turn boundary") would be broken exactly whenever a
// message lands after the child's last mid-turn drain point: the child
// settles done/failed with the message still sitting, undelivered, in
// its own promptQueue, and nothing would ever look at it again — a live
// review finding on this fix's first pass (the original version relied
// on the mid-turn drain alone).
//
// Runs regardless of the prior call's own outcome — including a failed
// one — mirroring maybeDispatchQueued's own unconditional tail-dispatch
// (it fires whether the just-finished turn errored or not): a queued
// follow-up deserves its own attempt rather than being silently
// abandoned because an unrelated earlier turn on the same child failed.
//
// Stops draining, WITHOUT dequeuing anything further, the instant ctx is
// already canceled — checked before the FIRST Prompt call and again at
// the top of every loop iteration, before the next dequeue. Without this, a child canceled (task cancel) WHILE
// this loop is between Prompt calls would keep calling s.DequeuePrompt
// (journaling each item as "delivered") and s.Prompt(ctx, ...) — on an
// already-dead ctx — for every remaining queued entry: a live review
// finding. Any prompt still in the queue at that point is left there,
// UNTOUCHED — never explicitly drained or cleared by this loop — which
// is the right answer, not merely the simplest one: it matches
// cancellation's existing "stop, full stop" semantics elsewhere in this
// package (finalizeTurn's own re-drive re-check is gated on the node NOT
// being StatusCanceled, precisely so a canceled child's queue is never
// looked at again by anyone — see its own doc comment, and
// runTaskSend's queued-path note in task_tool.go, which already
// documents this same outcome from the model-facing side). A canceled
// child's leftover queue simply sits inert until the node itself is
// eventually Reaped — "stays queued," not "discarded" by any explicit
// step.
func drainQueueAndPrompt(ctx context.Context, s *Session, text string) (*message.Message, error) {
	// The FIRST call is guarded too, not just the loop — a review
	// finding: on the finalizeTurn re-drive and settled-relaunch paths a
	// cancel landing between the closure's creation and its `go resume()`
	// left this call issuing one wasted Prompt (appending one user
	// message) on an already-dead ctx. Returning ctx.Err() matches what
	// Prompt itself returns for that ctx, so finalizeTurn classifies the
	// turn exactly as before.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	msg, err := s.Prompt(ctx, text)
	for {
		if ctx.Err() != nil {
			return msg, err
		}
		next, _, ok := s.DequeuePrompt("delivered")
		if !ok {
			return msg, err
		}
		msg, err = s.Prompt(ctx, next.Text)
	}
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
// CanSend reports whether a Send(ctx, id, ...) call right now would pass
// Send's own admission checks, without reserving anything or driving a
// turn — a cheap, synchronous pre-check for a caller (server's
// handleSessionSend) that wants to report a real admission failure
// (ErrSessionBusy/ErrConcurrencyLimit/ErrSessionCanceled/
// ErrUnknownSession) back to its own caller BEFORE committing to an
// asynchronous Send it cannot easily surface a failure from without
// blocking on the whole turn. A small residual race remains between this
// call and the caller's own subsequent Send: CanSend takes and releases
// m.mu immediately, so another concurrent Send/Spawn against the SAME id
// or tree could change the outcome in the gap. That gap is the
// legitimate "benign race window" this package documents elsewhere
// (runOrQueueText, enqueueOrDispatch) — CanSend exists to eliminate the
// FAR more common, entirely non-racy case an earlier revision of
// handleSessionSend's child branch got wrong: discarding Send's real,
// deterministic admission error (the target was ALREADY busy/canceled,
// or the tree was ALREADY at its concurrency cap) behind an
// unconditional 202 "sent".
func (m *SessionManager) CanSend(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	if n.status == StatusCanceled {
		return ErrSessionCanceled
	}
	if n.status == StatusRunning {
		return ErrSessionBusy
	}
	if n.depth > 0 && m.runningByRoot[n.rootID] >= m.maxConcurrent {
		return ErrConcurrencyLimit
	}
	return nil
}

func (m *SessionManager) Send(ctx context.Context, id, text string) (*message.Message, error) {
	m.mu.Lock()
	s, nodeCtx, isChild, err := m.reserveSendLocked(id)
	// unlockAndFlushPersist, matching SendToDescendant's settled branch,
	// which reserves through this same reserveSendLocked call — a review
	// finding on the one plain unlock left after that branch was
	// hardened. reserveSendLocked defers nothing today, so this is
	// uniformity rather than a live fix: the helper is the standard
	// unlock for every SessionManager method (an empty-slice no-op when
	// nothing is queued), so a future deferred write on this path cannot
	// be dropped silently.
	m.unlockAndFlushPersist()
	if err != nil {
		return nil, err
	}

	runCtx, stop := mergeCancel(ctx, nodeCtx)
	defer stop()
	var msg *message.Message
	if isChild {
		msg, err = drainQueueAndPrompt(runCtx, s, text)
	} else {
		msg, err = s.Prompt(runCtx, text)
	}
	if resume := m.finalizeTurn(id, msg, err); resume != nil {
		go resume()
	}
	return msg, err
}

// reserveSendLocked performs Send's own admission checks and slot
// reservation for id, assuming m.mu is ALREADY held by the caller — it
// neither locks nor unlocks m.mu itself. Factored out of Send's own top
// so SendToDescendant's settled-target path (which has already
// re-validated id's existence/ancestry/status under this SAME m.mu hold,
// moments earlier) can reserve the turn in that SAME critical section
// instead of releasing m.mu and having a freshly launched goroutine call
// Send, which re-acquires m.mu from scratch — a live review finding: a
// caller's own periodic Reap() sweep could collect an already-terminal
// leaf in the gap between SendToDescendant's own admission decision and
// that goroutine's Send call actually re-acquiring m.mu, silently
// discarding the resulting ErrUnknownSession and leaving the "dispatched
// as a fresh turn" promise unfulfilled with no trace at all. Reserving
// here, inside the SAME unbroken critical section, closes the window
// entirely rather than narrowing it: once this returns nil, n.status is
// ALREADY StatusRunning, and Reap's own eligibility switch never
// collects a running node — there is nothing left for a concurrent Reap
// to race against.
//
// Returns Send's own four admission errors on refusal (err is the only
// meaningful return then): ErrUnknownSession, ErrSessionCanceled,
// ErrSessionBusy, ErrConcurrencyLimit. On success, reserves the
// concurrency slot and flips n.status to StatusRunning exactly as Send's
// own top always did, and returns the session/context/isChild triple the
// caller needs to actually drive the turn OUTSIDE m.mu — exactly what
// Send itself does immediately after unlocking.
func (m *SessionManager) reserveSendLocked(id string) (s *Session, nodeCtx context.Context, isChild bool, err error) {
	n, ok := m.nodes[id]
	if !ok {
		return nil, nil, false, fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	if n.status == StatusCanceled {
		return nil, nil, false, ErrSessionCanceled
	}
	if n.status == StatusRunning {
		// Refuse rather than proceed: Session.Prompt must never be called
		// concurrently with itself, and Send is the one SessionManager
		// entry point a caller might plausibly invoke twice at once for
		// the same id (see ErrSessionBusy's doc comment) — a bug a live
		// -race run against an earlier version of this method caught,
		// where this check was missing and a second concurrent Send fell
		// through to a second concurrent Prompt call. Unreachable from
		// SendToDescendant's own settled-target call site specifically
		// (it only ever calls this after already confirming, under this
		// SAME unbroken m.mu hold, that status is NOT Running) — kept
		// anyway, for Send's own direct callers, in the same
		// defensive-dead-code spirit classifyTaskVerbError's identical
		// ErrSessionBusy case documents (task_tool.go).
		return nil, nil, false, ErrSessionBusy
	}
	if n.depth > 0 && m.runningByRoot[n.rootID] >= m.maxConcurrent {
		// A done/failed CHILD is eligible for Send (a legitimate follow-up
		// message — see the design doc's session.send), and Send must
		// enforce the SAME tree-wide concurrency budget Spawn does: without
		// this check, enough concurrent session.send calls against
		// already-settled children could push runningByRoot above
		// maxConcurrent, the same overrun Spawn's own check exists to
		// prevent.
		return nil, nil, false, ErrConcurrencyLimit
	}
	n.status = StatusRunning
	// See ReportTurnStart's identical reset for why: n may be a
	// done/failed child Send is legitimately restarting for a follow-up
	// turn, and it still carries finalized=true from finishing its PRIOR
	// one.
	n.finalized = false
	m.markChangedLocked()
	if n.depth > 0 {
		m.runningByRoot[n.rootID]++
	}
	// isChild gates drainQueueAndPrompt to CHILDREN only — see its own
	// doc comment for why a child needs it (no external tail dispatch).
	// A ROOT keeps its original single-Prompt-call behavior unchanged: in
	// bare-CLI/engine usage with no server layered over it (the only case
	// Send ever drives a root's turn directly — see this method's own doc
	// comment), a root's residency/queue semantics are this package's
	// existing, separately-tested contract — not a behavior change for
	// roots nothing asked for.
	return n.session, n.ctx, n.depth > 0, nil
}

// isDescendantLocked reports whether targetID is a STRICT descendant of
// ancestorID: ancestorID appears somewhere in targetID's own parent
// chain, walking m.nodes' live parentID pointers up from targetID.
// Backs the `task` tool's three ancestor-gated verbs (cancel/status/
// send — see the design doc's "only ancestors may cancel their
// descendants," which this generalizes to all three): a spawning session
// may act on anything it spawned, directly or transitively, and nothing
// else.
//
// ancestorID == targetID is deliberately false — acting on yourself is
// not this mechanism's job (self-cancel has no established meaning here;
// self-status already has session_info; self-send is just an ordinary
// prompt) — and a target whose chain does not reach ancestorID, or
// breaks on a node this process no longer tracks (Reaped, or never
// adopted), is false rather than guessed true: an unrelated or
// unreachable lineage is not proof of descent. Caller holds m.mu.
func (m *SessionManager) isDescendantLocked(ancestorID, targetID string) bool {
	if ancestorID == "" || targetID == "" || ancestorID == targetID {
		return false
	}
	n, ok := m.nodes[targetID]
	if !ok {
		return false
	}
	for n.parentID != "" {
		if n.parentID == ancestorID {
			return true
		}
		p, ok := m.nodes[n.parentID]
		if !ok {
			return false
		}
		n = p
	}
	return false
}

// CancelDescendant cancels targetID's entire subtree on callerID's
// behalf, after confirming callerID is a live ancestor of targetID (see
// isDescendantLocked) — the SessionManager side of the `task` tool's
// cancel verb.
//
// Routes to Cancel/cancelSubtreeLocked (cascade cancellation), not
// AbortTurn: "stop this delegation" means the whole subtree targetID may
// itself have fanned out, not merely targetID's own current turn —
// AbortTurn's own doc comment is explicit that it deliberately leaves a
// target's children running, which is the opposite of what a caller
// asking to cancel a child it spawned wants. See Cancel's doc comment
// for the one case it does NOT reach on its own — a ROOT's turn driven
// by an ExternalRunner needs the external scheduler's own abort too
// (server.Server's handleCancelTree calls both) — which cannot arise
// here: isDescendantLocked can never return true for a root target
// (parentID == "" has no ancestor by construction), so targetID is
// always a genuine child, always driven directly by this package, and
// Cancel alone is always sufficient.
//
// Returns targetID's resulting status (StatusCanceled for the ordinary
// case; whatever terminal status it already carried if canceling an
// already-done/failed descendant was a no-op — see cancelOneNodeLocked's
// own doc comment) read back BEFORE releasing m.mu, in the SAME locked
// operation that performed the cancellation — not via a separate later
// Info(targetID) call. A live review finding on this fix's first pass:
// a done/failed/canceled LEAF is Reap-eligible the instant it is
// finalized (Reap's own doc comment), so a caller's periodic Reap sweep
// could collect targetID in the gap between this method returning and a
// separate follow-up read, turning "no such session" into an incorrect
// answer for a cancel call that had just, in fact, succeeded. Reading
// the outcome inside the same critical section that produced it closes
// that gap by construction rather than papering over it with a guess.
//
// Returns ErrUnknownSession if either id is not tracked, ErrNotDescendant
// if targetID is not callerID's descendant.
func (m *SessionManager) CancelDescendant(callerID, targetID string) (SessionStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[callerID]; !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownSession, callerID)
	}
	n, ok := m.nodes[targetID]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownSession, targetID)
	}
	if !m.isDescendantLocked(callerID, targetID) {
		return "", fmt.Errorf("%w: %s", ErrNotDescendant, targetID)
	}
	m.cancelSubtreeLocked(n)
	return n.status, nil
}

// DescendantInfo returns targetID's lifecycle snapshot (status, lineage,
// result/fail reason) and cumulative token usage, after confirming
// callerID is a live ancestor of targetID — the SessionManager side of
// the `task` tool's status verb, and the engine-level counterpart to the
// wire's session.info payload (design doc: "status, lineage ..., usage").
//
// Usage is read from the live *Session (n.session.Usage(), which takes
// s.mu internally) while m.mu is still held — nesting m.mu outer, a
// session's own mu inner, the same order Spawn's parentSess.mu access
// already establishes for this package — so it reflects targetID's
// current total, not a stale or partial snapshot.
//
// Returns ErrUnknownSession if either id is not tracked, ErrNotDescendant
// if targetID is not callerID's descendant.
func (m *SessionManager) DescendantInfo(callerID, targetID string) (SessionNode, provider.Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[callerID]; !ok {
		return SessionNode{}, provider.Usage{}, fmt.Errorf("%w: %s", ErrUnknownSession, callerID)
	}
	n, ok := m.nodes[targetID]
	if !ok {
		return SessionNode{}, provider.Usage{}, fmt.Errorf("%w: %s", ErrUnknownSession, targetID)
	}
	if !m.isDescendantLocked(callerID, targetID) {
		return SessionNode{}, provider.Usage{}, fmt.Errorf("%w: %s", ErrNotDescendant, targetID)
	}
	snap := n.snapshot()
	// Children is the DURABLE-plus-live union, not the live list alone —
	// a review finding: Reap removes a terminal leaf from its parent's
	// live children, so once a descendant's own children finish and are
	// swept, n.children is empty while the session's durable spawn record
	// still names them. The wire's GET /session/{id}/lineage already
	// merges the same two sources (childIDsUnion, server/handlers.go),
	// and the design doc promises `task status` reports the lineage the
	// wire reports — reporting only the live half told a model inspecting
	// its own delegation tree that grandchildren it really did spawn
	// never existed.
	//
	// Durable first, in spawn order, then any live id the durable list
	// does not have: the same order the wire builds, so the two answers
	// are byte-comparable. n.depth needs no equivalent merge — the wire
	// prefers the durable TaskDepth and adoptReloadedLocked already
	// restores n.depth from that same value.
	// snap.Children, not n.children: snapshot already made a private copy,
	// and mergeChildIDs takes ownership of its live argument, so the
	// common case (no durable ids to merge) reuses that one copy instead
	// of allocating a second — a review finding on the first version of
	// this call, which discarded snapshot's copy and built another.
	snap.Children = mergeChildIDs(n.session.SpawnedChildIDs(), snap.Children)
	return snap, n.session.Usage(), nil
}

// DescendantTranscript returns targetID's lifecycle snapshot and the LAST
// tail messages of its canonical history, after the same ancestry check
// CancelDescendant/DescendantInfo/SendToDescendant apply — the
// SessionManager side of the `task` tool's log verb.
//
// It works for a LIVE descendant and a DEAD one alike, which is the whole
// point: a node stays in m.nodes after it goes terminal, until Reap
// collects it, and its *Session — history included — stays with it. A
// parent whose child died therefore reads the child's own last messages
// through the same call it would use on a running one, with no session
// reload and no disk read. A REAPED descendant is gone from the tree and
// answers ErrUnknownSession, exactly as it does for every other verb; the
// child's durable log still exists on disk for an operator to read out of
// band.
//
// History is read through n.session.History() (which takes s.mu) while
// m.mu is still held — m.mu outer, session mu inner, the same nesting
// DescendantInfo's own Usage() read establishes.
//
// tail <= 0 returns no messages rather than the whole history: the caller
// decides the bound, and a zero must never silently mean "everything" on
// a surface whose output goes into a model's context. A tail larger than
// the history returns the whole history.
//
// total is the descendant's WHOLE message count, reported alongside the
// trimmed slice and read in the SAME locked operation. A caller cannot
// derive it from the slice — that is by construction only the part that
// survived the bound — and a separate follow-up read could race the
// descendant's own next turn appending to it.
//
// Returns ErrUnknownSession if either id is not tracked, ErrNotDescendant
// if targetID is not callerID's descendant.
func (m *SessionManager) DescendantTranscript(callerID, targetID string, tail int) (node SessionNode, msgs []message.Message, total int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[callerID]; !ok {
		return SessionNode{}, nil, 0, fmt.Errorf("%w: %s", ErrUnknownSession, callerID)
	}
	n, ok := m.nodes[targetID]
	if !ok {
		return SessionNode{}, nil, 0, fmt.Errorf("%w: %s", ErrUnknownSession, targetID)
	}
	if !m.isDescendantLocked(callerID, targetID) {
		return SessionNode{}, nil, 0, fmt.Errorf("%w: %s", ErrNotDescendant, targetID)
	}
	snap := n.snapshot()
	history := n.session.History()
	total = len(history)
	if tail <= 0 {
		return snap, nil, total, nil
	}
	if len(history) > tail {
		history = history[len(history)-tail:]
	}
	return snap, history, total, nil
}

// mergeChildIDs returns durable's ids first, in order, then every id in
// live that durable does not already name — the engine-side twin of the
// wire's childIDsUnion (server/handlers.go). Never nil for a childless
// session: an empty, non-nil slice, matching snapshot's own shape.
//
// Takes ownership of live: it is returned as-is when there is nothing to
// merge, so the caller must pass a slice it owns (snapshot's own fresh
// copy), never a live node field.
func mergeChildIDs(durable, live []string) []string {
	if len(durable) == 0 {
		// Live cannot contain duplicates: adoptLocked appends a child id
		// at most once.
		return live
	}
	out := make([]string, 0, len(durable)+len(live))
	seen := make(map[string]bool, len(durable)+len(live))
	for _, ids := range [2][]string{durable, live} {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// SendToDescendant delivers text to targetID, a live descendant of
// callerID, after the same ancestry check as CancelDescendant/
// DescendantInfo — the SessionManager side of the `task` tool's send
// verb. Reuses the wire's session.send machinery and settled-target
// semantics (server/session_tree.go's handleSessionSend, child branch —
// specifically its underlying Send/EnqueuePrompt primitives) rather than
// duplicating a third implementation, but DIVERGES from it, deliberately,
// for a RUNNING target: handleSessionSend's own child branch calls
// CanSend, which refuses a running child outright (ErrSessionBusy, wire
// 409) — its own comment there is explicit that "a child has no prompt
// queue... nowhere to defer to," true of THAT surface. This method gives
// it one instead, per the design doc's own explicit choice for the
// `task` tool's send verb ("reuse the existing prompt-queue machinery
// rather than rejecting busy children, mirroring how roots queue
// prompts") — a live review finding on an earlier revision of this
// comment, which claimed this "mirrors" the wire unconditionally,
// overlooking that the two surfaces now behave OPPOSITELY for exactly
// this case:
//
//   - A RUNNING target gets text appended to its own durable prompt
//     queue (in memory synchronously, the durable record deferred — see
//     enqueueMemoryOnlyLocked's own doc comment) instead of being
//     refused. Delivery itself takes one of two paths: the
//     target's own mid-turn tool-call-boundary drain (engine.go's Prompt
//     loop, the "OPERATOR MESSAGES" injection) if one arrives before the
//     current turn ends, or — if it does not, since a child (unlike a
//     root) has no external residency layer to pick the queue back up
//     once Prompt returns — drainQueueAndPrompt (below), called from
//     both of the places that drive a child's own turn (Spawn's launched
//     goroutine, and Send itself for a child target). A live review
//     finding on this fix's first pass: relying on the mid-turn drain
//     ALONE stranded a message enqueued after a target's last tool-call
//     boundary — its turn would simply end, taking the child straight to
//     done/failed with the message still sitting, undelivered, in its
//     own promptQueue forever. drainQueueAndPrompt closes that gap.
//     queued is true on this path.
//   - A settled (idle/done/failed) target is restarted with text as its
//     next turn — existing Send semantics, unchanged — but launched in a
//     goroutine rather than called synchronously: Send blocks until the
//     WHOLE re-run turn completes (see its own doc comment), and
//     SessionManager's one deliberately non-blocking entry point is
//     Spawn (the "non-blocking execution" locked decision) — calling
//     Send inline here would make this verb the one exception, blocking
//     the caller's own tool call for as long as the re-run takes. The
//     re-run's outcome is not this call's to report: it arrives later
//     via the ordinary completion-notification path, exactly like a
//     Spawn'd child's result. queued is false on this path.
//   - A canceled target, or one that would push the tree over its
//     concurrency cap, is refused synchronously and deterministically —
//     mirroring CanSend's own pre-Send admission checks (used by
//     handleSessionSend for the identical reason: a caller-visible error
//     up front, not a silently dropped launch). Unlike an earlier
//     revision of this method, there is no residual admission race left
//     to accept here: the settled-target path reserves the turn
//     (reserveSendLocked) inside the SAME m.mu critical section that
//     just re-validated targetID, rather than releasing m.mu and having
//     a freshly launched goroutine call Send, which would re-acquire
//     m.mu from scratch — closing a live review finding (a caller's own
//     periodic Reap() sweep could collect an already-terminal leaf in
//     that gap, silently discarding the resulting ErrUnknownSession and
//     leaving the "dispatched as a fresh turn" promise unfulfilled) by
//     eliminating the window entirely rather than merely narrowing or
//     documenting it.
//
// Returns ErrUnknownSession if either id is not tracked, ErrNotDescendant
// if targetID is not callerID's descendant, ErrSessionCanceled if
// targetID is canceled, or ErrConcurrencyLimit if the tree is already at
// its running-children cap (settled-target restart path only — a
// running target's enqueue never touches this budget).
func (m *SessionManager) SendToDescendant(callerID, targetID, text string) (queued bool, err error) {
	// Validate and trim ONCE, before either delivery path — a review
	// finding: the running-target branch rejected blank text while the
	// settled-target branch passed it straight to Prompt and burned a
	// whole re-run turn on a space. runTaskSend masks that for the `task`
	// tool, but this is an exported API and the asymmetry is exactly the
	// class this method exists to remove. The trimmed text is what both
	// paths use, matching EnqueuePrompt's own trim-then-store rule
	// (queue.go).
	text = strings.TrimSpace(text)
	if text == "" {
		return false, ErrEmptyPromptText
	}
	m.mu.Lock()
	if _, ok := m.nodes[callerID]; !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrUnknownSession, callerID)
	}
	n, ok := m.nodes[targetID]
	if !ok {
		m.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrUnknownSession, targetID)
	}
	if !m.isDescendantLocked(callerID, targetID) {
		m.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrNotDescendant, targetID)
	}
	if n.status == StatusRunning {
		// Mutate the queue and emit its event while STILL HOLDING m.mu —
		// nested m.mu (outer) -> s.mu (inner, taken inside
		// enqueueMemoryOnlyLocked itself), the same lock order Spawn's
		// own parentSess.mu access already establishes elsewhere in this
		// file — not released first. A live review finding: an earlier
		// version of this branch released m.mu before enqueueing,
		// leaving a window where a concurrent finalizeTurn call (also
		// serialized on m.mu) could observe n.status still Running (the
		// read just above already happened), transition n to
		// done/failed, and have THIS enqueue land afterward in an
		// already-settled child's queue that nothing would ever drain
		// again. Keeping both steps inside ONE m.mu critical section
		// makes them atomic relative to finalizeTurn's own
		// terminal-status transition (which also runs under m.mu
		// throughout — see its own doc comment): either this whole block
		// completes strictly before finalizeTurn's, in which case
		// finalizeTurn's own queue re-check (added alongside this fix)
		// sees the enqueued text and re-drives instead of finalizing; or
		// finalizeTurn's transition completes strictly first, in which
		// case the n.status read above would instead observe the new
		// terminal status and this method would take the settled/
		// relaunch branch below, never reaching here at all. Neither
		// ordering can strand the message — see finalizeTurn's matching
		// doc comment for the other half of this fix.
		//
		// The durable persist (persistPromptQueueLocked's own
		// ensureLog+writeRecord) is deliberately NOT run inline here —
		// a SEPARATE live review finding: doing so would hold m.mu, the
		// tree-wide lock every OTHER session's Info/Reap/Spawn/finalize
		// call also needs, across a synchronous disk write for the
		// WHOLE duration of this ONE session's fsync. Deferred via
		// SessionManager.deferPersist/unlockAndFlushPersist instead —
		// the exact mechanism this package's task-notification delivery
		// path already uses for its own durable writes
		// (commitOutcomeLocked, finalizeTurn's own notify-delivery
		// block) — so the write happens after m.mu releases, in this
		// same goroutine, in order.
		//
		// The record is PARKED on the session
		// (queueRecordDeferredLocked), under the same s.mu hold as the
		// memory append, and only then flushed by the deferPersist
		// thunk — never written from the thunk's own closure. See that
		// method's own doc comment (queue.go): a closure-held record
		// could reach disk AFTER the child's own goroutine (which needs
		// only s.mu) drained this very item and wrote its
		// prompt.dequeued record synchronously, and LoadSession's fold
		// then resurrected the delivered prompt.
		s := n.session
		s.mu.Lock()
		p := s.enqueueMemoryOnlyLocked(text)
		s.queueRecordDeferredLocked(recPromptQueued, promptRecord{ID: p.ID, Text: p.Text},
			Event{Type: EventPromptQueued, QueueID: p.ID, QueueText: p.Text, QueueLen: len(s.promptQueue)})
		s.mu.Unlock()
		m.deferQueueRecordFlush(s)
		m.unlockAndFlushPersist()
		return true, nil
	}
	// Settled (idle/done/failed): reserve the turn HERE, inside this SAME
	// m.mu critical section that just re-validated targetID's existence/
	// ancestry/status — not via a separate later Send call re-acquiring
	// m.mu from scratch (see reserveSendLocked's own doc comment for the
	// Reap race this closes). n.status is already flipped to
	// StatusRunning, atomically, before this method ever returns — the
	// turn is genuinely committed, not merely intended.
	s, nodeCtx, _, rerr := m.reserveSendLocked(targetID)
	// unlockAndFlushPersist, not a plain m.mu.Unlock(), even though
	// reserveSendLocked defers nothing today — that method's own doc
	// comment makes it the standard unlock for every SessionManager
	// method (cheap, an empty-slice no-op when nothing is queued), so a
	// future deferred write on this path cannot be dropped silently. A
	// review finding on the lone plain unlock this method had left.
	m.unlockAndFlushPersist()
	if rerr != nil {
		// ErrSessionCanceled/ErrConcurrencyLimit: reachable — mirror the
		// checks this method used to run separately, now folded into
		// reserveSendLocked. ErrSessionBusy/ErrUnknownSession are NOT
		// reachable from here: status was already confirmed not Running
		// above and targetID confirmed tracked moments ago, both under
		// this one unbroken m.mu hold — kept anyway, in
		// reserveSendLocked's own defensive spirit (see its doc
		// comment), rather than asserted away.
		return false, rerr
	}
	go func() {
		// drainQueueAndPrompt unconditionally, not gated on the isChild
		// reserveSendLocked also returns: targetID is ALWAYS a genuine
		// descendant here (isDescendantLocked guaranteed it above), so
		// isChild is always true — this call can never actually reach a
		// root.
		msg, perr := drainQueueAndPrompt(nodeCtx, s, text)
		if resume := m.finalizeTurn(targetID, msg, perr); resume != nil {
			go resume()
		}
	}()
	return false, nil
}

// accumulateUsageLocked folds n's newly-spent usage (this turn's DELTA,
// not its full cumulative total — see budgetedUsage's own doc comment
// for why a delta is required: n may be a done/failed child Send just
// restarted for a follow-up turn, and this is not the first time this
// has run for it) into its ROOT's running tree-wide total — see
// usageByRoot's own doc comment ("per-tree budgets," a follow-up
// finding). Called unconditionally, near the very top of finalizeTurn,
// BEFORE that method's own re-drive-or-settle decision: it must run
// exactly once per turn that actually happened, regardless of whether
// finalizeTurn goes on to settle n terminal, re-drive it (the queued-
// message race fix — see finalizeTurn's own doc comment), or leave a
// root idle — a live review finding on this fix's first pass, which ran
// this accumulation much later, AFTER the re-drive branch's own early
// return, silently skipping it for however long the re-driven turn
// takes and letting a concurrent Spawn under-count the tree budget in
// that window (self-correcting once the re-driven turn's own eventual
// call reaches this same accumulation with the combined total, but a
// real, if narrow, gap until then). Never re-derived by re-summing every
// node — incremental, mirroring runningByRoot's own accumulator shape
// exactly. Caller holds m.mu.
func (m *SessionManager) accumulateUsageLocked(n *sessionNode) {
	total := n.session.Usage()
	delta := provider.Usage{
		InputTokens:      total.InputTokens - n.budgetedUsage.InputTokens,
		OutputTokens:     total.OutputTokens - n.budgetedUsage.OutputTokens,
		CacheReadTokens:  total.CacheReadTokens - n.budgetedUsage.CacheReadTokens,
		CacheWriteTokens: total.CacheWriteTokens - n.budgetedUsage.CacheWriteTokens,
	}
	n.budgetedUsage = total
	m.budgetedByChild[n.id] = total
	u := m.usageByRoot[n.rootID]
	u.InputTokens += delta.InputTokens
	u.OutputTokens += delta.OutputTokens
	u.CacheReadTokens += delta.CacheReadTokens
	u.CacheWriteTokens += delta.CacheWriteTokens
	m.usageByRoot[n.rootID] = u
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
	return m.finalizeTurnFrom(id, msg, perr, false)
}

// finalizeTurnFrom is finalizeTurn with the one fact finalizeTurn's own
// signature cannot carry: WHO drove the turn. external is true only for
// ReportTurnEnd — a turn an outside scheduler ran (the server's runPrompt/
// runGoal on a resident session) — and false for every in-package driver
// (Spawn's launched goroutine, Send, triggerResumeLocked, this method's own
// recursive re-drive).
//
// It exists because the queued-message re-drive below is only correct for
// an in-package turn, a review finding. The re-drive assumes the resume it
// returns is the SOLE continuation of the session and that no run slot is
// held by anyone else. For an externally scheduled turn both assumptions
// are false: the server holds its own run slot across the ReportTurnEnd
// call and its tail (maybeDispatchQueued, server/handlers.go) drains the
// queue itself as an ordinary prompt turn. A depth>0 node reaches that
// path for real — claimForPrompt LoadSessions any id with no depth guard,
// so a former child driven through POST /session/{id}/prompt_async or
// /goal is resident and externally scheduled — and the re-drive then did
// one of two wrong things: with one item queued it popped it and returned
// a resume that runs Prompt with no slot held (a concurrent HTTP prompt
// can claim the freed slot and call Prompt on the same session), and with
// two it popped the first, journaled it delivered, and left the server's
// own tail to dispatch the second, so the first never ran.
func (m *SessionManager) finalizeTurnFrom(id string, msg *message.Message, perr error, external bool) (resume func()) {
	m.mu.Lock()
	n, ok := m.nodes[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}

	// Accumulate n's newly-spent usage BEFORE the re-drive check below —
	// a live review finding: an earlier version of this call ran only
	// once, much later (right before the settled-marker persist), which
	// the re-drive branch's early return skipped entirely. That left
	// usageByRoot under-counting the just-finished turn's tokens for the
	// WHOLE duration of the re-driven turn (self-correcting once ITS OWN
	// eventual finalizeTurn call finally reaches this same accumulation
	// with the combined total, but a concurrent Spawn checking the tree
	// budget in that window could be wrongly admitted). See
	// accumulateUsageLocked's own doc comment for why this is safe to
	// run unconditionally, every call, regardless of which path below
	// this ends up taking.
	m.accumulateUsageLocked(n)

	// A CHILD (depth>0), not already canceled, may have had a message
	// enqueued to it by a concurrent SendToDescendant call that read
	// n.status == StatusRunning and enqueued — both under this SAME
	// m.mu, nested, never released in between (see SendToDescendant's
	// own doc comment) — in the instant strictly before this call
	// acquired m.mu. drainQueueAndPrompt's own loop, which drove
	// whichever turn just produced msg/perr, already made its own last
	// (empty) queue check and returned BEFORE this call ever started —
	// it cannot see anything enqueued after that point. Re-check here,
	// atomically with the terminal-status decision the rest of this
	// method is about to make, and dequeue-and-re-drive instead of
	// settling if anything is waiting: a live review finding, and the
	// other half of the fix described in SendToDescendant's own doc
	// comment (nesting the enqueue under m.mu is necessary but not
	// sufficient without this matching re-check on the finalize side).
	//
	// Runs BEFORE any of this method's own bookkeeping below
	// (decrementRunningLocked, notify construction, commitOutcomeLocked,
	// the settled marker) and returns immediately when it fires: from
	// THIS call's point of view the turn simply is not over yet, so
	// nothing below should run for it at all. The re-driven turn's own
	// EVENTUAL finalizeTurn call is what actually settles n — exactly as
	// if drainQueueAndPrompt's own loop had picked the message up
	// directly, which, absent the race this closes, it always does.
	// n.status is deliberately left untouched (still StatusRunning): the
	// child genuinely is still running, one more turn.
	//
	// Dequeues via dequeueMemoryOnlyLocked, not DequeuePrompt, for the
	// same reason SendToDescendant's enqueue does — see its own doc
	// comment: DequeuePrompt's own persistPromptQueueLocked call is a
	// synchronous disk write, and this whole branch runs under m.mu, the
	// tree-wide lock every OTHER session's Info/Reap/Spawn/finalize call
	// also needs. The durable record is deferred via deferPersist,
	// flushed by unlockAndFlushPersist below, same as SendToDescendant's
	// own enqueue and this package's existing task-notification writes.
	//
	// Gated on n.ctx as well as n.status — a review finding.
	// StatusCanceled is set ONLY by cancelOneNodeLocked/
	// cancelSubtreeLocked (task cancel, AbortTurn), so a cascade cancel
	// of the manager's own base ctx — process shutdown — cancels n.ctx
	// and leaves n.status at StatusRunning. The status test alone then
	// popped a queued prompt and journaled it prompt.dequeued
	// ("delivered") while drainQueueAndPrompt's own ctx guard made sure
	// nothing ran, and the resume's own finalizeTurn call re-entered
	// this gate and popped the next one, draining the whole queue as
	// delivered; a later reload folds those records out and the prompts
	// are gone. Testing n.ctx here gives a ctx-only cancel the same
	// "the queue stays queued, untouched" outcome a status cancel has
	// (see drainQueueAndPrompt's own doc comment for that contract).
	//
	// One bounded exception to that contract lives PAST this gate, named
	// here at a review's request rather than left absolute: this branch
	// pops next and journals it dequeued("delivered") before the resume
	// goroutine runs, so a cancel landing in THAT window leaves exactly
	// one item recorded delivered that never ran. Journaling the dequeue
	// before the text enters a turn is what makes double delivery
	// impossible, and holding the record back until the resume has
	// started would trade this bounded inconsistency for a crash window
	// that delivers the same prompt twice — the same lose-once-on-crash
	// exposure every in-flight prompt already carries (see
	// maybeDispatchQueued's "No-double-delivery equivalence", invariant
	// 7, server/handlers.go). Every item still IN the queue is untouched,
	// exactly as documented.
	if !external && n.parentID != "" && n.status != StatusCanceled && n.ctx.Err() == nil {
		s := n.session
		s.mu.Lock()
		next, ok := s.dequeueMemoryOnlyLocked()
		if ok {
			// Parked on the session, under the same s.mu hold as the
			// memory pop — see queueRecordDeferredLocked's own doc
			// comment (queue.go) for why a closure-held record breaks
			// the log's record order and what that costs on replay.
			s.queueRecordDeferredLocked(recPromptDequeued, promptRecord{ID: next.ID, Text: next.Text, Reason: "delivered"},
				Event{Type: EventPromptDequeued, QueueID: next.ID, QueueText: next.Text, QueueReason: "delivered", QueueLen: len(s.promptQueue)})
		}
		s.mu.Unlock()
		if ok {
			m.deferQueueRecordFlush(s)
			nodeCtx := n.ctx
			m.unlockAndFlushPersist()
			return func() {
				nmsg, nperr := drainQueueAndPrompt(nodeCtx, s, next.Text)
				// go, not inline — matching every other recursive resume
				// invocation in this file (triggerResumeLocked's own
				// closures): keeps a pathological repeatedly-re-enqueued
				// chain from growing one goroutine's call stack instead
				// of just its goroutine count.
				if resume2 := m.finalizeTurn(id, nmsg, nperr); resume2 != nil {
					go resume2()
				}
			}
		}
	}

	alreadyCanceled := n.status == StatusCanceled
	m.decrementRunningLocked(n)
	// See sessionNode.finalized's doc comment: this call is always the
	// point n's concurrency-slot bookkeeping (the decrement just above)
	// is fully settled, regardless of which status branch runs below —
	// safe for Reap to remove n once this is true.
	n.finalized = true

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
		// distinct StatusCanceled value — this Status/FailReason pair is
		// the ordinary PARENT-facing wire shape (queued/rendered exactly
		// like any other failed child), never itself compared against
		// status. Canceled: true is the SEPARATE, restore-only signal
		// (see its own doc comment, taskdelivery.go) that lets a LATER
		// re-adoption of this same child (restoreKnownStatusLocked)
		// distinguish this from an ordinary failure and correctly restore
		// StatusCanceled rather than silently rewriting history to
		// StatusFailed — a live review finding.
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusFailed, FailReason: "canceled", Canceled: true, Usage: n.session.Usage()}
	case perr != nil:
		n.status = StatusFailed
		n.failReason = classifySpawnError(perr)
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusFailed, FailReason: n.failReason, Usage: n.session.Usage()}
	default:
		n.status = StatusDone
		// msg is nil for a caller that never has one to give — an
		// external scheduler's ReportTurnEnd(id, nil, err) (server's
		// runGoal and cmd/harness's own runGoal both pass nil
		// unconditionally, on the assumption documented at each call
		// site that a child never reaches ReportTurnEnd's root-only
		// PursueGoal path — an assumption adoptReloadedLocked broke: a
		// reloaded former child whose true parent is still tracked is
		// re-attached as a genuine depth>0 node, so a goal call against
		// its id (POST /session/{id}/goal, cold-loaded) can reach this
		// exact branch with msg == nil). Treated as an empty result
		// rather than dereferenced — a live review caught the resulting
		// nil-pointer panic.
		if msg != nil {
			n.result = msg.Parts.Text()
		}
		notify = &taskNotification{ChildID: n.id, Agent: n.agentType, Status: StatusDone, Result: n.result, Usage: n.session.Usage()}
	}
	// One notification for the whole terminal transition above: every
	// field it writes (finalized, status, result, failReason) is set
	// under this single m.mu hold, so an observer woken here re-reads a
	// fully settled node. See markChangedLocked's own doc comment.
	m.markChangedLocked()

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
		forwarded = n.session.drainAllTaskNotifications() // memory-only — see its own doc comment
	}

	// Commit notify BEFORE attempting delivery — see
	// SessionManager.commitOutcomeLocked's own doc comment and the
	// crash-window table on recoverInterruptedTurnLocked's own doc
	// comment for the full mechanism this closes: a live review finding
	// that a crash landing INSIDE this method's own deliver-then-settle
	// sequence below (the notify already durably queued on target's log,
	// but this turn not yet marked settled) let a later recovery attempt
	// reconstruct a DIFFERENT payload than the one already delivered —
	// for a failed turn, a generic "lost to restart" instead of the real
	// classified reason; the ancestor ends up told the same child both
	// succeeded and failed. Gated on hasTaskParent() (see that method's
	// own doc comment for why this must be the SAME predicate the
	// settled-marker gate below uses), not merely `notify != nil`: for a
	// genuine root, notify is already nil from the switch above, so this
	// is a no-op either way — the explicit check just keeps this line
	// visibly consistent with the settled-marker gate right below it.
	if n.session.hasTaskParent() && notify != nil {
		m.commitOutcomeLocked(n.session, *notify)
	}

	if notify != nil || len(forwarded) > 0 {
		if target := m.nearestLiveAncestorLocked(n); target != nil {
			// Memory-only append here, durable write deferred via
			// m.deferPersist to run AFTER m.mu is released — a live
			// review finding: this method runs under m.mu (the single
			// lock guarding every session in the tree), and
			// enqueueTaskNotification persists inline, running disk I/O
			// while that global lock was held. See
			// SessionManager.deferPersist/unlockAndFlushPersist's own
			// doc comment for the full mechanism.
			targetSess := target.session
			if notify != nil {
				targetSess.enqueueTaskNotificationMemoryOnly(*notify)
				nn := *notify
				m.deferPersist(func() { targetSess.persistQueuedTaskNotification(nn) })
			}
			for _, fn := range forwarded {
				targetSess.enqueueTaskNotificationMemoryOnly(fn)
				m.deferPersist(func() { targetSess.persistQueuedTaskNotification(fn) })
			}
			if len(forwarded) > 0 {
				sourceSess := n.session
				m.deferPersist(func() { sourceSess.persistDeliveredTaskNotifications(forwarded) })
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
	// Mark this turn settled — see turnUnsettled's own doc comment
	// (engine.go) for the full reasoning: this is what lets
	// recoverInterruptedTurnLocked tell an ordinary, properly-finalized
	// outcome (this call reaching this point at all) apart from a
	// genuine crash, instead of the unreliable trailing-message-role
	// heuristic a live review found broken in both directions. Only
	// meaningful for a non-root node — a root is never a
	// recoverInterruptedTurnLocked candidate (adoptReloadedLocked's own
	// early return). Queued via deferPersist AFTER the delivery thunks
	// above, deliberately: a crash between "notify delivered" and "this
	// child's own turn marked settled" must still leave the child
	// looking unsettled on the next reload (a safe, if redundant, retry
	// of recovery for something already delivered — the SAME crash-
	// window discipline recoverInterruptedTurnLocked's own reorder
	// established, applied here too for the ordinary-completion path).
	//
	// Gated on hasTaskParent(), NOT n.parentID != "" — a live review
	// finding: the in-memory sessionNode.parentID and the durable
	// TaskParentID() can disagree for a node adoptReloadedLocked attached
	// with attachTo=="" because its real parent was not tracked (the
	// "true depth is unrecoverable" case — see that method's own doc
	// comment), even though it durably DOES have a real TaskParentID.
	// Gating this on the in-memory pointer meant such a node's turns were
	// NEVER marked settled, even on a completely ordinary, successful
	// completion — hasUnfinalizedTurn() stayed true forever, and a LATER
	// AdoptReloaded(recover=true) for it (adoptReloadedLocked's own
	// root/non-root branch DOES use TaskParentID(), so it does not treat
	// this node as a root) spuriously ran recovery against a turn that
	// had already finished cleanly. hasTaskParent() is the SAME predicate
	// adoptReloadedLocked's own root/non-root branch uses, so the two
	// ends of this exact crash/degraded-lineage window can no longer
	// disagree about which nodes this covers.
	if n.session.hasTaskParent() {
		n.session.markTurnSettled()
		childSess := n.session
		m.deferPersist(func() { childSess.persistTurnSettled() })
	}
	m.unlockAndFlushPersist()

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
	// See ReportTurnStart's identical reset for why: node may be resuming
	// from a PRIOR completed turn and still carry finalized=true from it.
	node.finalized = false
	m.markChangedLocked()
	if node.depth > 0 {
		m.runningByRoot[node.rootID]++
	}
	s := node.session
	ctx := node.ctx
	id := node.id
	if node.depth == 0 && m.externalRunner != nil {
		runner := m.externalRunner
		return func() {
			switch runner(id, taskResumeTriggerText) {
			case RunnerHandled:
				// The external scheduler now owns this turn and is
				// responsible for reporting its completion back via
				// ReportTurnStart/ReportTurnEnd itself (and for deferring
				// any further resume THAT call returns past its own
				// run-slot release — see ReportTurnEnd's doc comment).
				return
			case RunnerRefused:
				// The scheduler recognizes id but is refusing this
				// attempt right now (a live review finding this
				// centralizes: an earlier revision left every
				// ExternalRunner implementation responsible for
				// remembering this call itself — see RunnerOutcome's own
				// doc comment). No bracketed turn will ever settle the
				// commitment triggerResumeLocked made above, so undo it
				// here instead of leaving id stuck StatusRunning forever
				// with queue-or-resume dead for it.
				m.RevertResumeIfStillRunning(id)
				return
			}
			// RunnerUnknown: the scheduler doesn't recognize this id at
			// all — fall back to driving it directly rather than losing
			// the resume. This call owns the WHOLE turn itself (no
			// server run-slot involved), so firing any further resume
			// immediately is safe — no release to race.
			msg, err := s.PromptEngineResume(ctx, taskResumeTriggerText)
			if resume := m.finalizeTurn(id, msg, err); resume != nil {
				go resume()
			}
		}
	}
	return func() {
		msg, err := s.PromptEngineResume(ctx, taskResumeTriggerText)
		if resume := m.finalizeTurn(id, msg, err); resume != nil {
			go resume()
		}
	}
}

// RevertResumeIfStillRunning undoes triggerResumeLocked's speculative
// commitment for id — status back to StatusIdle (it never actually
// started a turn) and the depth>0 concurrency reservation released — for
// a case where NO bracketed turn will EVER call ReportTurnEnd to release
// that commitment on its own: a workdir-held conflict (a DIFFERENT
// session entirely holds the shared workdir — id itself may not be
// running anything at all), or a draining server. This is unlike an
// ORDINARY "busy" refusal, where a different, already-running BRACKETED
// turn holds the slot and WILL eventually call ReportTurnEnd, correctly
// picking up the still-pending notification itself (see finalizeTurn's
// perr == nil && hasPendingTaskNotifications() re-trigger case) — no
// revert needed there.
//
// Called from exactly one place now: triggerResumeLocked's own closure,
// centrally, whenever an ExternalRunner reports RunnerRefused — see
// RunnerOutcome's own doc comment for why this moved here instead of
// staying each ExternalRunner implementation's own responsibility (a
// live review finding: the bool-returning predecessor of RunnerOutcome
// left that call easy to forget in any FUTURE implementation, with
// nothing but a doc comment enforcing it). Still exported: an
// ExternalRunner implementation with its own reason to revert speculatively
// outside the RunnerRefused path it already has may still call this
// directly, but the ORDINARY refusal path no longer needs to.
//
// Reverting (rather than leaving id stuck StatusRunning forever, with
// nothing left to ever un-stick it and queue-or-resume dead for it) lets
// a LATER notification — or this same one, still pending — retry once
// the transient condition clears. A live review caught the original
// stuck-forever gap.
//
// Guarded by a status check: a no-op if id is no longer StatusRunning
// (something else already resolved it in the meantime) or is no longer
// tracked at all.
func (m *SessionManager) RevertResumeIfStillRunning(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok || n.status != StatusRunning {
		return
	}
	n.status = StatusIdle
	m.decrementRunningLocked(n)
	// No goroutine will ever call finalizeTurn for this abandoned
	// attempt (nothing actually started) — matching cancelOneNodeLocked's
	// own convention for a node with no in-flight Prompt call, this
	// slot's bookkeeping is already fully settled right here.
	n.finalized = true
	m.markChangedLocked()
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

// AbortTurn cancels id's OWN turn — via cancelOneNodeLocked, the SAME
// single-node bookkeeping Cancel/cancel_tree applies to every node it
// visits — but, UNLIKE Cancel, does NOT recurse into id's children. id
// itself always ends up StatusCanceled, the same clean, predictable
// outcome cancel_tree would give it; the two operations differ only in
// whether anything BEYOND id gets touched.
//
// This is the SessionManager-side implementation of the wire's POST
// /session/{id}/abort for a CHILD: "stop THIS session's turn," a
// DIFFERENT operation from cancel_tree's "tear down the whole subtree" —
// a live review caught an earlier revision of handleAbort using Cancel
// (the full cascade) for a child, making abort indistinguishable from
// cancel_tree: every descendant got explicitly marked StatusCanceled by
// cancelSubtreeLocked's tree walk, not just id itself.
//
// A real, unavoidable side effect remains: any descendant's context is
// context.WithCancel(parent.ctx) (see adoptLocked), so canceling id's own
// ctx here still interrupts an actually-running descendant's Prompt call
// — Go's context cancellation cascades through the tree regardless of
// whether SessionManager's own bookkeeping walks it explicitly. The
// difference from Cancel is in HOW that descendant's outcome gets
// recorded: cancel_tree marks it StatusCanceled directly, atomically,
// before anything else observes it (cancelSubtreeLocked's recursion
// reaches it too); AbortTurn never touches it at all, so it instead
// reaches StatusFailed (failReason "canceled", via finalizeTurn's
// ordinary perr != nil path once its own interrupted Prompt call
// returns) — the natural, individually-finalized consequence of
// interrupting its ancestor, not a first-class "this subtree was
// cancelled" outcome. Two genuinely different operations, not the same
// one under two names: id's own result matches user intuition (aborting
// id shows id canceled), while a caught-in-the-crossfire descendant's
// result stays observably distinct from an actual cancel_tree.
//
// A no-op — id's own status and context untouched — unless id is
// CURRENTLY StatusRunning. "abort" means "stop the turn in progress";
// a done/failed/canceled/idle id has no turn in progress to stop, and
// canceling its context anyway would be pure collateral damage: id's own
// ctx is permanent (context.CancelFunc never re-arms), so a later
// legitimate session.send to it would instantly fail on mergeCancel, and
// — the same unavoidable descendant-interruption AbortTurn's own doc
// comment above describes — any of id's OWN still-running descendants
// (spawned during an EARLIER turn, now outliving it) would be killed as
// collateral even though id itself has nothing left to abort. A live
// review reproduced exactly this: aborting an already-done child killed
// its still-running grandchild, and permanently broke the done child's
// own later reachability.
//
// Returns an error only if id is not tracked at all.
func (m *SessionManager) AbortTurn(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSession, id)
	}
	if n.status != StatusRunning {
		return nil
	}
	m.cancelOneNodeLocked(n)
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
	m.cancelOneNodeLocked(n)
	for _, cid := range n.children {
		if c, ok := m.nodes[cid]; ok {
			m.cancelSubtreeLocked(c)
		}
	}
}

// cancelOneNodeLocked applies cancelSubtreeLocked's single-node
// bookkeeping (status/finalized/ctx) to n ALONE, with no recursion into
// n.children — the shared core cancelSubtreeLocked (Cancel/cancel_tree)
// and AbortTurn both build on. See AbortTurn's doc comment for why that
// distinction (recurse or not) matters and what it does and does not
// change about a descendant's own outcome — the authoritative
// explanation; not repeated here.
func (m *SessionManager) cancelOneNodeLocked(n *sessionNode) {
	// wasRunning, captured before the status overwrite below, decides
	// sessionNode.finalized for n — see that field's doc comment. A node
	// canceled while StatusIdle had no in-flight Prompt call and so no
	// goroutine that will ever call finalizeTurn for it: nothing is left
	// to settle, safe to reap immediately. A node canceled while
	// StatusRunning still has one unwinding; finalizeTurn is what will
	// eventually mark it finalized, once that goroutine's own canceled
	// Prompt call actually returns.
	wasRunning := n.status == StatusRunning
	settled := false
	if n.status != StatusDone && n.status != StatusFailed && n.status != StatusCanceled {
		n.status = StatusCanceled
		settled = true
	}
	if !wasRunning {
		n.finalized = true
		settled = true
	}
	if settled {
		m.markChangedLocked()
	}
	// Canceling the context aborts an in-flight Prompt call driven
	// DIRECTLY by this package (Spawn's goroutine, Send, or a
	// SessionManager-driven resume) regardless of this node's status —
	// always safe, and a no-op if already canceled. It does NOT abort a
	// turn an ExternalRunner is driving on its own context — see Cancel's
	// doc comment.
	n.cancel()
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
