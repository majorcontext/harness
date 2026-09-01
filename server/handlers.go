package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/plugin"
)

// sessionJSON is the openapi Session shape.
type sessionJSON struct {
	ID        string           `json:"id"`
	CreatedAt time.Time        `json:"created_at"`
	Model     message.ModelRef `json:"model"`
	// Effort is the session's current reasoning-effort level (empty =
	// EffortUnset, the provider default). A dashboard reads it back here after
	// POST /session/{id}/thinking, the same way it reads Model.
	Effort message.Effort `json:"effort,omitempty"`
	Status string         `json:"status"`
	// State is the unambiguous composite: idle, busy, or goal-running. Kept
	// alongside Status (never replacing it) for backward compat. Precedence:
	// goal-running wins whenever a goal is active, REGARDLESS of the momentary
	// worker status — including the goal loop's between-turn gap (worker
	// finished a turn, evaluator hasn't answered yet) where Status alone would
	// still read "busy" (the server's busy/idle transition brackets the WHOLE
	// PursueGoal call, not each turn) but a naive orchestrator inferring
	// progress from "status=idle plus goal.active" elsewhere is exactly the
	// ambiguity this field exists to remove. See compositeState.
	State    string    `json:"state"`
	Messages int       `json:"messages"`
	Seq      int64     `json:"seq,omitempty"`
	Goal     *goalJSON `json:"goal,omitempty"`
	WorkDir  string    `json:"workdir"`
	// LastTurn is the most recent prompt or goal-worker turn's outcome for
	// this process — "completed" or "error", plus the sanitized error detail
	// on failure — so a poller can distinguish "idle because done" from
	// "idle because the turn died" without inferring it from message part
	// shapes. Present only once a turn has finished in this process (like
	// Goal, absent on a freshly reloaded, never-prompted-here session).
	LastTurn *lastTurnJSON `json:"last_turn,omitempty"`
	// Usage is cumulative token usage plus message count and (when
	// available) the most recent turn's input tokens (issue #62 layer 2) —
	// so an orchestrator can rotate a session BEFORE it hits the provider's
	// context-window cliff (see LastTurn.outcome "context_exhausted" for
	// the case where it didn't rotate in time). Always present, since the
	// engine tracks usage for every session, resident or reloaded from its
	// log — a fresh, never-prompted session simply reports all zeros.
	Usage usageJSON `json:"usage"`
	// LastActivityAt is the timestamp of the most recent message appended
	// to the session (user, assistant, or tool) — or CreatedAt if none has
	// been appended yet. See engine.Session.LastActivityAt's doc comment
	// for why this exists: operators previously had to double-sample Seq to
	// distinguish a session quietly working from one wedged mid-turn; this
	// answers that directly, as a single absolute timestamp, resident or
	// not (a non-resident session gets it from LoadSession replay, same as
	// a resident one gets it from memory — no separate reconcile step
	// needed, unlike Goal/LastTurn above, which really are process-local).
	LastActivityAt time.Time `json:"last_activity_at"`
	// ParentSession is the session's lineage pointer (see
	// engine.Config.ParentSession's doc comment): an opaque provenance
	// pointer to the session this one continues from, set at creation via
	// POST /session's parent_session field, durable across resume/restart.
	// Absent (omitempty) when the session has no recorded parent — the
	// common case.
	ParentSession string `json:"parent_session,omitempty"`
	// CompactionCount/LastCompactedAt surface whether and when this session
	// has been compacted (docs/design/context-compaction.md), auto-
	// triggered or via POST /session/{id}/compact — so a UI can show that
	// compaction happened. CompactionCount is 0 (omitted) until the first
	// compaction; LastCompactedAt is the zero Time (omitted) likewise. Both
	// survive a restart (engine.Session.CompactionCount/LastCompactedAt
	// replay the compact journal record — see engine/store.go).
	CompactionCount int       `json:"compaction_count,omitempty"`
	LastCompactedAt time.Time `json:"last_compacted_at,omitzero"`
	// Plugins lists the session's configured plugins — name, spawn state,
	// registered tools, and subscribed hooks (see engine.Session.Plugins).
	// It reports CONFIGURED plugins, not only spawned ones, since a plugin
	// spawns lazily. Always present as an array (empty, never null) when no
	// plugins are configured.
	Plugins []plugin.Info `json:"plugins"`
	// Queued is the session's current durable prompt-queue depth (see
	// docs/plans/2026-07-19-prompt-queue.md): prompts submitted via
	// prompt_async while the session was busy, waiting for the next natural
	// drain trigger (idle dispatch or a goal loop's turn-boundary
	// injection). Always present (0 when nothing is waiting, never
	// omitted) — unlike Goal/LastTurn, this needs no "never happened here"
	// distinction, so there is no reason to hide the zero value. Read
	// directly from engine.Session.QueuedPrompts(), so it is correct for a
	// resident session and a freshly reloaded one alike (see buildSession).
	Queued int `json:"queued"`
	// Lineage is session.info's extension for the subagent-sessions tree
	// (docs/plans/2026-08-23-subagent-sessions-design.md): present
	// whenever this server's SessionManager tracks the session (every
	// session created or loaded by this process — see handleCreate and
	// s.opts.LoadSession call sites), OR — for a child session this
	// process has NOT (yet, or ever again) adopted, e.g. after a Reap or a
	// process restart — a durable fallback built directly from the
	// child's own on-disk Config.TaskParentID/TaskAgentType/TaskDepth/
	// SpawnedChildIDs (see lineageJSONFor's cold-fallback branch), missing
	// only the fields with no durable source at all (Status, Result,
	// FailReason — see lineageJSON's own field comments). nil only for a
	// genuine root or a session with no lineage at all (an embedder that
	// opts out, or a session predating this feature). Deliberately a
	// SEPARATE object from ParentSession above: that
	// field is an opaque, unvalidated cross-box provenance pointer with no
	// structural meaning; Lineage.ParentID names a LIVE session in this
	// process's tree, with enforced depth/concurrency, cascade
	// cancellation, and completion delivery. The two are unrelated
	// concepts that happen to share the word "parent."
	Lineage *lineageJSON `json:"lineage,omitempty"`
	// SubscriptionUsage is this session's most recently captured
	// subscription-lane rate-limit/quota snapshot (see
	// engine.Session.SubscriptionUsage / message.SubscriptionUsage's own
	// doc comments for the two lanes that capture one and the exact field
	// mapping). Deliberately NOT omitempty: null on the wire until a turn
	// in THIS process has carried the signal — a session that has never
	// delegated a turn through either lane, or has but this process
	// hasn't seen its first one yet — rather than omitted, so a caller can
	// unmarshal into a fixed struct without special-casing key presence.
	// Process-local only, like LastTurn/Goal above: never re-derived from
	// a durable source on a cold read (buildSessionFromIndex), since
	// persisting it is not required for GET /session's own contract here.
	SubscriptionUsage *message.SubscriptionUsage `json:"subscription_usage"`
}

// lineageJSON is sessionJSON's subagent-sessions extension, sourced from
// engine.SessionManager.Info — see sessionJSON.Lineage's doc comment.
type lineageJSON struct {
	ParentID string `json:"parent_id,omitempty"`
	// Depth is omitempty: a WARM root's real depth 0 and a session whose
	// true depth is genuinely UNKNOWN (a legacy child predating
	// Config.TaskDepth, cold or warm — see lineageJSONFor's Depth
	// paragraph) would otherwise be indistinguishable on the wire — both
	// print "depth":0. Since depth 0 only ever occurs for a root, and a
	// root never has a durable TaskParentID to trigger the cold-fallback
	// branch in the first place, omitting a zero depth costs nothing for
	// the warm case (a warm root's depth was always knowably 0; a caller
	// that cares can infer it from parent_id's absence) while letting an
	// unknown depth omit it truthfully instead of guessing. For a child
	// with a durably recorded TaskDepth (every child spawned since that
	// field shipped), THIS is the value reported — cold or warm alike,
	// never the live-tree-derived m.maxDepth refusal sentinel a reload
	// with no currently-tracked parent used to substitute (see
	// lineageJSONFor's Depth paragraph for the incident this closes).
	Depth int `json:"depth,omitempty"`
	// Status is the SessionManager lifecycle state (running/idle/done/
	// failed/canceled — engine.SessionStatus) — DISTINCT from
	// sessionJSON's own Status/State fields above, which predate
	// SessionManager and describe only "is a turn streaming right now" for
	// THIS process. A done/failed child keeps that status here even once
	// this process is no longer resident-tracking it any other way.
	// omitempty for the same reason as Depth: the cold-fallback branch
	// (lineageJSONFor) has no durable source for this SessionManager-only
	// field and must omit it rather than guess, and "" is not a real
	// engine.SessionStatus value, so omitting is unambiguous.
	Status string `json:"status,omitempty"`
	// Children deliberately has NO omitempty, unlike Depth/Status/
	// AgentType/Result/FailReason just above and below — a live review
	// finding on an earlier revision's fix: giving it omitempty (to stop
	// the cold-fallback branch's old []string{} from lying "zero
	// children") went one step too far, since a Go slice's omitempty
	// collapses nil AND a genuinely empty non-nil slice to the exact
	// same "field absent" wire shape. That made a WARM, truly childless
	// node ALSO omit the field, indistinguishable from "unknown" — the
	// very ambiguity omitempty was supposed to close. A caller polling
	// the same session as it transitions between these states would see
	// the field flicker between present and absent with no way to tell
	// "known: zero" from "don't know" from the flicker alone.
	//
	// The fix: no omitempty, and lineageJSONFor's childIDsUnion helper
	// guarantees a non-nil (possibly empty) slice on the WARM branch — see
	// that helper's own doc comment. On the warm branch, Children has no
	// "genuinely unknown" wire state to distinguish at all:
	// sess.SpawnedChildIDs() (engine/store.go restores it on every
	// LoadSession, unconditionally, exactly like TaskParentID/
	// TaskAgentType) is cross-checked against info.Children (this
	// process's OWN live adoption history), so an empty result there is
	// trustworthy — a live audit finding on an EARLIER version of this
	// fix: the cold-fallback branch used to report Children as nil
	// ("unknown") even though SpawnedChildIDs had the real answer sitting
	// right there in sess's own already-loaded Config.
	//
	// The cold-fallback branch is a narrower story: a live review finding
	// on THAT earlier fix caught that SpawnedChildIDs() is a complete
	// answer only for a log written after recTaskSpawned records shipped
	// — a legacy log that genuinely did spawn children before that record
	// existed has an empty SpawnedChildIDs() indistinguishable from a
	// parent that truly never spawned anything, and the cold branch has
	// no live tree to cross-check against the way the warm branch does.
	// So an empty result there IS reported as genuinely unknown (nil,
	// `"children":null`) again — only a NON-empty SpawnedChildIDs() is
	// trustworthy regardless of log vintage, since a durably recorded
	// child is a durably recorded child no matter how old the log. Three
	// wire shapes in total: `"children":null` (cold fallback ONLY,
	// genuinely unknown — the warm branch never produces this),
	// `"children":[]` (known: zero — the warm branch only; the cold
	// branch never produces this shape, since an empty durable list there
	// is exactly the unknown case above), and `"children":["..."]`
	// (known: non-zero, either branch).
	Children  []string `json:"children"`
	AgentType string   `json:"agent_type,omitempty"`
	// Result is the final assistant text for a done session; FailReason a
	// classified reason for a failed one — a fixed prefix plus the masked,
	// capped provider cause (engine's classifySpawnFailure, and the #82 leak
	// rule its doc comment states). Both empty otherwise.
	Result     string `json:"result,omitempty"`
	FailReason string `json:"fail_reason,omitempty"`
	// FailKind classifies FailReason for a control plane that must branch
	// rather than parse prose: "provider_exhausted" (engine's
	// FailKindProviderExhausted) means the provider ACCOUNT is walled, so
	// the child is intact and re-runnable and a replacement child would
	// hit the same wall. Empty for an ordinary failure, and — like Status
	// — always empty on the cold-fallback branch, which has no durable
	// source for a SessionManager-only field.
	FailKind string `json:"fail_kind,omitempty"`
}

// usageJSON is the Session/StatusEntry usage sub-object (issue #62 layer 2):
// cumulative token usage the engine already tracks (engine.Session.Usage),
// plus message count and, when the engine can derive it cheaply, the most
// recent turn's input tokens (engine.Session.LastUsage /
// engine.SessionInfo.LastInputTokens).
type usageJSON struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	Messages         int `json:"messages"`
	// LastInputTokens is the input-token count of the most recent completed
	// turn, omitted (zero) until at least one turn has completed.
	LastInputTokens int `json:"last_input_tokens,omitempty"`
}

// usageJSONForSession builds usageJSON from a fully loaded/resident
// engine.Session — used by buildSession (GET /session/{id}, GET /session)
// and by handleStatus's resident branch.
func usageJSONForSession(sess *engine.Session) usageJSON {
	u := sess.Usage()
	out := usageJSON{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Messages:         len(sess.History()),
	}
	if last, ok := sess.LastUsage(); ok {
		out.LastInputTokens = last.InputTokens
	}
	return out
}

// usageJSONForInfo builds usageJSON from a cheap engine.SessionInfo — the
// non-resident branch of GET /session/status, where paying for a full
// LoadSession per listed session would defeat the point of a lightweight
// status endpoint.
func usageJSONForInfo(info engine.SessionInfo) usageJSON {
	return usageJSON{
		InputTokens:      info.Usage.InputTokens,
		OutputTokens:     info.Usage.OutputTokens,
		CacheReadTokens:  info.Usage.CacheReadTokens,
		CacheWriteTokens: info.Usage.CacheWriteTokens,
		Messages:         info.Messages,
		LastInputTokens:  info.LastInputTokens,
	}
}

// lastTurnJSON is the openapi LastTurn shape.
type lastTurnJSON struct {
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// compositeState resolves the unambiguous Session.state field: goal-running
// whenever a goal is active, regardless of the momentary running/busy flag
// (see sessionJSON.State's doc comment for why momentary busy/idle is not
// enough); otherwise busy or idle mirroring the plain status.
//
// forceIdle (see goalTracker.pauseView and forcesIdlePause below) overrides
// the goalActive branch above for the two pause reasons whose loop has
// genuinely stopped driving the goal — "restart" (a goal restored from the
// journal at boot with no loop ever attached) and "worker_failure" (Task 2:
// a worker turn exit-parked the goal, and PursueGoal has actually returned —
// no goroutine is running it until the next auto-arm or re-POST). Neither is
// "goal-running" in any sense an operator or composer can act on — it will
// never progress on its own, and "busy"/"goal-running" forever is exactly
// the operator trap this field exists to close (see docs/design/
// fleet-model.md's ADOPT lifecycle). A provider-backoff pause deliberately
// does NOT take this path — its loop is genuinely alive and running, just
// waiting out provider weather, so it keeps reading goal-running (see
// TestGoalStalledProviderBackoffSurfacesPaused).
//
// forceIdle does NOT override running, though: it means no loop is driving
// the GOAL, not that no TURN is running — an ordinary prompt (or the plain
// resume prompt that eventually re-arms the goal) can be actively streaming
// while the goal itself sits parked, and "idle" would be a lie in that
// window (incident: an operator watching the monitor concluded a box was
// dead while it was mid-turn). So forceIdle&&running reads "busy", never
// "goal-running" — goalActive must not win here either, that's exactly the
// zombie-goal trap forceIdle exists to close — and only forceIdle&&!running
// reads "idle" (see TestCompositeStateForceIdleNeverMasksRunningTurn and
// TestCompositeStateBusyDuringForcedIdlePause).
func compositeState(running, goalActive, forceIdle bool) string {
	switch {
	case forceIdle:
		if running {
			return "busy"
		}
		return "idle"
	case goalActive:
		return "goal-running"
	case running:
		return "busy"
	default:
		return "idle"
	}
}

// forcesIdlePause reports whether goal represents a pause reason whose loop
// has genuinely stopped driving the goal — "restart" (see
// pauseArmedGoalsAtBoot) or "worker_failure" (see goalTracker.pausedWorker)
// — the two pause reasons that force compositeState to idle.
// "provider-backoff" deliberately returns false here: that loop is still
// alive, merely waiting. nil-safe.
func forcesIdlePause(goal *goalJSON) bool {
	return goal != nil && goal.Paused && (goal.PauseReason == pauseReasonRestart || goal.PauseReason == pauseReasonWorkerFailure)
}

// goalJSON is the Session.goal sub-object: present only when a goal has been
// set for the session in this process.
//
// Retryable/RetryableClass/Waiting mirror the most recent goal.stalled
// record's classification (see engine/goal.go and GitHub issue #61):
// Retryable is true when that stall was classified provider-retryable
// weather, RetryableClass names it, and Waiting is true while still inside
// the retryable budget ("waiting out provider weather") and false once
// that budget is exhausted (the loop is about to park a turn, not die).
// All three are reset by goal.set/goal.eval/goal.achieved, same as Attempt.
type goalJSON struct {
	Condition      string `json:"condition"`
	Active         bool   `json:"active"`
	Achieved       bool   `json:"achieved,omitempty"`
	Turns          int    `json:"turns"`
	LastReason     string `json:"last_reason,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	Retryable      bool   `json:"retryable,omitempty"`
	RetryableClass string `json:"retryable_class,omitempty"`
	Waiting        bool   `json:"waiting,omitempty"`
	// Paused/PauseReason present the "goal armed but nothing is driving it"
	// state (see goalTracker.pauseView): true with pause_reason "restart"
	// when this process booted and found the goal active with no loop ever
	// attached (see pauseArmedGoalsAtBoot); true with "worker_failure"
	// (Task 2) when a worker turn exit-parked the goal (engine/
	// goal.go's goal.parked) — the loop has genuinely exited, resumed only
	// by the next ordinary activity (maybeAutoArmGoal) or an operator
	// re-POST; true with "provider-backoff" while the retryable-backoff
	// park machinery (engine/goal.go) waits out provider weather — that
	// loop is still alive, merely waiting. All three clear on re-arm (POST
	// /session/{id}/goal) or, for provider-backoff, the moment the loop's
	// own retry succeeds.
	Paused      bool   `json:"paused,omitempty"`
	PauseReason string `json:"pause_reason,omitempty"`
	// EvalFailures is the most recent goal.eval_failed record's consecutive
	// failure count (see engine/goal.go's "Round 6" doc section
	// and goalTracker.evalFailures): rises with each failed evaluator
	// boundary below goalEvalFailureLimit and resets to 0 on goal.set,
	// goal.eval, goal.achieved, goal.cleared, or goal.updated. Omitted
	// (zero) whenever no boundary has failed since the last reset.
	EvalFailures int `json:"eval_failures,omitempty"`
}

// goalJSONFrom builds the goalJSON wire shape from a per-session goal
// tracker, deriving the paused presentation via pauseView — the single
// construction path shared by buildSession and waitSnapshot so the two can
// never drift on this. Returns nil for a nil tracker (no goal ever set).
func goalJSONFrom(g *goalTracker) *goalJSON {
	if g == nil {
		return nil
	}
	paused, reason := g.pauseView()
	return &goalJSON{
		Condition:      g.condition,
		Active:         g.active,
		Achieved:       g.achieved,
		Turns:          g.turns,
		LastReason:     g.lastReason,
		Attempt:        g.attempt,
		Retryable:      g.retryable,
		RetryableClass: g.retryableClass,
		Waiting:        g.waiting,
		Paused:         paused,
		PauseReason:    reason,
		EvalFailures:   g.evalFailures,
	}
}

// sessionIDOrNotFound extracts {id} from the request path and validates it
// with engine.ValidSessionID (legacy "ses_" + 16 hex, or a well-formed "ses"
// TypeID), writing 404 and returning ok=false otherwise. Every handler
// keyed by {id} must call this before touching the session directory or
// s.sessions: net/http's ServeMux splits routing segments on the RAW,
// still-percent-encoded path, so a single segment spelled "..%2fleaked"
// matches "/session/{id}" and PathValue decodes it to "../leaked" — parsing
// at this boundary, rather than trusting whatever came back from
// os.ReadFile/filepath.Join, is what keeps that from escaping SessionDir.
func (s *Server) sessionIDOrNotFound(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !engine.ValidSessionID(id) {
		writeErr(w, http.StatusNotFound, "no such session")
		return "", false
	}
	return id, true
}

// rejectManagedChildTurn refuses id if it names a session SessionManager
// tracks as a CHILD — see handleSessionSend's own doc comment:
// SessionManager is a child's SOLE scheduler. The generic per-{id} routes
// that can drive a turn or persist a durable record directly against
// whatever *engine.Session claimForPrompt (or an equivalent cold-load)
// hands them — prompt_async, goal, enqueue, compact, model, thinking —
// have no notion of that at all. Without this guard, a request against a
// child's id cold-loads a SECOND, independent *engine.Session for the SAME
// on-disk log and drives Session.Prompt (or persists a recModel/recEffort
// record) on it CONCURRENTLY with the child's own Spawn-driven turn on the
// FIRST object — both appending to the same session log at once, the
// exact "never call Prompt concurrently with itself" contract violation
// ExternalRunner exists to prevent for roots, left wide open for children
// (which get addressable ids from handleSpawnChild's 201 and
// session.info's lineage). A live review caught this.
//
// "Is a managed CHILD" is decided on sess.TaskParentID() != "" — the
// DURABLE signal, restored by LoadSession unconditionally — never
// info.ParentID, the LIVE tree pointer. A live review finding: an earlier
// revision of this guard checked info.ParentID, which adoptReloadedLocked
// leaves EMPTY for a warm orphan (a genuine managed child adopted while
// its own parent was untracked — see that method's own doc comment and
// lineageJSONFor's identical ParentID fallback just above in this file).
// A warm orphan slipped through the old check entirely, letting exactly
// the concurrent-Session corruption this guard exists to prevent happen
// to precisely the child shape it was least equipped to protect.
//
// Returns true (having already written a 409) if id is a managed child
// and the caller must stop; false — safe to proceed through the ordinary
// root/untracked path — otherwise. A residual gap: this only protects a
// child SessionManager has ALREADY adopted into THIS process's tree
// (Spawn, or a prior session.send/task touching it) — an id that is
// really a child but has never been adopted here yet (e.g. a fresh
// process, or one already Reaped) is not caught, the same
// task-on-reload-adoption boundary this PR's SessionManager.
// adoptReloadedLocked already documents elsewhere.
func (s *Server) rejectManagedChildTurn(w http.ResponseWriter, id string) bool {
	if sess, ok := s.sessMgr.Session(id); ok && sess.TaskParentID() != "" {
		writeErr(w, http.StatusConflict, "session is a SessionManager-managed child session; use POST /session/{id}/send instead")
		return true
	}
	return false
}

// healthJSON is the openapi Health shape. VCSRevision, VCSTime, SessionSync,
// and StartedAt are always present (never omitted, even empty) so a client
// never has to special-case "field absent" vs "field empty" — see buildInfo
// and handleHealth. /health is deliberately unauthenticated (see Options.
// RunToken's doc comment), and every field here is as low-sensitivity as the
// Version string it has always reported: they exist precisely so a canary
// can machine-check "what engine, what durability mode, since when" against
// a running box with no token and no session, the /health counterpart to
// the ambient in-session engine-identity block (see engine/
// identity_status.go) an agent already gets for free every turn.
type healthJSON struct {
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
	// SessionSync is the EFFECTIVE session-durability mode (see
	// effectiveSessionSync) — never the raw, possibly-empty Options.
	// SessionSync value — so a canary never has to know the zero value's
	// meaning to know the mode a box is actually running in.
	SessionSync string `json:"session_sync"`
	// StartedAt is this server process's start time (Options.StartedAt),
	// rendered as an RFC3339 UTC timestamp, or "" when Options.StartedAt was
	// never set (e.g. a test harness that doesn't care about it).
	StartedAt string `json:"started_at"`
}

// pseudoVersionRe matches the trailing "<14-digit-UTC-timestamp>-
// <12-hex-commit>" suffix of a Go module pseudo-version (see
// golang.org/ref/mod#pseudo-versions), e.g. the "20240102150405-
// abcdef012345" tail of "v0.0.0-20240102150405-abcdef012345" — the shape
// `go install pkg@<sha>` produces when the target commit carries no semver
// tag, and parseModuleVersion's primary case. Anchored at the end of the
// string; deliberately does NOT require the timestamp to be immediately
// preceded by "-", so the same pattern also matches a tagged pre-release
// pseudo-version like "v1.2.4-0.20240102150405-abcdef012345" (the "0."
// prefix landing just before the timestamp there).
var pseudoVersionRe = regexp.MustCompile(`(\d{14})-([0-9a-f]{12})$`)

// parseModuleVersion extracts a VCS revision and commit time from a Go
// module version string (debug.BuildInfo.Main.Version), for buildInfo's
// fallback when ReadBuildInfo carries no vcs.revision setting at all — the
// case for every `go install pkg@sha`-style module-mode build, since Go
// only embeds vcs.* settings when the build reads them from a local .git
// checkout, never from a downloaded module (see buildInfo's doc comment).
// Two recognized shapes:
//
//   - A pseudo-version (see pseudoVersionRe): its trailing timestamp-commit
//     suffix embeds exactly the two pieces vcs.revision/vcs.time would have
//     carried, so this returns the 12-hex commit as revision and the
//     timestamp reformatted as RFC3339 UTC as buildTime — vcs_revision is
//     therefore meaningful under `go install pkg@<sha>` too, not just a
//     local .git build.
//   - Any other non-empty, non-"(devel)" version (a real tag, e.g. a module
//     installed by "@v1.2.3" rather than by commit): returned in revision
//     as-is, with buildTime left empty since a bare tag names no specific
//     commit time.
//
// "(devel)" (what debug.ReadBuildInfo reports for a plain `go build`/`go
// run` outside module-download mode) and "" both return ("", ""): neither
// names any commit at all.
func parseModuleVersion(version string) (revision, buildTime string) {
	if version == "" || version == "(devel)" {
		return "", ""
	}
	if m := pseudoVersionRe.FindStringSubmatch(version); m != nil {
		if ts, err := time.Parse("20060102150405", m[1]); err == nil {
			return m[2], ts.UTC().Format(time.RFC3339)
		}
	}
	return version, ""
}

// buildInfo reads the running binary's VCS revision and commit time from
// runtime/debug.ReadBuildInfo, so GET /health can identify exactly which
// commit is live — a stale box binary otherwise looks identical to a fresh
// one behind a fixed config Version string (an engineer once burned 30
// minutes suspecting exactly that). Both return "" when build info is
// unavailable, which is the ordinary case for a `go test` binary —
// ReadBuildInfo still succeeds there, it simply has no "vcs.revision"/
// "vcs.time" entries in Settings AND no usable Main.Version (a test binary's
// Main.Version is also "(devel)"). When Settings carries no vcs.revision —
// the ordinary case for a `go install pkg@sha`-style module-mode install,
// which embeds no local .git at all — this falls back to parseModuleVersion
// against info.Main.Version instead of returning empty, so vcs_revision
// stays meaningful under every install method, not just a build from a
// local checkout.
func buildInfo() (revision, buildTime string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.time":
			buildTime = setting.Value
		}
	}
	if revision == "" {
		revision, buildTime = parseModuleVersion(info.Main.Version)
	}
	return revision, buildTime
}

// effectiveSessionSync normalizes a raw Options.SessionSync value to the
// mode it actually behaves as: any value other than "volume" (including the
// zero value) is fsync mode. Mirrors engine's identityStatusSegment pairing
// (engine/identity_status.go's effectiveSessionSync, which in turn mirrors
// Session.volumeSync in engine/store.go) for the ambient in-session block —
// kept as a small local copy rather than an import, since that engine
// helper is unexported; the two are deliberately identical in behavior and
// SHOULD be changed together if the default ever does.
func effectiveSessionSync(mode string) string {
	if mode == engine.SessionSyncVolume {
		return engine.SessionSyncVolume
	}
	return engine.SessionSyncFsync
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	rev, t := buildInfo()
	var startedAt string
	if !s.opts.StartedAt.IsZero() {
		startedAt = s.opts.StartedAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, healthJSON{
		Version:     s.opts.Version,
		VCSRevision: rev,
		VCSTime:     t,
		SessionSync: effectiveSessionSync(s.opts.SessionSync),
		StartedAt:   startedAt,
	})
}

// monitorContentSecurityPolicy hardens the embedded monitor page, mirroring
// tools/hub/hub.go's contentSecurityPolicy (same reasoning: a single inline
// file with no build step, so a per-response nonce/hash is not viable —
// 'unsafe-inline' is the only way to permit its own inline script/style)
// adapted to what THIS page actually needs when served from the box it
// monitors: connect-src 'self', not '*'. The hub's page is deliberately
// origin-agnostic (it drives arbitrary, operator-added box origins it keeps
// no state about); the embedded monitor is the opposite — GET /monitor
// exists specifically so a box can offer a same-origin, zero-CORS way to
// watch ITSELF (see docs/development-interfaces.md's "Session monitor"
// section), so this CSP
// scopes fetch/EventSource targets to that one origin, blocking it from
// being used to reach anywhere else even if the served copy were somehow
// pointed at a different base URL. An operator who genuinely wants
// cross-origin monitoring still has the unrestricted, unauthenticated
// file://-or-any-static-host path index.html's own header comment
// documents — this route is additive, not a replacement.
const monitorContentSecurityPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// handleMonitor serves the embedded tools/monitor/index.html verbatim at
// GET /monitor (and /monitor/ — see routes()), registered only when
// Options.MonitorPage is non-nil (cmd/harness's serveCmd is the only
// caller that sets it, via tools/monitor.Page). Deliberately UNAUTHENTICATED
// — see MonitorPage's own doc comment for why that is correct here — and,
// unlike every other handler in this file, reached WITHOUT s.auth wrapping
// it (see routes()). GET or HEAD only, matching tools/hub/hub.go's
// handleIndex precedent for its own embedded page.
func (s *Server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", monitorContentSecurityPolicy)
	// no-cache: the page is embedded at build time, so a box that respawns
	// on a newer image serves a newer page — but browsers heuristically
	// cache an HTML response with no freshness headers, so an operator who
	// revisits the same box URL would keep seeing a stale page across a pin
	// bump (or a local dev rebuild) until a hard reload. no-cache forces a
	// revalidation on every load; the page itself is a few KB, so the cost
	// is negligible and correctness (always the current build) wins.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		w.Write(s.opts.MonitorPage) //nolint:errcheck
	}
}

// handleRoot 302-redirects the bare root path to the canonical monitor URL
// at /monitor, so visiting a box's host with no path lands on the monitor
// instead of a bare 404. Registered only when Options.MonitorPage is non-nil
// (see routes(), which anchors it with the GET /{$} pattern so it matches "/"
// EXACTLY — a plain "GET /" would be a catch-all swallowing every otherwise-
// unmatched path's 404 and redirecting it here instead). A pure-API box that
// never sets MonitorPage keeps / as a clean 404, not a redirect to a route it
// doesn't serve. Unauthenticated and outside s.auth, same as handleMonitor:
// the redirect carries no secret, and a browser preserves any #t=<token>
// fragment across the 302 (the target carries none of its own — see
// tools/monitor's capability-URL handling), so the capability flow still
// works from the bare host. 302 (not 301) keeps / uncacheable as a permanent
// alias: /monitor is the one canonical URL (printed by monitorTerminalHint,
// carried in the capability link), and / stays a convenience we're free to
// repurpose later.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/monitor", http.StatusFound)
}

// handleGoroutines writes the full, all-goroutine stack dump (the exact
// text Go's default SIGQUIT handler prints) as a diagnostic HTTP surface —
// for a box wedged badly enough that even exec is awkward (or unavailable,
// e.g. a managed sandbox with no shell access), this gets the same picture
// SIGQUIT would give over authed HTTP instead. It is registered behind
// s.auth like every other route (see routes()), deliberately NOT under
// net/http/pprof's default mux registration (which would also register
// /debug/pprof/* unauthenticated on http.DefaultServeMux as an import side
// effect) — this calls runtime/pprof directly against the existing mux
// instead, so the only new surface is this one explicit, authed route.
// debug=2 (not the default 1) is what makes the output match SIGQUIT's own
// format: full stack traces in the panic-style layout, not pprof's
// symbolized-count summary.
func (s *Server) handleGoroutines(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Lookup("goroutine") is always non-nil (a predefined profile registered
	// by the runtime itself), so no nil check is needed here.
	pprof.Lookup("goroutine").WriteTo(w, 2) //nolint:errcheck // best-effort diagnostic write; nothing to do with a failure once headers are sent
}

// maxParentSessionLen bounds POST /session's optional parent_session field:
// an opaque provenance pointer, not a session ID this server necessarily
// knows about (lineage may cross boxes — see engine.Config.ParentSession),
// so the only sane validation is a length cap against an accidental
// pasted-blob value, not a format check.
const maxParentSessionLen = 128

// validateParentSession validates POST /session's optional parent_session
// field: nil (the key was omitted) is valid and returns "", no error. A
// present value must be non-empty and at most maxParentSessionLen bytes;
// either violation is a 400. It is deliberately NOT required to name a
// session that exists on this server, or anywhere — see
// engine.Config.ParentSession's doc comment.
func validateParentSession(v *string) (string, error) {
	if v == nil {
		return "", nil
	}
	if *v == "" {
		return "", errors.New("parent_session: must be non-empty when present")
	}
	if len(*v) > maxParentSessionLen {
		return "", fmt.Errorf("parent_session: exceeds maximum length of %d bytes", maxParentSessionLen)
	}
	return *v, nil
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	handlerStart := time.Now()
	var body struct {
		Model        message.ModelRef `json:"model"`
		WorkDir      string           `json:"workdir"`
		ShareWorkdir bool             `json:"share_workdir"`
		// WorkdirIsolation is "shared" (default, omitted/empty) or
		// "worktree" — see createWorktreeForSession and workdirHolderLocked.
		WorkdirIsolation string `json:"workdir_isolation"`
		// ParentSession is an opaque provenance pointer to the session this
		// one continues from (see engine.Config.ParentSession's doc
		// comment). Optional; validated by validateParentSession below.
		ParentSession *string `json:"parent_session"`
		// ParentID, Agent, and Prompt are session.create's "with a parent"
		// form (design doc, Stage 4): "identical to a task call made from
		// outside the model." A COMPLETELY DIFFERENT concept from
		// ParentSession above — see lineageJSON's doc comment — this names
		// a live session in THIS process's SessionManager tree. When
		// ParentID is set, every other field in this body is ignored: see
		// handleSpawnChild, which takes over entirely and shares none of
		// the workdir/worktree/residency machinery below.
		ParentID *string `json:"parent_id"`
		Agent    string  `json:"agent"`
		Prompt   string  `json:"prompt"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ParentID != nil && strings.TrimSpace(*body.ParentID) != "" {
		s.handleSpawnChild(w, *body.ParentID, body.Agent, body.Prompt, body.Model)
		return
	}
	parentSession, err := validateParentSession(body.ParentSession)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	isolation := body.WorkdirIsolation
	if isolation == "" {
		isolation = isolationShared
	}
	if isolation != isolationShared && isolation != isolationWorktree {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("workdir_isolation: unknown value %q (want \"shared\" or \"worktree\")", body.WorkdirIsolation))
		return
	}
	workDir, err := resolveWorkDir(s.opts.WorkspaceRoots, body.WorkDir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// sessionWorkDir is what the session's tools actually run in: workDir
	// itself for 'shared', or a dedicated git worktree checked out from it
	// for 'worktree'. wt is nil for 'shared'.
	sessionWorkDir := workDir
	var wt *worktreeInfo
	if isolation == isolationWorktree {
		wt, err = s.createWorktreeForSession(workDir)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sessionWorkDir = wt.path
	}

	phaseStart := time.Now()
	sess, err := s.opts.NewSession(body.Model, sessionWorkDir, parentSession)
	if err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	s.reportCreatePhase(sess.ID, "new_session", time.Since(phaseStart))
	// Refuse a model with no known context window at CREATE time, before
	// the session becomes durable or resident: a session that can never
	// run one Prompt is worse than a 400, because it looks created. The
	// session object is simply dropped — nothing has been journaled or
	// registered for it yet. See engine.Config.RequireContextWindow.
	if err := sess.ContextWindowErr(); err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Report "total" on every return past this point — success or error —
	// not just the success tail below. Without this, a failure after
	// new_session (recordWorktreeOwner, Persist) never reports "total", and
	// the cmd-layer accumulator that keys phases by session ID (see
	// cmd/harness/main.go's createPhaseLogger) leaks that entry forever — a
	// saturated storage volume is precisely what makes Persist fail or stall
	// on every create. Reporting "total" on the error path is also strictly
	// better diagnostics: it is the one phase report that survives a failed
	// create, showing which prior phase ran (and how slowly) before the
	// failure.
	defer func() {
		s.reportCreatePhase(sess.ID, "total", time.Since(handlerStart))
	}()
	if wt != nil {
		// Record the owning session in the meta BEFORE the session log
		// becomes durable, and fail creation if it cannot be recorded. The
		// order is load-bearing: the startup sweep keys entirely on
		// sessionResumable(meta.SessionID), so the moment Persist succeeds
		// the meta must already name the owner — a crash between "log on
		// disk" and "owner recorded" would leave a resumable session whose
		// meta has no SessionID, and the sweep would reap its live worktree
		// out from under it. With this order a crash instead leaves a meta
		// whose session log does not exist yet: never resumable, safely
		// adjudicated clean/dirty like any abandoned worktree.
		if err := s.recordWorktreeOwner(wt, sess.ID); err != nil {
			s.discardWorktree(wt)
			writeErr(w, http.StatusInternalServerError, "cannot create session")
			return
		}
	}
	// Persist the log now so the session has durable state even if it is
	// evicted before its first prompt; otherwise eviction below would drop a
	// never-prompted session with no on-disk backing to reload from.
	if err := s.timedCreatePhase(sess.ID, "persist", sess.Persist); err != nil {
		if wt != nil {
			s.discardWorktree(wt)
		}
		writeErr(w, http.StatusInternalServerError, "cannot create session")
		return
	}
	// Adopt into the SessionManager so `task` is available and this session
	// is reachable by session.info's lineage extension and session.send —
	// see Server.sessMgr's doc comment. Deliberately AFTER Persist, not
	// right after NewSession: AdoptRoot registers a root sessionNode that
	// Reap NEVER removes (a root is the tree's own address — see Reap's
	// doc comment), so adopting before the fallible recordWorktreeOwner/
	// Persist steps above leaked one root node plus its full *Session per
	// failed create, forever — a live review finding. By the time this
	// line runs, both fallible steps have already succeeded, so there is
	// no error path left past this point that could strand it. Errors
	// only on an ID collision (astronomically unlikely — see
	// engine.newID) or a double-adopt; both are safe to ignore here
	// rather than fail session creation over a purely additive
	// capability.
	_ = s.sessMgr.AdoptRoot(sess)

	s.timedCreatePhase(sess.ID, "register", func() error { //nolint:errcheck // never errors; see timedCreatePhase's uniform shape
		s.mu.Lock()
		s.sessions[sess.ID] = &sessionState{sess: sess, lastUsed: time.Now(), shareWorkdir: body.ShareWorkdir, isolation: isolation, worktree: wt}
		evicted := s.evictResidentLocked()
		s.mu.Unlock()
		releaseEvicted(evicted)
		return nil
	})

	s.timedCreatePhase(sess.ID, "emit_created", func() error { //nolint:errcheck // never errors; see timedCreatePhase's uniform shape
		s.emitDurable(Event{Type: evtSessionCreated, SessionID: sess.ID, Model: sess.Model()})
		return nil
	})

	writeJSON(w, http.StatusCreated, s.buildSession(s.resolveLive(sess.ID).withLoaded(sess)))
}

// reportCreatePhase forwards elapsed to Options.OnCreatePhase, nil-guarded.
// See its doc comment for the reported phases and per-phase end contract.
func (s *Server) reportCreatePhase(sessionID, phase string, elapsed time.Duration) {
	if s.opts.OnCreatePhase != nil {
		s.opts.OnCreatePhase(sessionID, phase, elapsed)
	}
}

// reportCreatePhaseStart forwards to Options.OnCreatePhaseStart, nil-guarded.
// See its doc comment for which phases are covered (persist/register/
// emit_created — not new_session, not total).
func (s *Server) reportCreatePhaseStart(sessionID, phase string) {
	if s.opts.OnCreatePhaseStart != nil {
		s.opts.OnCreatePhaseStart(sessionID, phase)
	}
}

// timedCreatePhase runs fn as one instrumented create phase:
// reportCreatePhaseStart immediately before fn runs, reportCreatePhase
// immediately after fn returns — success OR error — with the elapsed time fn
// actually took either way. Mirrors engine.Session.timedStorePhase
// (engine/store.go) one layer up, for the identical reason: a hand-paired
// start-then-later-report around each phase let an early `return` on the
// phase's own failure (e.g. Persist erroring) skip the matching report,
// leaving a watchdog's in-flight table (see cmd/harness/main.go) with an
// entry nothing would ever clear — a permanent false "still stuck" warning
// for a phase that in fact failed and returned promptly. Every phase site in
// handleCreate that calls reportCreatePhaseStart at all routes through this
// one helper so that invariant is structural, not a rule each call site has
// to remember.
func (s *Server) timedCreatePhase(sessionID, phase string, fn func() error) error {
	s.reportCreatePhaseStart(sessionID, phase)
	t0 := time.Now()
	err := fn()
	s.reportCreatePhase(sessionID, phase, time.Since(t0))
	return err
}

// createWorktreeForSession validates that workDir is inside a git repository
// and creates a dedicated, detached-HEAD worktree for a new 'worktree'
// isolation session (see addWorktree). Its error message is written directly
// into the 400 response body, so it names the actual problem (not inside a
// git repository vs. the underlying git failure) rather than a generic
// "cannot create session".
func (s *Server) createWorktreeForSession(workDir string) (*worktreeInfo, error) {
	repoRoot, ok, err := gitRepoRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("workdir_isolation=worktree requires workdir %q to be inside a git repository", workDir)
	}
	base, err := s.worktreeBaseDir()
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	id := newWorktreeID()
	path := filepath.Join(base, id)
	// Write a provisional meta BEFORE addWorktree runs: path and repoRoot
	// are already known, and BaseCommit/SessionID are patched in once they
	// exist (the writeWorktreeMeta call below, and recordWorktreeOwner
	// after the session is minted). This closes the crash window that used
	// to exist between addWorktree succeeding and the meta actually landing
	// on disk — a real git worktree with zero bookkeeping, invisible to
	// sweepWorktrees (which only ever reads meta/*.json) and leaked
	// forever. A meta whose worktree directory doesn't exist yet is
	// something sweepWorktrees already knows how to prune.
	metaPath, err := writeWorktreeMeta(base, id, worktreeMeta{RepoRoot: repoRoot, Path: path})
	if err != nil {
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	baseCommit, err := addWorktree(repoRoot, path)
	if err != nil {
		os.Remove(metaPath) // best effort: no worktree was ever created, nothing for the sweep to adjudicate
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	// metaPath is deterministic in base+id alone, so it's unchanged by this
	// second write; only its content (BaseCommit) gets patched in.
	if _, err := writeWorktreeMeta(base, id, worktreeMeta{RepoRoot: repoRoot, Path: path, BaseCommit: baseCommit}); err != nil {
		_ = removeWorktree(repoRoot, path)
		os.Remove(metaPath)
		return nil, fmt.Errorf("workdir_isolation=worktree: %w", err)
	}
	return &worktreeInfo{id: id, base: base, path: path, repoRoot: repoRoot, baseCommit: baseCommit, metaPath: metaPath}, nil
}

// recordWorktreeOwner patches wt's meta file with the now-known session ID,
// once NewSession has actually minted one.
func (s *Server) recordWorktreeOwner(wt *worktreeInfo, sessionID string) error {
	_, err := writeWorktreeMeta(wt.base, wt.id, worktreeMeta{
		SessionID:  sessionID,
		RepoRoot:   wt.repoRoot,
		Path:       wt.path,
		BaseCommit: wt.baseCommit,
	})
	return err
}

// discardWorktree removes a just-created worktree when session construction
// fails after it: nothing has been journaled or made resident yet, so there
// is no "kept" record to worry about — a bare best-effort removal (falling
// back to leaving it, never forcing) is enough.
func (s *Server) discardWorktree(wt *worktreeInfo) {
	if err := removeWorktree(wt.repoRoot, wt.path); err == nil {
		os.Remove(wt.metaPath)
	}
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	type snap struct {
		sess    *engine.Session
		running bool
	}
	s.mu.Lock()
	mem := make([]snap, 0, len(s.sessions))
	for _, st := range s.sessions {
		mem = append(mem, snap{st.sess, st.running})
	}
	s.mu.Unlock()

	out := []sessionJSON{}
	seen := make(map[string]bool)
	for _, m := range mem {
		// Built from the bulk residency read above rather than a second
		// per-session s.mu hold: m.sess and m.running already come from
		// ONE hold, which is the pairing rule liveSession needs. Only the
		// manager half (for the lineage block) is read here.
		out = append(out, s.buildSession(liveFromResident(m.sess.ID, m.sess, m.running).withManager(s.sessMgr)))
		seen[m.sess.ID] = true
	}
	// Ids first, indexes second. A session this loop renders from a live
	// object needs no index at all, and reading one for it is work thrown
	// away — and a stale sidecar would be refolded and written back here
	// while that session's own writer holds it.
	ids, err := engine.ListSessionIDs(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		// resolveLive first, index second. An id on disk can still be live
		// in this process — a Spawn-driven child is never a residency key,
		// so it lands in this branch, and its status and lineage must come
		// from SessionManager's own node. Only an id nothing live holds is
		// rendered from its index, which is the case this listing used to
		// pay a full LoadSession for, per session, on every call.
		lv := s.resolveLive(id)
		if lv.session() != nil {
			out = append(out, s.buildSession(lv))
			continue
		}
		ix, ixErr := engine.ReadSessionIndex(s.opts.SessionDir, id)
		// A session neither the index nor a load can render is omitted
		// here, while GET /session/status still reports its usage from a
		// direct journal scan. That asymmetry predates this index — see
		// TestListOmitsWhatItCannotRenderWhileStatusReportsIt — and it is
		// what the two endpoints promise: a listing entry names a session's
		// model, workdir, and lineage, which a journal that will not load
		// cannot supply, and GET /session/{id} 404s for the same session.
		// Status promises only counts, which a scan can still give.
		//
		// coldSessionJSON for BOTH cases, index-backed and load-backed. It
		// renders from the index when that index can answer, falls back to
		// the authoritative load path when it cannot (see
		// SessionIndex.Complete), and re-checks residency at the end. The
		// re-check is why this listing does not build from the index
		// directly: claimForPrompt can make a session live between the
		// resolveLive above and this call, and GET /session/{id} closes
		// that window the same way. The two must not disagree about a
		// session's liveness. A session neither path can render is skipped,
		// exactly as this listing always has.
		if body, ok := s.coldSessionJSON(id, ix, ixErr == nil && ix.Complete); ok {
			out = append(out, body)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

// handleGet answers GET /session/{id}.
//
// A session any live source in this process holds — this server's own
// residency map or SessionManager's tree — is rendered from that live
// object, unchanged. Anything else is rendered from its metadata index
// (engine.ReadSessionIndex): one small sidecar read, no journal replay.
// That cold path used to call engine.LoadSession, decode every message
// body, rebuild the whole history, and then throw all of it away to report
// a dozen scalar fields — about 7 s per read on the fleet's longest session
// (see docs/design/console-read-path.md in meetneptune/boxes).
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	lv := s.resolveLive(id)
	if lv.session() != nil {
		writeJSON(w, http.StatusOK, s.buildSession(lv))
		return
	}
	ix, err := engine.ReadSessionIndex(s.opts.SessionDir, id)
	if body, ok := s.coldSessionJSON(id, ix, err == nil && ix.Complete); ok {
		writeJSON(w, http.StatusOK, body)
		return
	}
	writeErr(w, http.StatusNotFound, "no such session")
}

// coldSessionJSON renders a session no live source holds, from the index
// the caller already has, and falls back to a full load when that index
// cannot answer.
//
// The caller passes the index it holds rather than an id to re-read: GET
// /session has one per session already, and re-reading would cost a second
// stat and sidecar read per session in a listing. usable is false when the
// caller has no index at all, or holds one that is not Complete.
//
// The fallback is not a safety net, it is a correctness rule. A journal
// does not always record every field a reader needs: a legacy header
// carries no workdir, and a crash can tear away the initial model record a
// fresh log writes beside its header. engine.LoadSession answers those from
// the loading Config; a fold has no Config, and reports
// SessionIndex.Complete false rather than an empty model. Then the
// authoritative path answers, exactly as it did before this index existed.
//
// It also re-checks residency at the end. resolveLive ran before the index
// read, so a concurrent claimForPrompt can make the session live in that
// gap, and reporting "idle" for a session this process is now running is
// the false-idle answer an orchestrator acts on. The re-check does not
// close the window — nothing can, without holding a lock across a disk read
// — but it narrows it to the width of one map lookup.
func (s *Server) coldSessionJSON(id string, ix engine.SessionIndex, usable bool) (sessionJSON, bool) {
	var body sessionJSON
	if usable {
		body = s.buildSessionFromIndex(ix)
	} else {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			return sessionJSON{}, false
		}
		// Keep it. Before this, the fallback threw the loaded session
		// away, so the NEXT read of the same session replayed the same
		// journal from byte 0 again — and GET /session is polled by a
		// control-plane activity probe every ~20s, forever. A box's
		// finished sub-agent sessions were therefore cold-replayed on that
		// cadence for the life of the process, which is the repeating
		// `reason=start` context-window line an operator sees
		// (logContextWindowArmed fires once per LoadSession). Retaining
		// makes it at most one replay per session per residency window.
		s.retainLoaded(id, sess)
		body = s.buildSession(liveSession{id: id}.withLoaded(sess))
	}
	if lv := s.resolveLive(id); lv.session() != nil {
		body = s.buildSession(lv)
	}
	return body, true
}

// retainLoaded makes a session a cold READ just loaded resident, so the
// next read of it does not replay the journal again.
//
// A read that mutates residency deserves its justification stated. The
// alternative is not "no mutation": it is an unbounded full journal replay
// on every poll of an endpoint built to be polled, which is strictly worse
// for the same session and for every other session sharing the process's
// disk. What is retained is bounded by exactly the same MaxResident budget
// every other loader lives under, and the retained session is idle
// (running/goalLoop both false), so it is immediately eviction-eligible on
// the very next evictResidentLocked sweep — a listing can displace a warm
// idle session, never a running one.
//
// The shape is claimForPrompt's and handleSetModel's, deliberately not a
// third variant: LoadSession has already run OUTSIDE s.mu (it hits disk),
// and this re-acquires the lock and defers to any resident that appeared
// while it was loading, so two *engine.Session instances for one log are
// never both retained. releaseEvicted runs after the unlock, as it must.
func (s *Server) retainLoaded(id string, sess *engine.Session) {
	s.mu.Lock()
	var evicted []*engine.Session
	if s.sessions[id] == nil {
		s.sessions[id] = &sessionState{sess: sess, lastUsed: time.Now()}
		evicted = s.evictResidentLocked()
	}
	s.mu.Unlock()
	releaseEvicted(evicted)
}

// messagePlaceholder substitutes for a resident message that fails to
// marshal (see handleMessages): it carries just enough to identify which
// message broke and why, without ever risking a second marshal failure
// itself (every field here is a plain string).
type messagePlaceholder struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	MarshalError string `json:"marshal_error"`
}

// handleMessages returns the session's full canonical message history.
//
// It marshals per-message rather than the whole slice at once: a single
// resident message that fails to marshal — e.g. a Reasoning part carrying a
// non-zero-length but invalid ProviderData entry, which
// message.Message.Normalize does not catch (see its doc comment) because it
// only scrubs zero-length entries — used to take the entire endpoint down
// with a 500 ("json: error calling MarshalJSON for type message.Parts"),
// exactly when the transcript view was most needed to diagnose the death
// (observed in production on ses_01kx453ewfedqrg7p3c64f8sca and
// ses_01kx453ev9ejattygpf7rbzptw). Now a message that fails to marshal is
// replaced with a messagePlaceholder carrying its ID, role, and the marshal
// error, and every other message in the response is unaffected: the
// endpoint always returns 200 with as much of the transcript as is actually
// renderable.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	if query.Has("before_seq") || query.Has("limit") {
		s.handleMessagePage(w, query, id)
		return
	}
	if query.Has("stream_from") {
		msgs, seq, ok := s.transcriptSyncedThrough(id)
		if !ok {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		writeJSON(w, http.StatusOK, transcriptJSON{
			Messages:   marshalMessages(msgs),
			StreamFrom: seq,
		})
		return
	}
	sess, ok := s.lookupSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	msgs := sess.History()
	writeJSON(w, http.StatusOK, marshalMessages(msgs))
}

// transcriptJSON is the ?stream_from=1 envelope: the session's whole message
// history PLUS the durable event-journal seq it is synced through (see
// transcriptSyncedThrough), so a client can open GET /event?from=<stream_from>
// immediately after this snapshot with no REPLAY window that can re-deliver
// or drop a message straddling the two reads — the "race-closed bootstrap"
// that closes the tail-load-versus-live-stream race behind the console's
// duplicate-render bug.
//
// A THIRD opt-in shape, alongside the bare-array default (no query
// parameter) and the before_seq/limit messagePageJSON page: a caller that
// never names stream_from keeps getting exactly what it always got, byte
// for byte.
type transcriptJSON struct {
	Messages   []json.RawMessage `json:"messages"`
	StreamFrom int64             `json:"stream_from"`
}

// marshalMessages renders messages for the wire, one at a time, replacing
// any that fails to marshal with a messagePlaceholder — see handleMessages'
// own doc comment for the production incident that rule exists for. It
// always returns a non-nil slice, so an empty history serializes as [].
func marshalMessages(msgs []message.Message) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		raw, err := json.Marshal(m)
		if err != nil {
			ph, phErr := json.Marshal(messagePlaceholder{
				ID:           m.ID,
				Role:         string(m.Role),
				MarshalError: err.Error(),
			})
			if phErr != nil {
				// messagePlaceholder is a plain string struct; this cannot
				// happen, but never let a placeholder failure reintroduce
				// the wholesale-500 this handler exists to prevent.
				ph = []byte(`{"id":"","role":"","marshal_error":"unmarshalable message and placeholder"}`)
			}
			raw = ph
		}
		out = append(out, raw)
	}
	return out
}

// messagePageJSON is the openapi MessagePage shape: one bounded page of a
// session's durable message sequence, plus where that page sits.
//
// The envelope is deliberately a DIFFERENT shape from the unparameterized
// response's bare array. A client that pages needs the page's position, and
// a client that does not page must keep working byte for byte — so the
// array response stays exactly as it was, and only a request that names
// before_seq or limit gets this.
type messagePageJSON struct {
	Messages []json.RawMessage `json:"messages"`
	// FirstSeq and LastSeq bound the page: a client fetches the next older
	// page with before_seq=first_seq. Both are 0 for an empty page.
	FirstSeq int `json:"first_seq"`
	LastSeq  int `json:"last_seq"`
	// Total is the session's whole durable message count (see
	// engine.SessionIndex.DurableMessages), so a client can size a
	// scrollbar without a second call. It can be lower than the `messages`
	// field of GET /session, which also counts the repair messages a
	// replay derives; those have no record, so they have no seq.
	Total int `json:"total"`
	// HasMore reports whether older messages exist before FirstSeq.
	HasMore bool `json:"has_more"`
}

// handleMessagePage answers GET /session/{id}/message?before_seq=N&limit=K:
// the K durable messages immediately before seq N, newest page by default.
//
// It reads the journal's TAIL through engine.ReadMessagePage, never the
// whole log, and it does so whether or not the session is resident. Reading
// the durable records even for a live session is what keeps one seq
// definition for both cases: a resident history can carry messages the log
// does not (message.ResolveOrphanToolCalls repairs applied at load,
// recovery's memory-only closers), and numbering those would give the same
// message two different seqs depending on residency.
//
// A session with no journal at all — created and never persisted — has no
// durable sequence to page. It falls back to the resident history, which
// for such a session is exactly the durable sequence it would have had.
func (s *Server) handleMessagePage(w http.ResponseWriter, query url.Values, id string) {
	beforeSeq, ok := intParam(w, query, "before_seq")
	if !ok {
		return
	}
	limit, ok := intParam(w, query, "limit")
	if !ok {
		return
	}
	// Reject, do not clamp. The published schema names a maximum, and a
	// generated client or a gateway enforces it, so silently answering a
	// larger request with a smaller page would make the server disagree
	// with its own spec. engine.MessagePageWindow still clamps for a
	// direct engine caller, which has no schema to honor.
	if limit > engine.MaxMessagePageLimit {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("limit must be at most %d", engine.MaxMessagePageLimit))
		return
	}
	page, err := engine.ReadMessagePage(s.opts.SessionDir, id, beforeSeq, limit)
	if err != nil {
		s.messagePageFallback(w, id, beforeSeq, limit, err)
		return
	}
	writeJSON(w, http.StatusOK, messagePageJSON{
		Messages: marshalMessages(page.Messages),
		FirstSeq: page.FirstSeq,
		LastSeq:  page.LastSeq,
		Total:    page.Total,
		HasMore:  page.HasMore,
	})
}

// messagePageFallback answers a page request that engine.ReadMessagePage
// could not, and it classifies the reason rather than giving one answer for
// all of them.
//
// It pages resident history for exactly ONE case: a live session with no
// durable journal at all. Such a session has no records, so its resident
// history IS its durable sequence, and numbering it invents nothing. Every
// other case keeps the durable contract instead of bending it. A resident
// history can carry messages the log does not — message.ResolveOrphanToolCalls
// repairs applied at load, recovery's memory-only closers — so paging it for
// a session whose journal merely could not be READ would hand those messages
// sequence numbers. A client that then paged again, after the journal
// became readable, would see its pages renumbered.
//
// So: a missing journal with no live session is a 404, the same answer the
// unparameterized read gives. A journal that exists but cannot be read, or
// keeps changing under the read, is a 500 — for a live session too. A
// session whose journal exists is not "no such session", and reporting it
// as one sends an operator looking for an id that is on disk in front of
// them.
func (s *Server) messagePageFallback(w http.ResponseWriter, id string, beforeSeq, limit int, cause error) {
	// A session dir the process never configured has no durable sequence
	// for ANY session, so resident history is the only answer there is.
	noJournal := errors.Is(cause, fs.ErrNotExist) || s.opts.SessionDir == ""
	if !noJournal {
		writeErr(w, http.StatusInternalServerError, "cannot read session messages")
		return
	}
	sess := s.liveSessionObject(id)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	// Number the same sequence the journal path numbers: durable messages
	// only. A session with no journal has no repair applied to its history
	// today — the repair runs at load, and this session was never loaded —
	// but filtering makes the two paths agree by CONSTRUCTION rather than
	// by an argument about which shapes can reach here.
	msgs := durableOnly(sess.History())
	total := len(msgs)
	// The same window arithmetic the journal path uses, from the same
	// helper: two copies would give one session two different paginations
	// depending on which path answered it.
	lo, hi, _ := engine.MessagePageWindow(total, beforeSeq, limit)
	if hi < lo {
		writeJSON(w, http.StatusOK, messagePageJSON{Messages: []json.RawMessage{}, Total: total})
		return
	}
	writeJSON(w, http.StatusOK, messagePageJSON{
		Messages: marshalMessages(msgs[lo-1 : hi]),
		FirstSeq: lo,
		LastSeq:  hi,
		Total:    total,
		HasMore:  lo > 1,
	})
}

// durableOnly drops the messages a load-time repair derives
// (message.ResolveOrphanToolCalls) from a resident history. Such a message
// has no record in the journal, so it has no byte offset and no sequence
// number — see engine/messagepage.go's package comment. A page must never
// give one a seq, whichever path produced the page.
func durableOnly(msgs []message.Message) []message.Message {
	out := make([]message.Message, 0, len(msgs))
	for _, m := range msgs {
		if message.IsSyntheticOrphanID(m.ID) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// intParam reads a non-negative integer query parameter, writing 400 and
// returning ok=false when it is present but not one. An absent parameter is
// 0, which every caller reads as "unset". It takes the already-parsed
// query: url.URL.Query() re-parses the raw string on every call, and a page
// request asks for two parameters after testing for two more.
//
// A repeated parameter is a 400, not a silent choice of the first value:
// "?limit=2&limit=nonsense" names two different intentions, and answering
// one of them hides a client bug. An explicitly empty value ("?limit=") is
// a 400 for the same reason: it is present, and it is not an integer.
func intParam(w http.ResponseWriter, query url.Values, name string) (int, bool) {
	values := query[name]
	if len(values) == 0 {
		return 0, true
	}
	if len(values) > 1 {
		writeErr(w, http.StatusBadRequest, name+" must be given at most once")
		return 0, false
	}
	v, err := strconv.Atoi(values[0])
	if err != nil || v < 0 {
		writeErr(w, http.StatusBadRequest, name+" must be a non-negative integer")
		return 0, false
	}
	return v, true
}

// requestJSON is the openapi Request shape: the latest fully-assembled model
// request for a session (canonical, in-memory only).
type requestJSON struct {
	Model    message.ModelRef `json:"model"`
	System   []string         `json:"system"`
	Tools    []string         `json:"tools"`
	Messages int              `json:"messages"`
	Params   paramsJSON       `json:"params"`
}

type paramsJSON struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

// handleRequest returns the latest fully-assembled request the process was
// about to send for a session. It reads memory only (full requests are never
// persisted), so a session that has not prompted this process is 404 —
// including a valid, on-disk session that has only been created.
func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	snap := s.lastRequest[id]
	s.mu.Unlock()
	if snap == nil {
		writeErr(w, http.StatusNotFound, "no assembled request for session")
		return
	}
	system := snap.system
	if system == nil {
		system = []string{}
	}
	tools := snap.tools
	if tools == nil {
		tools = []string{}
	}
	writeJSON(w, http.StatusOK, requestJSON{
		Model:    snap.model,
		System:   system,
		Tools:    tools,
		Messages: snap.messages,
		Params: paramsJSON{
			Temperature: snap.temperature,
			TopP:        snap.topP,
			MaxTokens:   snap.maxTokens,
		},
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	type entry struct {
		Type     string        `json:"type"`
		State    string        `json:"state"`
		LastTurn *lastTurnJSON `json:"last_turn,omitempty"`
		Usage    usageJSON     `json:"usage"`
	}
	result := map[string]entry{}

	type snap struct {
		id      string
		running bool
		sess    *engine.Session
	}
	s.mu.Lock()
	mem := make([]snap, 0, len(s.sessions))
	for id, st := range s.sessions {
		mem = append(mem, snap{id, st.running, st.sess})
	}
	s.mu.Unlock()
	for _, m := range mem {
		result[m.id] = entry{
			Type:     statusStr(m.running),
			State:    s.compositeStateFor(m.id, m.running),
			LastTurn: s.lastTurnFor(m.id),
			Usage:    usageJSONForSession(m.sess),
		}
	}
	// Ids first, indexes second — the same rule GET /session follows. A
	// session already answered from memory above needs no index, and
	// reading one for it would refold and rewrite the sidecar of a session
	// this process holds live, racing that session's own writer.
	ids, err := engine.ListSessionIDs(s.opts.SessionDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot list sessions")
		return
	}
	for _, id := range ids {
		if _, ok := result[id]; ok {
			continue
		}
		// The same index-then-scan path a listing takes, so this endpoint
		// and GET /session never disagree about which sessions exist: a
		// session whose fold breaks appears in both, from its journal.
		info, err := engine.ReadSessionInfo(s.opts.SessionDir, id)
		if err != nil {
			continue // unreadable or not a session journal: not listable
		}
		result[id] = entry{
			Type:     "idle",
			State:    s.compositeStateFor(id, false),
			LastTurn: s.lastTurnFor(id),
			Usage:    usageJSONForInfo(info),
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// lastTurnFor is lastTurnJSONLocked with its own locking, for callers (like
// handleStatus) that are not already holding s.mu.
func (s *Server) lastTurnFor(id string) *lastTurnJSON {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTurnJSONLocked(id)
}

// compositeStateFor resolves the composite state for a session ID using this
// process's goal tracker (see compositeState) — the same source Session JSON
// uses, so /session/status and GET /session/{id} agree.
func (s *Server) compositeStateFor(id string, running bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	goal := goalJSONFrom(s.goalState[id])
	return compositeState(running, goal != nil && goal.Active, forcesIdlePause(goal))
}

// promptAsyncResponse is POST /session/{id}/prompt_async's success-response
// body (see handlePrompt/enqueueOrDispatch): status is "started" when a
// prompt turn is now running (this call's own claim, or the freed-slot retry
// dispatching THIS request's own just-enqueued prompt — see
// enqueueOrDispatch's doc comment for the exact rule), or "queued" when this
// request's prompt is sitting in the durable FIFO waiting for a future
// drain. Queued carries the current queue depth (including this request's
// own prompt) only when status is "queued" — omitted (0) on "started",
// where it would be meaningless.
//
// One narrow exception: Queued reads 0 (and so is omitted, same JSON shape
// as "started") on a "queued" response when a concurrent DELETE
// /session/{id}/queue cleared the entire queue — including this request's
// own just-enqueued prompt — in the gap between this call's own enqueue and
// its dispatch-the-head attempt (see the two dispatchQueueHead call sites'
// doc comments). This is the most honest shape the existing vocabulary
// offers for "accepted, then cleared before it ran": the request was not an
// error (its prompt WAS durably enqueued and journaled), it simply never
// got the chance to run. See TestQueueClearRaceDuringIdleDispatchIsNotAnError
// and TestQueueClearRaceDuringDispatchIsNotAnError.
type promptAsyncResponse struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
	Queued int    `json:"queued,omitempty"`
}

// handlePrompt is POST /session/{id}/prompt_async (see docs/plans/2026-07-19-
// prompt-queue.md). An idle session claims its run slot exactly as before and
// starts running immediately ("started"). A session already busy with
// ANOTHER prompt or goal loop no longer 409s: the prompt is enqueued
// durably (engine.Session.EnqueuePrompt, synchronously, before any response
// is written — the accept-vs-lose race this closes is the same one
// RegisterGoal/handleGoalBusy already close for goals), then ONE claim retry
// is made to close the freed-slot race where the busy occupant finishes in
// the gap between the failed claim above and the enqueue (see
// enqueueOrDispatch). The workdir-held-by-ANOTHER-session 409 and the
// draining 503 are unchanged — only same-session busy gets queue semantics.
func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Model message.ModelRef `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "parts must be non-empty")
		return
	}
	var texts []string
	for _, p := range body.Parts {
		if p.Type != "text" {
			writeErr(w, http.StatusBadRequest, "v1 accepts text parts only")
			return
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n")

	// Resolve the session and atomically claim its prompt slot (also does the
	// wg.Add under the admission gate). See claimForPrompt for the ordering that
	// makes eviction races and drain admission impossible.
	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			// Same-session busy: queue-on-busy (invariant 9), not a 409.
			s.enqueueOrDispatch(w, id, text)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	if len(st.sess.QueuedPrompts()) > 0 {
		// Global FIFO on an idle-with-queue session: the queue can be
		// non-empty even though claimForPrompt just succeeded (the session
		// itself was idle) — a restart refold (TestQueueRestartRefoldNoAuto
		// Dispatch's queue survives a restart with the session left idle),
		// or a prompt stranded by a gap in some OTHER tail's drain wiring.
		// Either way, this request's own prompt must never jump the line:
		// enqueue it durably behind whatever is already waiting, then
		// dispatch the queue's HEAD (not necessarily this request's own
		// text) into the run slot just claimed above. See
		// dispatchQueueHead and enqueueOrDispatch's identical shape for the
		// same-session-BUSY counterpart of this same rule.
		ourID, err := st.sess.EnqueuePrompt(text)
		if err != nil {
			// handlePrompt already rejects an empty parts list and joins
			// non-empty text above, so this is not reachable in practice;
			// fail closed rather than silently drop the request, releasing
			// the claim just taken.
			s.releasePromptClaim(st)
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.queueDispatchRace != nil {
			// Test-only seam (see enqueueOrDispatch's identical use): let a
			// test force a concurrent DELETE /session/{id}/queue to land
			// deterministically in the gap between the EnqueuePrompt above
			// and the DequeuePrompt inside dispatchQueueHead below. Nil in
			// production.
			s.queueDispatchRace()
		}
		head, remaining, ok := s.dispatchQueueHead(id, st, ctx)
		if !ok {
			// Benign race, not a bug: a concurrent DELETE /session/{id}/queue
			// (safe to call regardless of run-slot state — see its own doc
			// comment) cleared the ENTIRE queue, including the prompt
			// EnqueuePrompt just added above, in the gap between that
			// enqueue and this dispatch attempt — dispatchQueueHead already
			// released the run-slot claim taken above, exactly like
			// runPrompt's own tail would. This request's own prompt WAS
			// accepted (durably enqueued and journaled) but never ran:
			// report the most honest shape the existing status vocabulary
			// offers — "queued" with the current (now necessarily zero)
			// depth — rather than a 500, which would misrepresent a benign,
			// documented race as a server bug. See
			// TestQueueClearRaceDuringIdleDispatchIsNotAnError and
			// promptAsyncResponse's queued field doc for why depth 0 is
			// possible here.
			writeJSON(w, http.StatusAccepted, promptAsyncResponse{
				Seq: fromSeq, Status: "queued", Queued: len(st.sess.QueuedPrompts()),
			})
			return
		}
		status := "queued"
		if head.ID == ourID {
			status = "started"
		}
		resp := promptAsyncResponse{Seq: fromSeq, Status: status}
		if status == "queued" {
			// remaining, not a fresh QueuedPrompts() re-read — see
			// dispatchQueueHead's own doc comment for the race that used
			// to live here.
			resp.Queued = remaining
		}
		writeJSON(w, http.StatusAccepted, resp)
		return
	}

	// Explicit model wins over the session's persisted model (CLI -model
	// rule) -- applied only here, on the empty-queue fast path, because this
	// is the one branch where THIS request's own prompt is actually the one
	// about to run next. Applying it earlier (before the queue check above)
	// retargeted the session's model even when a DIFFERENT, already-queued
	// head was what actually got dispatched — contradicting the documented
	// "a per-request model override is silently dropped when the prompt is
	// queued" rule (see docs/session-storage-and-queue.md's "Prompt queue"
	// section and
	// enqueueOrDispatch's identical rule for the same-session-busy branch).
	// See TestQueuedArrivalDoesNotRetargetSessionModel.
	if !body.Model.IsZero() {
		// Reject an unconfigured provider BEFORE SetModel runs — the same
		// ModelSupported check handleSetModel and the `model` tool use, so all
		// three SetModel routes validate identically. SetModel would otherwise
		// persist the durable recModel record for a ref no transcoder can
		// resolve: the turn fails on Providers.For, and every later request —
		// including after a LoadSession resume — transcodes against the bad
		// ref, wedging the session for its whole life. This is the empty-queue
		// fast path (the queue-non-empty branch above already drops the model
		// override per the "silently dropped when queued" rule), so nothing is
		// enqueued yet and no prompt is orphaned on reject: release the run-slot
		// claim taken by claimForPrompt (mirrors releasePromptClaim's other
		// nothing-to-run callers) and return, leaving zero durable state behind.
		if !st.sess.ModelSupported(body.Model) {
			s.releasePromptClaim(st)
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("provider %q is not configured", body.Model.Provider))
			return
		}
		// Same gate, same place, for the third SetModel route — see
		// handleSetModel's own call and engine.Session.CheckModel.
		if err := st.sess.CheckModel(body.Model); err != nil {
			s.releasePromptClaim(st)
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// SetModel emits EventModelChanged on a real change, which Publish
		// journals as the durable "model" record (see server/journal.go). No
		// explicit emitDurable here: that would double-journal one swap.
		st.sess.SetModel(body.Model)
	}

	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runPrompt(ctx, id, st, text, "")
	writeJSON(w, http.StatusAccepted, promptAsyncResponse{Seq: fromSeq, Status: "started"})
}

// enqueueOrDispatch implements handlePrompt's same-session-busy branch:
// claimForPrompt 409'd with an empty holder, meaning something in THIS
// session (a running prompt or goal loop) already holds the run slot (the
// workdir-held-by-ANOTHER-session case is handled inline in handlePrompt and
// never reaches here — mirrors handleGoalBusy's same split).
//
// text is enqueued durably (EnqueuePrompt persists prompt.queued and emits
// its event before returning — see engine/queue.go) BEFORE any response is
// written, then ONE claim retry is made: this closes the race where the busy
// occupant's own tail (runPrompt/runGoal, which now calls
// maybeDispatchQueued — see its doc comment) runs between the failed claim
// in handlePrompt and this function's EnqueuePrompt call. If that happened,
// EnqueuePrompt is exactly what maybeDispatchQueued needed to see (it would
// have found the queue empty a moment earlier), so the retry here either
// wins the now-free slot itself or loses it to that same tail's own retry
// (via maybeDispatchQueued, whose own claim attempt may instead win) —
// either way this prompt starts exactly once, never zero times, never
// twice. This is the queue's counterpart to handleGoalBusy's register-then-
// retry pattern.
//
// On a WON retry, the head of the queue is dispatched — not necessarily this
// request's own prompt, since other prompts may already have been queued
// ahead of it (FIFO order is by queue ID, not by which HTTP request happens
// to observe the free slot first). The response's status reflects what
// happened to THIS request's own prompt specifically: "started" only if the
// dispatched head IS the prompt this call just enqueued; otherwise "queued"
// (this call's prompt is still waiting, now one place closer to the front).
// This is the simplest rule that stays correct regardless of how many other
// prompts were already queued: it never requires the caller to reason about
// queue position, only "is my own prompt running or not, right now".
//
// A model override on a request whose prompt gets queued (either branch) is
// silently NOT applied: QueuedPrompt carries only ID and Text (see the plan's
// "text-only" locked decision — no attachment machinery), so there is no
// slot to carry a per-prompt model override through to a future drain. A
// caller that needs a model swap to take effect should re-issue it once its
// prompt is confirmed "started".
func (s *Server) enqueueOrDispatch(w http.ResponseWriter, id string, text string) {
	sess := s.residentSession(id)
	if sess == nil {
		// Benign race window, identical to handleGoalBusy's (see its doc
		// comment): claimForPrompt found the session resident and running
		// (hence the 409 that routed us here), but s.mu is released between
		// that check and this residentSession call, and the busy occupant
		// finished and was evicted in the gap. A client retry resolves it
		// against a freshly (re)loaded, now-idle session.
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	ourID, err := sess.EnqueuePrompt(text)
	if err != nil {
		// handlePrompt already rejects an empty parts list and joins
		// non-empty text, so this is not reachable in practice; fail closed
		// rather than silently drop the request.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.queueDispatchRace != nil {
		// Test-only seam (mirrors autoArmRace): let a test force a real
		// concurrent claim to land here deterministically. Nil in production.
		s.queueDispatchRace()
	}
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		// Lost the retry: still queued, whatever already occupies the slot
		// keeps running undisturbed.
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{
			Seq: s.currentSeq(), Status: "queued", Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	head, remaining, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		// Benign race, not a bug: a concurrent DELETE /session/{id}/queue
		// cleared the ENTIRE queue — including the prompt this call's own
		// EnqueuePrompt just added above — somewhere in the gap between
		// that enqueue and this dispatch attempt (the seam above is one
		// deterministic way a test can land squarely in that gap;
		// dispatchQueueHead has already released the run-slot claim taken
		// by claimForPrompt just above, exactly like runPrompt's own tail
		// would). This request's own prompt WAS accepted (durably enqueued
		// and journaled) but never ran: report the same honest shape
		// handlePrompt's idle-with-queue branch uses for this identical
		// race — "queued" with the current (now necessarily zero) depth —
		// rather than a 500, which would misrepresent a benign, documented
		// race as a server bug. See TestQueueClearRaceDuringDispatchIsNotAnError.
		writeJSON(w, http.StatusAccepted, promptAsyncResponse{
			Seq: s.currentSeq(), Status: "queued", Queued: len(sess.QueuedPrompts()),
		})
		return
	}

	status := "queued"
	if head.ID == ourID {
		status = "started"
	}
	resp := promptAsyncResponse{Seq: s.currentSeq(), Status: status}
	if status == "queued" {
		// remaining, not a fresh QueuedPrompts() re-read — see
		// dispatchQueueHead's own doc comment for the race that used to
		// live here.
		resp.Queued = remaining
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// enqueueResponse is POST /session/{id}/enqueue's success body. Unlike
// promptAsyncResponse it never carries the journal's SSE seq — the field
// name "seq" is already taken by the request's own idempotency sequence,
// and an enqueue caller acks by watermark, not by event cursor.
// Watermark is the session's durable-enqueue high-water mark AFTER this
// request (== the request's own seq on accept; the pre-existing mark on
// duplicate). Queued mirrors promptAsyncResponse's rule: depth including
// this prompt, only when status is "queued".
type enqueueResponse struct {
	Status    string `json:"status"` // "started" | "queued" | "duplicate"
	Watermark int64  `json:"watermark"`
	Queued    int    `json:"queued,omitempty"`
}

// handleEnqueue is POST /session/{id}/enqueue (see docs/plans/2026-07-21-
// durable-enqueue.md): prompt_async's shape with an honest durability and
// idempotency contract. The prompt is fsynced into the session journal
// (engine.Session.EnqueuePromptDurable) BEFORE any success response — a 2xx
// authorizes the caller to ack ITS upstream — and a seq at or below the
// session's watermark is a 200 duplicate no-op, so upstream retries are
// always safe. Delivery is unchanged queue machinery: idle sessions
// dispatch the queue head immediately, busy sessions drain at turn/tool
// boundaries. No model override (queued prompts carry text only — see
// enqueueOrDispatch's doc comment); the workdir-busy 409, draining 503, and
// unknown-session 404 mirror handlePrompt.
func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	var body struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
		Seq int64 `json:"seq"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.Parts) == 0 {
		writeErr(w, http.StatusBadRequest, "parts must be non-empty")
		return
	}
	if body.Seq < 1 {
		writeErr(w, http.StatusBadRequest, "seq must be >= 1")
		return
	}
	var texts []string
	for _, p := range body.Parts {
		if p.Type != "text" {
			writeErr(w, http.StatusBadRequest, "v1 accepts text parts only")
			return
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n")
	// EnqueuePromptDurable rejects empty/whitespace-only text too, but by
	// then we'd have already taken (or failed to take) the run-slot claim,
	// and from the handler's side that engine error is indistinguishable
	// from a genuine persist failure — both fall through to the 500
	// "enqueue not durable" mapping below, which tells the caller to retry
	// with the same seq. An input that can never succeed must 400 instead,
	// and before any claim is taken.
	if strings.TrimSpace(text) == "" {
		writeErr(w, http.StatusBadRequest, "text must be non-empty")
		return
	}

	st, ctx, _, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			s.enqueueDurableBusy(w, id, text, body.Seq)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}

	// Idle: we hold the run slot. Durable-first, then dispatch the queue
	// HEAD — not necessarily this request's prompt (global FIFO, same rule
	// as handlePrompt's idle-with-queue branch).
	ourID, dup, err := st.sess.EnqueuePromptDurable(text, body.Seq)
	if dup {
		s.releasePromptClaim(st)
		// Stranded-head liveness fix: THIS request's prompt was a no-op,
		// but the run slot we just released may be stranding SOMEONE
		// ELSE's already-durable prompt. A concurrent same-seq retry can
		// land in enqueueDurableBusy while we hold the claim above, durably
		// enqueue there (advancing the watermark this call sees as a
		// duplicate), and then lose ITS OWN one-shot claim retry to us —
		// see enqueueDurableBusy's doc comment for that race. Without this
		// call, that prompt would sit in the queue on a now-idle session
		// with nothing left to dispatch it until unrelated future
		// activity. maybeDispatchQueued (see its doc comment) is built for
		// exactly this tail position: it re-claims the slot and dispatches
		// the head, or safely no-ops if the queue is empty or something
		// else wins the race — called BEFORE the response is written so
		// that a client polling GET /session/{id}/wait immediately after
		// this 2xx returns never observes a false "idle" ahead of the
		// drain's own claim. See TestEnqueueDuplicateOnIdleWithQueueDrainsHead.
		s.maybeDispatchQueued(id, st)
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: st.sess.EnqueueSeq()})
		return
	}
	if err != nil {
		s.releasePromptClaim(st)
		// Same stranded-head exposure as the duplicate branch above: our
		// own durable enqueue failed, but a concurrent request may already
		// have durably queued behind us in enqueueDurableBusy and lost its
		// claim retry to us. Drain before responding, for the same reason.
		s.maybeDispatchQueued(id, st)
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	head, remaining, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		// Concurrent DELETE /session/{id}/queue cleared everything in the
		// gap — same benign race as handlePrompt's idle-with-queue branch;
		// the prompt WAS durably accepted (watermark advanced), which is
		// exactly what the response must attest.
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: st.sess.EnqueueSeq(), Queued: len(st.sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: st.sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		// remaining, not a fresh QueuedPrompts() re-read — see
		// dispatchQueueHead's own doc comment for the race that used to
		// live here.
		resp.Queued = remaining
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// enqueueDurableBusy is handleEnqueue's same-session-busy branch, the
// durable mirror of enqueueOrDispatch: durably enqueue (fsynced, error on
// failure — never a silent 2xx), then ONE claim retry to close the
// freed-slot race. See enqueueOrDispatch's doc comment for the race
// analysis; only the enqueue call and response shape differ.
func (s *Server) enqueueDurableBusy(w http.ResponseWriter, id string, text string, seq int64) {
	sess := s.residentSession(id)
	if sess == nil {
		// Same benign race window as enqueueOrDispatch: busy occupant
		// finished and was evicted between the failed claim and here. The
		// caller retries with the same seq — idempotency makes that free.
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	ourID, dup, err := sess.EnqueuePromptDurable(text, seq)
	if dup {
		writeJSON(w, http.StatusOK, enqueueResponse{Status: "duplicate", Watermark: sess.EnqueueSeq()})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "enqueue not durable: "+err.Error())
		return
	}
	if s.queueDispatchRace != nil {
		s.queueDispatchRace() // test-only seam, mirrors enqueueOrDispatch
	}
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	head, remaining, ok := s.dispatchQueueHead(id, st, ctx)
	if !ok {
		writeJSON(w, http.StatusAccepted, enqueueResponse{
			Status: "queued", Watermark: sess.EnqueueSeq(), Queued: len(sess.QueuedPrompts()),
		})
		return
	}
	resp := enqueueResponse{Status: "queued", Watermark: sess.EnqueueSeq()}
	if head.ID == ourID {
		resp.Status = "started"
	} else {
		// remaining, not a fresh QueuedPrompts() re-read — see
		// dispatchQueueHead's own doc comment for the race that used to
		// live here.
		resp.Queued = remaining
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// releasePromptClaim releases a run-slot claim taken by claimForPrompt
// without running a turn: the exact reset runPrompt's own tail performs,
// shared by every path that claims the slot and then discovers there is
// nothing to run (an enqueue error, a queue emptied by a concurrent DELETE
// /session/{id}/queue).
func (s *Server) releasePromptClaim(st *sessionState) {
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	evicted := s.evictResidentLocked()
	s.mu.Unlock()
	releaseEvicted(evicted)
	s.wg.Done()
}

// freeRunSlotAndEmitIdle releases a just-completed turn's run-slot claim and
// journals the session's idle transition in ONE s.mu critical section —
// shared by runPrompt's, runGoal's, and handleCompact's tails, the three
// places a turn frees the slot it held.
//
// This closes a race the two-step version (unlock, THEN emit idle
// separately) left open: claimForPrompt needs the identical s.mu to read
// st.running, so a concurrent claimer racing in during the gap between the
// unlock and the separate emitDurable call could observe running==false and
// win the freed slot — dispatching its own turn's busy event — BEFORE this
// turn's own idle had been assigned a seq. That inverts the ordering
// collectUntilIdle's doc comment guarantees ("session.status busy for the
// dispatched turn always arrives strictly after this idle") and is exactly
// what TestPromptQueueRaceWithFreedSlot forces deterministically: a prompt
// queued behind a turn whose own idle got delayed past the very turn it was
// supposed to precede. Doing the running=false write and the idle emit
// under one lock hold makes the race impossible: any other claimForPrompt
// call blocks on the same mutex until this whole section — running=false
// AND idle already durably emitted — has completed, so it can never observe
// one without the other.
//
// The idle transition is ALWAYS emitted here, even when the queue is
// non-empty and about to be redispatched by this same tail's own
// maybeDispatchQueued call — collectUntilIdle and every test built on it
// depend on that. Server.queueDrainPending (its own doc comment) is set
// here, unconditionally, still under this same lock, so waitSnapshot
// (wait.go) can tell this transient, self-resolving gap apart from a
// session that is genuinely idle with a queue nobody is about to drain
// (e.g. resumed after a restart).
//
// Unconditional, not gated on "is the queue actually non-empty right
// now": queueDrainPending's own doc comment covers why in full, but in
// short, any such gate reads from a source that can itself be stale at
// this exact instant relative to a concurrent enqueue still landing —
// this call always follows immediately with maybeDispatchQueued (every
// caller's own tail), whose deferred clearQueueDrainPending resolves the
// flag correctly either way, so there is nothing to gain by checking here
// and a live-reproduced false-idle window to lose by getting the check
// wrong.
func (s *Server) freeRunSlotAndEmitIdle(id string, st *sessionState) {
	s.mu.Lock()
	st.running = false
	st.cancel = nil
	st.goalLoop = false
	st.lastUsed = time.Now()
	evicted := s.evictResidentLocked()
	s.emitDurableLocked(&Event{Type: evtSessionStatus, SessionID: id, Status: "idle"})
	s.queueDrainPending[id] = true
	s.mu.Unlock()
	releaseEvicted(evicted)
}

// dispatchQueueHead dequeues the session's queue head (reason "delivered")
// into the run slot st/ctx already holds — emitting its busy transition and
// spawning its runPrompt turn — shared by every call site that has JUST
// claimed (or already holds) the run slot and knows the queue is (or was
// just made) non-empty: handlePrompt's idle-with-queue branch,
// enqueueOrDispatch's won-retry branch, and maybeDispatchQueued.
//
// ok is false only when the queue turns out to be empty despite the
// caller's own check — reachable solely via a benign concurrent DELETE
// /session/{id}/queue race between that check and this call. In that case
// the claim just taken is released here (mirrors runPrompt's own tail reset)
// so the run slot never gets stuck "running" with nothing driving it; the
// caller only needs to respond, not clean up.
//
// remaining is st.sess.QueuedPrompts()'s length, snapshotted HERE —
// synchronously, immediately after the dequeue above, still before
// runPrompt's own goroutine is spawned — for every caller's own response
// to use directly, INSTEAD OF re-reading QueuedPrompts() itself after this
// call returns. A live review finding, root-caused from an intermittent
// CI failure (TestIdlePromptWithQueueGoesFIFO, reproduced 200+ times
// locally before finally catching it under real scheduling pressure — see
// that test's own doc comment): every caller used to compute its response's
// queued depth by re-reading QueuedPrompts() AFTER this call returned,
// racing the just-spawned runPrompt goroutine below. That goroutine is
// handed the run slot and nothing else waits for it — with a fast provider
// (a scripted test double, or simply an unlucky real one), it can run
// head's ENTIRE turn to completion and, via its own tail's
// maybeDispatchQueued, dispatch (and itself run to completion) the NEXT
// queued item too, before the calling goroutine's own next statement ever
// executes. -race never catches this: every field access is correctly
// mutex-protected, so there is no data race — it is a pure ordering
// assumption ("the item I just dispatched is still running, so anything
// behind it in the queue is untouched") that nothing here actually
// enforces.
//
// remaining comes straight from DequeuePrompt's own return value —
// engine.Session's answer to "how many are left," computed under the
// SAME s.mu hold as the dequeue itself (engine/queue.go) — never a
// separate, follow-up QueuedPrompts() call here. A live review finding
// on an earlier version of this fix: a second, separately-locked read
// reintroduced a NARROWER version of the exact race this method exists
// to close — a different dequeue (a concurrent DELETE
// /session/{id}/queue, another dispatch) can interleave in the gap
// between the two separately-locked calls, same class of gap as the
// goroutine-spawn race above, just smaller. Taking the count directly
// from the dequeue's own atomic result removes that gap entirely: there
// is no second lock acquisition left to race.
func (s *Server) dispatchQueueHead(id string, st *sessionState, ctx context.Context) (head engine.QueuedPrompt, remaining int, ok bool) {
	head, remaining, ok = st.sess.DequeuePrompt("delivered")
	if !ok {
		s.releasePromptClaim(st)
		return head, 0, false
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	// origin "": a dequeued prompt is always someone's real durably-queued
	// text (an ordinary prompt_async caught behind a busy turn, or a
	// session.send) — never the engine's own synthetic resume trigger, even
	// when the DRAIN that reaches here was itself provoked by
	// runOrQueueText's queue-non-empty branch (session_tree.go): that branch
	// deliberately dispatches the QUEUE HEAD instead of its own trigger
	// text, exactly so a real queued message is never displaced by the
	// resume trigger — see runOrQueueText's own doc comment.
	go s.runPrompt(ctx, id, st, head.Text, "")
	if s.dispatchQueueHeadRace != nil {
		// Test-only seam — see its own doc comment (server.go).
		s.dispatchQueueHeadRace()
	}
	return head, remaining, true
}

// runPrompt drives one Prompt to completion, then records the trailing
// messages and flips the session back to idle. The prompt's context is
// cancelled only by POST /abort, so a context.Canceled result is a deliberate
// abort — journaled as a durable session.aborted. Any other error is a genuine
// failure (provider error, transcode failure) — journaled as session.error
// with detail. Either way a durable record precedes the idle transition so a
// disconnected orchestrator learns the outcome on replay; the 202 only
// acknowledged receipt.
//
// origin is forwarded to Session.PromptWithOrigin verbatim (see
// message.Message.Origin's own doc comment): message.OriginEngine for
// runOrQueueText's own resume-trigger dispatch (session_tree.go, the ONLY
// call site that ever passes it), empty for every other caller — an
// ordinary prompt_async turn (handlePrompt), a dequeued prompt
// (dispatchQueueHead, whether or not the drain was itself provoked by a
// resume trigger — see that function's own doc comment), or a session.send
// delivery (sendTextToRoot).
func (s *Server) runPrompt(ctx context.Context, id string, st *sessionState, text string, origin string) {
	defer s.wg.Done()
	// ReportTurnStart/ReportTurnEnd bracket the ONE choke point every
	// ordinary (non-goal-loop) turn on a resident session funnels through
	// — handlePrompt's happy path, dispatchQueueHead, and
	// runOrQueueText/resumeSessionForTaskNotification (session_tree.go)
	// all call this function rather than driving Prompt themselves — so
	// this single bracket is what keeps SessionManager's view of "is this
	// session running" accurate for a root no matter which of those paths
	// triggered the turn. Adopts id as a fresh SessionManager root on
	// first sight if it isn't tracked yet (see ReportTurnStart's doc
	// comment) — the case a session reloaded after a restart or eviction
	// hits, closing the "task tool broken after restart" gap a live
	// review caught.
	s.sessMgr.ReportTurnStart(st.sess)
	msg, err := st.sess.PromptWithOrigin(ctx, text, origin)
	s.syncMessages(id) // catch any message not yet journaled
	switch {
	case err == nil:
		s.recordTurnEnd(id, "completed", nil)
	case errors.Is(err, context.Canceled):
		s.emitDurable(Event{Type: evtSessionAborted, SessionID: id})
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
		s.recordTurnEnd(id, turnEndOutcome(err), err)
	}
	s.freeRunSlotAndEmitIdle(id, st)
	if s.postIdleEmitRace != nil {
		// Test-only seam — see its own doc comment (server.go). Only
		// wired at this one call site (runPrompt's own tail): the
		// live-reproduced bug and its regression test are both about
		// THIS path specifically (a queued follow-up prompt draining
		// right behind an ordinary prompt turn).
		s.postIdleEmitRace()
	}

	// ReportTurnEnd runs AFTER freeRunSlotAndEmitIdle, not before: it is
	// what makes id's node visible to SessionManager as idle/done, and
	// ANY OTHER goroutine (a different child's own finalizeTurn call,
	// concurrently, delivering ITS completion notification here via
	// nearestLiveAncestorLocked) can act on that visibility the moment it
	// sees it — calling this server's own resumeSessionForTaskNotification,
	// which claims the run slot via the EXACT SAME claimForPrompt this
	// handler's own callers used. If SessionManager showed id idle while
	// the real run slot were STILL held (the ordering an earlier version
	// of this function used), that concurrent claim would see it busy,
	// refuse, and — since SessionManager considers a "handled" claim
	// delivered whether or not it actually started a turn — permanently
	// strand the notification with nothing left to ever retry it. A live
	// CI hang (a test blocked forever waiting for a resume that had been
	// silently dropped this exact way) is what caught this.
	//
	// resume (returned, not fired, by ReportTurnEnd) is non-nil only when
	// id's OWN completion needs to immediately start ANOTHER turn on
	// itself (a notification arrived too late for THIS turn's own
	// checkout). Firing it is safe here: the real run slot is already
	// free (line above), so this call's own resume attempt cannot race
	// itself.
	resume := s.sessMgr.ReportTurnEnd(id, msg, err)

	// Queue beats goal auto-arm (invariant 5): a prompt queued while this
	// turn ran outranks a goal merely waiting to auto-arm — direct user
	// input outranks the background objective. maybeDispatchQueued claims
	// the freed slot and runs the queue head if the queue is non-empty; only
	// when it reports nothing to dispatch (queue empty, or it lost the
	// race) do we fall through to maybeAutoArmGoal. Each dispatched queued
	// prompt's own runPrompt tail repeats this same check, so the queue
	// fully drains, one turn at a time, before the goal ever gets a look —
	// see maybeDispatchQueued's doc comment.
	if s.maybeDispatchQueued(id, st) {
		return
	}

	// A pending task notification for id ITSELF outranks goal auto-arm too
	// (same reasoning as the queue above: it is picked up harmlessly later
	// if this loses the race for the freed slot — the notification stays
	// queued, not lost — so there is no correctness reason to prefer the
	// goal loop over it). resume's own claim (runOrQueueText) is quick and
	// non-blocking (it launches its own async runPrompt/dispatch and
	// returns), so calling it synchronously here, before the
	// maybeAutoArmGoal fallback, is safe.
	if resume != nil {
		resume()
		return
	}

	// Auto-arm (Task 5): a goal set (or self-adjust-set) mid-turn via the
	// `goal` session tool, or armed by a POST /goal that arrived while this
	// prompt was busy (handleGoalBusy's "armed" response), begins running
	// now instead of sitting active-but-idle until an operator happens to
	// re-poll. This runs AFTER the idle emit above so an SSE collector
	// always observes the prompt's idle before the goal's own busy (see
	// maybeAutoArmGoal's doc comment for the full race analysis).
	s.maybeAutoArmGoal(id, st)
}

// maybeDispatchQueued is called at the tail of both runPrompt (BEFORE
// maybeAutoArmGoal — see its call site above, invariant 5) and runGoal
// (below — goal termination frees the run slot too, and the engine's own
// turn-boundary drain, engine/goal.go's PursueGoal, only runs BETWEEN turns;
// a prompt queued after the loop's last turn boundary but before it actually
// terminates needs this hook to ever be dispatched). It runs after the
// just-finished turn's own idle transition has already been emitted.
//
// If the session's durable prompt queue is non-empty, it claims the run slot
// exactly like maybeAutoArmGoal, dequeues the head (reason "delivered"), and
// spawns a normal runPrompt turn for it. Returns true when it dispatched a
// turn, false when there was nothing queued or it lost the race.
//
// A losing race — the slot was claimed by an incoming prompt_async's own
// retry (enqueueOrDispatch), a POST /goal's auto-arm retry (handleGoalBusy),
// or another goroutine's own maybeDispatchQueued call — simply returns
// false: whichever request won the claim is now responsible for the
// session's next occupancy, and if that occupant is itself a plain prompt,
// its OWN runPrompt tail calls maybeDispatchQueued again once it finishes —
// so a queued prompt is never stranded, only delayed. See
// TestPromptQueueRaceWithFreedSlot.
//
// No-double-delivery equivalence (invariant 7, documentation only — nothing
// new to enforce beyond what already exists): DequeuePrompt("delivered")
// journals BEFORE the dispatched runPrompt call is even made, mirroring
// EnqueuePrompt's own persist-before-emit shape. A crash between that
// journal write and the dispatched turn's completion leaves the prompt gone
// from the queue on replay — it is not re-delivered, and its text is not
// recoverable from the queue a second time. This is not a new failure mode:
// it is the SAME exposure an ordinary in-flight prompt already has today (a
// crash mid-turn loses that turn's provider call and any partial response;
// replay simply resumes from the last durably appended message). A queued
// prompt becomes, for crash-recovery purposes, indistinguishable from a
// prompt that arrived directly via prompt_async and was already mid-flight
// when the process died, the instant it is dequeued and handed to
// runPrompt. See engine/goal.go's DequeueAllPrompts callsite for the
// engine-side half of this same equivalence (goal-turn injection).
func (s *Server) maybeDispatchQueued(id string, st *sessionState) bool {
	// Resolves whatever freeRunSlotAndEmitIdle's own tail set (see
	// Server.queueDrainPending's doc comment) — unconditionally and on
	// every return path, whether this call actually dispatches, loses the
	// claim race to a concurrent caller, or finds the queue already
	// drained by a concurrent DELETE /session/{id}/queue. Harmless when
	// nothing was pending (deleting an absent map key, waking waiters
	// that will just re-observe the same state).
	defer s.clearQueueDrainPending(id)
	if len(st.sess.QueuedPrompts()) == 0 {
		return false
	}
	if s.queueDispatchRace != nil {
		s.queueDispatchRace()
	}
	claimedSt, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		return false // lost the race; see the doc comment above
	}
	// The queue was drained by a concurrent DELETE /session/{id}/queue
	// between the len check above and winning this claim: dispatchQueueHead
	// already released the claim we just took (mirrors runPrompt's own tail
	// reset) — nothing left to dispatch.
	_, _, ok := s.dispatchQueueHead(id, claimedSt, ctx)
	return ok
}

// clearQueueDrainPending resolves the transient window freeRunSlotAndEmitIdle
// opens when it finds the queue non-empty at idle-emit time (see
// Server.queueDrainPending's own doc comment) — called once, unconditionally,
// by maybeDispatchQueued's own defer, regardless of how that call resolves.
//
// Also wakes any waiter parked on this session directly: clearing the flag
// alone does not correspond to a new durable event in every case (the "queue
// turned out already empty" sub-case — a concurrent DELETE
// /session/{id}/queue landed in the gap between freeRunSlotAndEmitIdle's own
// check and maybeDispatchQueued's — dispatches nothing and so emits nothing
// new). Without this, a waiter parked on the earlier idle event's own
// queueDrainPending-gated non-wake would otherwise sit until its own timeout
// instead of promptly re-observing the now-genuinely-idle state. Reuses
// notifyWaitersLocked, already idempotent and non-blocking, rather than
// inventing a second wake path.
func (s *Server) clearQueueDrainPending(id string) {
	s.mu.Lock()
	delete(s.queueDrainPending, id)
	s.notifyWaitersLocked(id)
	s.mu.Unlock()
}

// maybeAutoArmGoal is called once, at the very tail of runPrompt — never
// from runGoal's tail (see below) — after the prompt's own idle transition
// has already been emitted. If this server has a configured goal evaluator
// and the session's engine-level goal is active with no loop currently
// attached (armed by a POST /goal that arrived while this prompt was busy —
// see handleGoalBusy's "armed" 202 — or by the `goal` session tool's own
// `set` action invoked mid-turn, per docs/plans/2026-07-19-goal-self-
// adjust.md's headline user story), the goal loop starts running right now
// instead of waiting for the next external poke.
//
// It reclaims the run slot itself via claimForPrompt, exactly like a fresh
// POST /goal would. A losing race — the slot got claimed by an incoming
// prompt_async or by handleGoalBusy's own single retry (see its doc
// comment) between runPrompt's unclaim and this call — simply returns
// without starting a second loop: whichever request won the claim is now
// responsible for the session's next occupancy, and if that occupant is
// itself a plain prompt, ITS OWN runPrompt tail will call maybeAutoArmGoal
// again once it finishes, so the still-active goal is never stranded armed
// forever — it just waits one more prompt's length of time. See
// TestAutoArmRaceWithIncomingPrompt.
//
// Deliberately NOT called from runGoal's tail: every terminal outcome of
// PursueGoal (achieved, cleared, max-turns-exhausted, or a permanent error)
// either deactivates the goal or leaves it in the same "active, ordinarily
// re-armable via POST /goal" state a goal has always been left in — none of
// those is the "armed, waiting for a busy run slot to free up" state
// auto-arm exists to bridge. Wiring auto-arm into runGoal's own tail as well
// would add nothing this design needs while risking a self-sustaining spin
// if a future change ever left a loop exiting with the goal still "active
// and freshly re-armable" in the auto-arm sense.
func (s *Server) maybeAutoArmGoal(id string, st *sessionState) {
	if s.opts.GoalEvaluator.IsZero() {
		return
	}
	condition, active := st.sess.ActiveGoal()
	if !active {
		return
	}
	if s.autoArmRace != nil {
		s.autoArmRace()
	}
	claimedSt, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		return // lost the race; see the doc comment above
	}
	s.mu.Lock()
	claimedSt.goalLoop = true
	// Activity-driven resume of a paused goal (restart, worker_failure,
	// or a stale retryable-backoff fold): a plain prompt
	// completing is exactly what re-attaches a loop to a goal left armed
	// with no loop running — reset the FULL pause presentation here via the
	// same helper handleGoal's own re-arm branch uses, so the freshly
	// spawned loop below is never seen wearing a stale paused presentation
	// from before it started (see resetGoalPauseLocked's doc comment for
	// why every field, not just pausedWorker, must reset here).
	resetGoalPauseLocked(s.goalState[id])
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runGoal(ctx, id, claimedSt, condition, 0)
}

// goalPostResponse is POST /session/{id}/goal's success-response body: seq to
// tail events from, plus status naming which of the three outcomes happened:
//   - "started": a fresh loop is now running (the session was idle, or the
//     retry in handleGoalBusy's "not active" branch won the freed slot).
//   - "armed": the goal is registered, but the run slot is still held by a
//     plain prompt; maybeAutoArmGoal starts the loop once that prompt ends.
//   - "updated": an already-running loop's condition was rewritten in place;
//     no new loop, no run-slot claim.
type goalPostResponse struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

// handleGoal starts, updates, or arms a goal loop on a session — see
// docs/plans/2026-07-19-goal-self-adjust.md's Task 5 for the full design.
// Like prompt_async it claims the session's single run slot when the
// session is idle; the evaluator model comes from Options.GoalEvaluator
// (config goal_evaluator_model), and goals are rejected with 400 when it is
// unset.
//
// When claimForPrompt succeeds (the session was idle), three outcomes:
//   - no goal active: RegisterGoal, spawn runGoal, "started".
//   - a goal is active (a paused/restart goal, or one left active-but-idle
//     by an abort — see PursueGoal's context.Canceled branch) with the SAME
//     condition: just resume it, "started".
//   - active with a DIFFERENT condition: UpdateGoal rewrites it in place,
//     then resume with the new condition, "started". This is the one
//     behavior change from the pre-Task-5 contract, which 409'd here
//     instead (see TestGoalReArmDifferentConditionUpdatesAndResumes) —
//     claimForPrompt's success proves no loop is currently running to race
//     UpdateGoal with, so updating in place is always safe here.
//
// When claimForPrompt 409s because THIS session's own run slot is already
// held (empty holder — the non-empty-holder, workdir-held-by-ANOTHER-
// session case is handled inline below, unchanged), handleGoalBusy takes
// over: update-in-place (invariant 7) if a goal loop holds the slot, or
// register-and-arm (invariants 8/9) if a plain prompt does.
func (s *Server) handleGoal(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	if s.opts.GoalEvaluator.IsZero() {
		writeErr(w, http.StatusBadRequest, "goal_evaluator_model is not configured; goals are unavailable")
		return
	}
	var body struct {
		Condition string `json:"condition"`
		MaxTurns  int    `json:"max_turns"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Condition) == "" {
		writeErr(w, http.StatusBadRequest, "condition must be non-empty")
		return
	}

	st, ctx, fromSeq, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			s.handleGoalBusy(w, id, body.Condition, body.MaxTurns)
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}
	// Re-arming a paused/restart (or post-abort) goal. claimForPrompt above
	// only 409s on st.running — it knows nothing about goal state — so
	// reaching here with the engine already reporting an active goal means
	// exactly one thing: this session's goal is active with NO loop
	// attached in this process. A genuinely running loop would already have
	// 409'd via st.running above (handleGoal/runGoal hold the claim for the
	// whole PursueGoal call), so this branch is unreachable for a live
	// provider-backoff park — only the boot-time restart pause (see
	// pauseArmedGoalsAtBoot), an abort that deliberately left the goal
	// active (see PursueGoal's context.Canceled branch), or an equivalent
	// crash-before-spawn window reaches it.
	condition := body.Condition
	if existing, active := st.sess.ActiveGoal(); active {
		if existing != body.Condition {
			// See this function's doc comment: a different condition here
			// now updates and resumes instead of rejecting. UpdateGoal can
			// only fail on "no active goal", which ActiveGoal() just ruled
			// out — structurally unreachable, but fail closed rather than
			// silently resuming the wrong condition if that ever changes.
			if err := st.sess.UpdateGoal(body.Condition); err != nil {
				s.mu.Lock()
				st.running = false
				st.cancel = nil
				st.goalLoop = false
				st.lastUsed = time.Now()
				s.mu.Unlock()
				s.wg.Done()
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		condition = body.Condition
		s.mu.Lock()
		// Reset ALL pause-relevant fold state via the shared helper,
		// mirroring the evtGoalSet fold: if the journal tail before a
		// restart was goal.stalled(retryable, waiting), clearing only
		// pausedRestart leaves pauseView's provider-backoff case firing on
		// a freshly re-armed, genuinely-running goal until its first
		// goal.eval resets waiting. pausedWorker (Task 2) needs
		// the same treatment: a goal left worker-parked (journal tail
		// goal.parked, no loop attached) reaches this exact branch too —
		// claimForPrompt succeeded, so nothing was running — and must not
		// still read paused/worker_failure the instant this re-arm's fresh
		// loop starts. See resetGoalPauseLocked's doc comment — this is
		// also maybeAutoArmGoal's own reset site.
		resetGoalPauseLocked(s.goalState[id])
		s.mu.Unlock()
	} else if err := st.sess.RegisterGoal(body.Condition); err != nil {
		// Register the goal synchronously BEFORE the loop goroutine spawns
		// and before the 202 returns: by the time the caller can DELETE,
		// the goal is active and clearable — the accept-vs-clear race is
		// structurally gone.
		//
		// Undo the claim taken above: mirror the tail of runPrompt/runGoal.
		s.mu.Lock()
		st.running = false
		st.cancel = nil
		st.goalLoop = false
		st.lastUsed = time.Now()
		s.mu.Unlock()
		s.wg.Done()
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.mu.Lock()
	st.goalLoop = true
	s.mu.Unlock()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	go s.runGoal(ctx, id, st, condition, body.MaxTurns)
	writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: fromSeq, Status: "started"})
}

// handleGoalBusy implements handleGoal's same-session-busy branch:
// claimForPrompt 409'd with an empty holder, meaning something in THIS
// session already holds the run slot — either a running goal loop or a
// plain prompt (the workdir-held-by-ANOTHER-session case is handled inline
// in handleGoal and never reaches here).
func (s *Server) handleGoalBusy(w http.ResponseWriter, id string, condition string, maxTurns int) {
	sess := s.residentSession(id)
	if sess == nil {
		// Reachable, in a narrow window: claimForPrompt found the session
		// resident and running (hence the 409 that routed us here), but
		// s.mu is released between that check and this residentSession
		// call. If the busy prompt/goal finishes in that gap, the session
		// goes idle and an eviction sweep (evictResidentLocked, run from
		// several other request paths) can unload it before we look. The
		// 409 below is benign — nothing was mutated — and a client retry
		// resolves it against a freshly (re)loaded, now-idle session.
		writeErr(w, http.StatusConflict, "session is busy")
		return
	}
	if existing, active := sess.ActiveGoal(); active {
		// A goal loop is running RIGHT NOW: claimForPrompt's 409 plus an
		// active goal can only mean the loop itself holds the slot for the
		// whole PursueGoal call (see runGoal) — a plain prompt never leaves
		// ActiveGoal() true. Update in place: no second loop, no run-slot
		// claim (invariant 7).
		if existing != condition {
			if err := sess.UpdateGoal(condition); err != nil {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, goalPostResponse{Seq: s.currentSeq(), Status: "updated"})
		return
	}

	// No goal active: a plain prompt occupies the slot. Register the goal
	// now — RegisterGoal needs no run slot — so it exists the instant this
	// call returns, then retry the claim ONCE. This closes the race where
	// the prompt's own runPrompt tail (and its maybeAutoArmGoal auto-arm
	// check) runs between our failed claim above and this RegisterGoal: if
	// that happened, RegisterGoal is exactly what maybeAutoArmGoal was
	// waiting to see, and this retry either wins the now-free slot itself
	// (spawning the loop here) or loses it to that same auto-arm call
	// (which will already have spawned it) — either way the goal starts
	// exactly once, never zero times, never twice. See maybeAutoArmGoal's
	// doc comment for the other half of this argument.
	if err := sess.RegisterGoal(condition); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	st, ctx, fromSeq, code, _ := s.claimForPrompt(id)
	if code == 0 {
		s.mu.Lock()
		st.goalLoop = true
		s.mu.Unlock()
		s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
		go s.runGoal(ctx, id, st, condition, maxTurns)
		writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: fromSeq, Status: "started"})
		return
	}
	writeJSON(w, http.StatusAccepted, goalPostResponse{Seq: s.currentSeq(), Status: "armed"})
}

// runGoal drives one PursueGoal to completion, then flips the session back to
// idle. The loop's context is cancelled only by DELETE /goal (which journals
// goal.cleared BEFORE cancelling, see handleGoalDelete) or by Drain at
// shutdown; a context.Canceled result is therefore a deliberate stop, not a
// failure, and needs no session.error. Any other error is journaled as
// session.error. Message journaling piggybacks on the same syncMessages path
// as runPrompt.
//
// A worker-parked error (engine.IsGoalWorkerParked — either
// exhaustion tier exit-parking instead of clearing) falls into the default
// branch below like any other error: session.error, then turn.end via
// turnEndOutcome, which maps it to outcomeWorkerParked rather than the
// generic "error". This is NOT a clear — engine/goal.go's goal.parked record
// (journaled by PursueGoal itself, under its own lock, before returning —
// always ordered before this function's own session.error/turn.end) leaves
// the goal fully active; publishGoal folds it into goalTracker.pausedWorker,
// the third paused arm (pause_reason "worker_failure", see pauseView), which
// forces compositeState to idle exactly like a restart pause. This function
// deliberately does NOT auto-arm a fresh loop for it (see maybeDispatchQueued
// below and maybeAutoArmGoal's own doc comment for why runGoal's tail never
// auto-arms) — resume is entirely activity-driven, via the next plain
// prompt's own runPrompt tail.
//
// turn.end outcome is decided from the RESULT, not merely from err == nil:
// PursueGoal returns a nil error with Achieved:false in two cases that are
// emphatically not "completed" —
//
//   - MaxTurns exhausted without the evaluator ever returning MET (Reason
//     "max turns"): the goal gave up, it did not finish. That is recorded as
//     its own outcomeMaxTurnsExceeded, never "completed" — see PR #55 review
//     finding: keying turn.end on err == nil alone told a poller "idle
//     because done" for a goal that was never met, exactly the ambiguity
//     this primitive exists to remove.
//   - ClearGoal won a race against an in-flight worker retry or evaluator
//     call (Reason "goal cleared"), without the loop's own context ever
//     being cancelled. This is a clear, same as the context.Canceled path
//     below, just reached without cancellation — and the openapi contract
//     is that DELETE /goal (or any clear) never emits turn.end. So this case
//     suppresses the record entirely, matching the context.Canceled branch.
//
// The terminal session.status idle record emitted at the end of this
// function is the same record an SSE collector waits for as the session's
// "occupancy over" signal (collect-until-idle is the wire contract). DELETE
// /goal's clear-before-cancel ordering guarantees goal.cleared always
// precedes it in the journal — this function must never emit idle before a
// goal.cleared that is still in flight.
func (s *Server) runGoal(ctx context.Context, id string, st *sessionState, condition string, maxTurns int) {
	defer s.wg.Done()
	// See runPrompt's identical bracket for why: this treats the WHOLE
	// goal loop as one continuous "running" span for SessionManager's
	// purposes (matching the single busy/idle transition this function
	// already emits around it below) — a task notification arriving
	// while a goal loop is active is queued and picked up at the loop's
	// own next turn boundary automatically, since PursueGoal's underlying
	// turns go through the identical streamTurn/checkoutTaskNotificationsSegment
	// path any other turn does. msg is nil for ReportTurnEnd: a root
	// session's finalizeTurn branch never reads it (see finalizeTurn's
	// doc comment) — only a CHILD's does, and a child is never resident,
	// so runGoal never runs for one.
	s.sessMgr.ReportTurnStart(st.sess)
	res, err := st.sess.PursueGoal(ctx, condition, engine.GoalOptions{
		Registered: true,
		MaxTurns:   maxTurns,
		Evaluator:  s.opts.GoalEvaluator,
	})
	s.syncMessages(id)
	switch {
	case err == nil && res.Achieved:
		s.recordTurnEnd(id, "completed", nil)
	case err == nil && res.Reason == "goal cleared":
		// Cleared in flight without the context being cancelled: goal.cleared
		// is already journaled (ClearGoal/handleGoalDelete); no turn.end, same
		// contract as the context.Canceled case below.
	case err == nil:
		// Any other nil-error, non-achieved result is MaxTurns exhaustion —
		// PursueGoal's only remaining terminal case (see its doc comment).
		s.recordTurnEnd(id, outcomeMaxTurnsExceeded, nil)
	case errors.Is(err, context.Canceled):
		// Cleared via DELETE (goal.cleared already journaled) or drained.
	default:
		s.emitDurable(Event{Type: evtSessionError, SessionID: id, Error: err.Error()})
		s.recordTurnEnd(id, turnEndOutcome(err), err)
	}
	s.freeRunSlotAndEmitIdle(id, st)

	// resume: see runPrompt's identical variable for why ReportTurnEnd is
	// called here, after freeRunSlotAndEmitIdle, rather than immediately
	// after PursueGoal returns above — the same claimForPrompt race (a
	// concurrent notification delivery observing this session idle via
	// SessionManager while the real run slot is still held, getting
	// refused, and permanently stranding the notification) applies here
	// identically. msg is nil: a root session's finalizeTurn branch never
	// reads it (see finalizeTurn's doc comment) — only a CHILD's does,
	// and a child is never resident, so runGoal never runs for one.
	resume := s.sessMgr.ReportTurnEnd(id, nil, err)

	// A prompt queued after the loop's last turn-boundary drain (engine/
	// goal.go's PursueGoal only drains BETWEEN turns) but before the loop
	// actually terminated would otherwise sit queued indefinitely once the
	// loop is gone — this is that gap's dispatch hook. Unlike runPrompt's
	// tail, this is never followed by maybeAutoArmGoal: every terminal
	// PursueGoal outcome either deactivates the goal or leaves it in the
	// ordinary "active, re-armable via POST /goal" state it has always been
	// left in, never the "auto-arm is watching for this" state (see
	// maybeAutoArmGoal's own doc comment for why it is deliberately not
	// wired in here either).
	if s.maybeDispatchQueued(id, st) {
		return
	}
	// See runPrompt's identical call for why this is safe to call
	// synchronously here rather than "go resume()".
	if resume != nil {
		resume()
	}
}

// handleGoalDelete cancels an active goal loop: it clears the goal (journaling
// goal.cleared and resetting the engine's goal state), THEN cancels the loop
// context (stopping further turns) -- but ONLY when the run slot's current
// occupant IS a goal loop (st.goalLoop). A goal can be active while a PLAIN
// PROMPT holds the slot (the 202 "armed" path -- see handleGoalBusy's
// register-and-arm branch and maybeAutoArmGoal): in that window st.cancel
// belongs to the prompt, not to any loop, and cancelling it would abort that
// prompt's turn (typically the very turn that armed the goal via the `goal`
// session tool). See TestDeleteGoalDuringArmedPromptLeavesPromptRunning.
// Clearing the goal is enough in that case: maybeAutoArmGoal's own tail check
// (run when the prompt finishes) reads ActiveGoal() as false and no-ops, so
// no loop ever starts. Unknown session (not resident, no log on disk) is
// 404; a known session is 204 whether or not a goal was active (idempotent
// -- no goal.cleared is journaled when nothing was active).
//
// Ordering guarantee: goal.cleared is always journaled before the
// session.status idle record that ends that goal's occupancy (see runGoal and
// engine.Session.ClearGoal). This is why clear happens before cancel, not
// after: cancelling first would let the goal-loop worker's context-
// cancellation unwind — which ends in that terminal idle record — race the
// handler to the journal, and an SSE collector that reads until idle (the
// wire contract every client relies on) could see goal.set but never
// goal.cleared.
//
// Non-resident-but-on-disk case (issue #78): a session with no in-memory
// sessionState at all is not necessarily gone -- it may be exactly the
// boot-time restart-paused goal (pauseArmedGoalsAtBoot): active in the
// journal, paused/restart in goalState, with no loop ever attached in this
// process. That is precisely the case an operator needs DELETE /goal to be
// able to clear. The old code's `st != nil` guard skipped ClearGoal for it
// entirely -- still returning 204 (nothing to reject), but journaling
// nothing, never flipping engine.Session.goalActive, and leaving
// goalState[id].active true so the goal re-paused at the next boot. See
// TestDeleteGoalNonResidentClearsAndJournals for the red/green case.
func (s *Server) handleGoalDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	// Same guard as every other mutating per-{id} route (prompt_async, goal,
	// enqueue, compact, model, thinking): without it, the cold-load below
	// builds a SECOND *engine.Session over a live managed child's log and
	// journals goal.cleared on it concurrently with the child's own
	// Spawn-driven turn — see rejectManagedChildTurn's doc comment.
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		// Not resident: load it from disk exactly like claimForPrompt's cold
		// path -- LoadSession OUTSIDE s.mu (it may hit disk), then re-acquire
		// the lock and re-check for a resident that appeared in the
		// meantime (a concurrent POST /goal or /prompt_async racing us),
		// using that winner instead so two *engine.Session instances for the
		// same log are never both mutated. See claimForPrompt's doc comment
		// for the full race argument this mirrors.
		//
		// The freshly loaded session is made resident here (lastUsed set,
		// evictResidentLocked invoked) -- deliberately, rather than used
		// transiently and discarded. That keeps exactly one "load a cold
		// session into residency" shape in this server (claimForPrompt's),
		// instead of a second copy of its race handling that would need to
		// be kept in sync by hand. It costs nothing beyond one ordinary
		// MaxResident slot: the loaded session is idle (running/goalLoop are
		// both the zero value false), so it is immediately eviction-eligible
		// like any other idle resident on the next evictResidentLocked
		// sweep.
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		s.mu.Lock()
		var evicted []*engine.Session
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			evicted = s.evictResidentLocked()
		}
		s.mu.Unlock()
		releaseEvicted(evicted)
	}
	s.mu.Lock()
	var cancel context.CancelFunc
	if st.goalLoop {
		cancel = st.cancel
	}
	s.mu.Unlock()
	// Orderly shutdown: clear BEFORE cancel. ClearGoal journals goal.cleared
	// and emits the event synchronously, under the session's own lock,
	// before it returns (see engine.Session.ClearGoal) — so by the time
	// cancel() below wakes the goal-loop worker, goal.cleared is already in
	// the durable journal. Cancelling first would let the worker's unwind
	// (which ends in the terminal session.status idle record, see runGoal)
	// race the handler to the journal: on an unlucky scheduling the idle
	// record could land before goal.cleared, and an SSE collector that reads
	// until idle (the wire contract every client relies on) would never see
	// the clear. See TestGoalDeleteClearBeforeIdleRace, which forces that
	// worst case deterministically.
	//
	// ClearGoal journals goal.cleared (via OnEvent -> publishGoal, wired the
	// same way whether st.sess came from claimForPrompt's residency, this
	// handler's own cold-load branch above, or was already resident) and
	// resets the engine goal state; a no-op when no goal is active.
	st.sess.ClearGoal()
	if s.goalDeleteRace != nil {
		// Handed cancel (rather than a bare notification) so a test can force
		// the worst case unconditionally: fire the worker's unblock as early
		// as structurally possible — right here, before this function's own
		// cancel() below — and ride out its unwind to completion before
		// letting this handler proceed. See TestGoalDeleteClearBeforeIdleRace.
		s.goalDeleteRace(cancel)
	}
	if cancel != nil {
		cancel() // stop the loop; runGoal treats context.Canceled as a clean stop (no-op if the hook above already fired it)
	}
	w.WriteHeader(http.StatusNoContent)
}

// setModelResponseJSON is the openapi POST /session/{id}/model response shape:
// the session's model after the swap (unchanged echoes the current model — a
// same-model set is a durable no-op, see engine.Session.SetModel).
type setModelResponseJSON struct {
	Model message.ModelRef `json:"model"`
}

// handleSetModel swaps a session's MAIN model, decoupled from prompting — a
// client/dashboard-driven swap that never claims the run slot (SetModel is
// concurrency-safe, so it applies even while a turn is running; it takes effect
// on the NEXT request). Validation mirrors the `model` session tool: an empty
// model is 400, an unconfigured provider is 400 (SetModel would otherwise leave
// an unusable ref that wedges every later request), and an unknown session is
// 404. On success SetModel emits EventModelChanged, which Publish journals as
// the durable "model" record — the SAME single path the tool and per-request
// override use, so a swap journals exactly once.
func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	var body struct {
		Model message.ModelRef `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the session, loading a cold one into residency with the same
	// race handling handleGoalDelete uses (two *engine.Session for one log must
	// never both be mutated — SetModel persists the durable recModel record).
	// Resolve BEFORE validating the body so an unknown session is 404, not a
	// 400 that hides the missing session behind an empty-model complaint.
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		s.mu.Lock()
		var evicted []*engine.Session
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			evicted = s.evictResidentLocked()
		}
		s.mu.Unlock()
		releaseEvicted(evicted)
	}

	if body.Model.IsZero() {
		writeErr(w, http.StatusBadRequest, "model must be a non-empty \"provider/model\" ref")
		return
	}

	if !st.sess.ModelSupported(body.Model) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("provider %q is not configured", body.Model.Provider))
		return
	}
	// The context-window gate, checked BEFORE the swap for the same reason
	// as the provider gate above: a model with no known context window runs
	// with no context management at all, and SetModel would already have
	// persisted the durable recModel record by the time the first Prompt
	// failed. See engine.Session.CheckModel.
	if err := st.sess.CheckModel(body.Model); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st.sess.SetModel(body.Model)
	writeJSON(w, http.StatusOK, setModelResponseJSON{Model: st.sess.Model()})
}

// setThinkingResponseJSON is the POST /session/{id}/thinking response shape:
// the session's reasoning-effort level after the swap (a same-level set is a
// durable no-op, echoing the current level — see engine.Session.SetEffort).
type setThinkingResponseJSON struct {
	Effort message.Effort `json:"effort"`
}

// handleSetThinking swaps a session's reasoning-effort level, decoupled from
// prompting — a client/dashboard-driven swap that never claims the run slot
// (SetEffort is concurrency-safe and takes effect on the NEXT request). It
// mirrors handleSetModel: an unknown session is 404, an invalid effort value is
// 400. Unlike a model swap it has no provider-configured gate — effort is a
// per-request string the adapter maps, and whether the CURRENT model accepts a
// given level is a provider fact the engine cannot know from the ref alone; a
// dashboard that must gate per model (the boxes picker) holds its own mapping.
// An empty string is accepted and clears the level (EffortUnset, provider
// default) — the caller uses "off" to explicitly disable reasoning where a
// provider supports it. On success SetEffort emits EventEffortChanged, which
// Publish journals as the durable "effort" record.
func (s *Server) handleSetThinking(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	var body struct {
		Effort string `json:"effort"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve the session FIRST, loading a cold one into residency with the same
	// race handling handleSetModel uses (two *engine.Session for one log must
	// never both be mutated — SetEffort persists the durable recEffort record).
	// Resolve BEFORE validating the effort so an unknown session is 404, not a
	// 400 that hides the missing session behind an invalid-effort complaint —
	// exactly the order handleSetModel uses.
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		s.mu.Lock()
		var evicted []*engine.Session
		if ex := s.sessions[id]; ex != nil {
			st = ex
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			evicted = s.evictResidentLocked()
		}
		s.mu.Unlock()
		releaseEvicted(evicted)
	}

	effort, err := message.ParseEffort(body.Effort)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st.sess.SetEffort(effort)
	writeJSON(w, http.StatusOK, setThinkingResponseJSON{Effort: st.sess.Effort()})
}

// evictResidentLocked unloads the longest-idle non-busy sessions from
// s.sessions (this server's OWN residency bookkeeping) when the resident
// count exceeds Options.MaxResident. Busy sessions are never evicted;
// s.seen is retained so journal idempotency survives the unload.
//
// This frees s.sessions' own entry, but not necessarily the *Session object
// itself: a root is adopted into sessMgr (AdoptRoot) and never reaped
// (Reap only removes terminal LEAF children, never a root — see
// TestReapNeverCollectsAnOrdinaryRootEvenWhenChildless), so sessMgr keeps
// pinning the live object in memory regardless of this eviction — a
// pre-existing property of sessMgr's own lifecycle, unrelated to and
// unchanged by this function. What DOES depend on Server.lookup's ordering
// (sessMgr checked before a disk reload — see its own doc comment) is which
// object the NEXT read after eviction actually returns: the live,
// sessMgr-pinned one, not a fresh LoadSession reread from disk — eviction
// here narrows only this server's own bookkeeping, never a guarantee that
// the next access is served cold. Caller holds s.mu.
// It returns the evicted sessions for the caller to release, and never
// releases them itself: releaseEvicted takes each session's OWN mutex, and
// s.mu is a leaf lock with respect to that (see syncMessages' lock-ordering
// note, journal.go). Taking a session mutex under s.mu would close exactly
// the cycle that rule forbids — the engine holds a session's mutex while
// emitting events into Publish, which takes s.mu.
func (s *Server) evictResidentLocked() (evicted []*engine.Session) {
	excess := len(s.sessions) - s.opts.MaxResident
	if excess <= 0 {
		return nil
	}
	type cand struct {
		id   string
		last time.Time
	}
	cands := make([]cand, 0, len(s.sessions))
	for id, st := range s.sessions {
		if st.running {
			continue // busy sessions hold an in-flight prompt; keep them resident
		}
		cands = append(cands, cand{id, st.lastUsed})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].last.Before(cands[j].last) })
	for i := 0; i < excess && i < len(cands); i++ {
		if st := s.sessions[cands[i].id]; st != nil {
			evicted = append(evicted, st.sess)
		}
		delete(s.sessions, cands[i].id)
		// Release the request snapshot (it holds a full copy of the
		// assembled system segments). lastReqHash survives deliberately:
		// it is small and keeps hash-on-change journaling correct if the
		// session is later reloaded.
		delete(s.lastRequest, cands[i].id)
	}
	return evicted
}

// releaseEvicted closes the file descriptors of sessions eviction just
// dropped: a session's journal handle and its sidecar-index handle. Call it
// only after s.mu is released — see evictResidentLocked's own doc comment
// for why.
//
// The sessions stay usable. Eviction has already decided each one is idle
// and reloadable, and the next persist call reopens both handles through
// ensureLog. Without this, a process holds two descriptors for every
// session it has ever touched.
func releaseEvicted(evicted []*engine.Session) {
	for _, sess := range evicted {
		sess.ReleaseFiles()
	}
}

// handleAbort interrupts a session's in-flight prompt. Unknown session (not
// resident and no session log on disk) is 404; a known session is 204 whether
// or not anything was running (idempotent). A non-resident session cannot
// have a prompt in flight, so a bare existence check suffices — the abort
// never loads it into memory.
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	var cancel context.CancelFunc
	if st != nil {
		cancel = st.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Not a server-resident turn. A managed CHILD's own turn runs on its
	// SessionManager node.ctx (Spawn/Send), never in s.sessions at all —
	// st == nil here is the NORMAL, expected shape for a running child,
	// not evidence there is nothing to abort. Fall back to
	// sessMgr.AbortTurn — NOT sessMgr.Cancel (the full cascade
	// cancel_tree also uses, which an earlier revision of this handler
	// used here too, making abort indistinguishable from cancel_tree for
	// a child with descendants) — see AbortTurn's own doc comment for
	// the authoritative explanation of that distinction. Deliberately
	// scoped to an ACTUAL child (info.ParentID != "") — never a root: an
	// idle root reaching this point genuinely has nothing running (the
	// resident-turn branch above already covers a running one), and
	// canceling its SessionManager node here would be pointless churn
	// for a node that otherwise only transitions via ReportTurnStart/
	// Cancel/cancel_tree — the unconditional 204 fallback below is the
	// correct, unchanged behavior for that case. A live review caught
	// the child gap: an earlier revision of this handler silently did
	// nothing for a running child (st == nil, cancel == nil, but the
	// child DOES exist on disk so the 404 branch below never fired
	// either) — a misleading 204 while the child ran to completion
	// untouched.
	//
	// Keyed on sess.TaskParentID() (durable), not info.ParentID (live) — a
	// live review finding: adoptReloadedLocked leaves info.ParentID EMPTY
	// for a warm orphan (a genuine managed child adopted while its own
	// parent was untracked — see that method's own doc comment and
	// rejectManagedChildTurn's identical fix above), which used to skip
	// this branch entirely for exactly that child, falling through to the
	// unconditional 204 below — the same "misleading 204 while the child
	// ran untouched" bug this comment already describes as fixed for the
	// tracked-child case, left open for the untracked-parent one.
	if sess, ok := s.sessMgr.Session(id); ok && sess.TaskParentID() != "" {
		_ = s.sessMgr.AbortTurn(id) // errors only on an unknown id — impossible, Session just confirmed it
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if st == nil && !s.sessionOnDisk(id) {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// queueGetResponse is GET /session/{id}/queue: the durable-enqueue
// watermark plus the pending (undelivered) prompt queue in FIFO order.
// Queued is always present (empty array, never null) so consumers need no
// nil check. Seq is 0/omitted on plain prompt_async-queued entries.
type queueGetResponse struct {
	Watermark int64            `json:"watermark"`
	Queued    []queuedItemJSON `json:"queued"`
}

type queuedItemJSON struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
	Seq  int64  `json:"seq,omitempty"`
}

// handleQueueGet is the reconciliation read surface for durable enqueue
// (see docs/plans/2026-07-21-durable-enqueue.md): an upstream recovering
// from its own crash reads the watermark to learn which messages are
// already accepted rather than re-sending blind. It resolves the session
// via s.lookup — the same resolve-or-load helper handleGet uses for every
// other read endpoint: resident sessions answer from live state, and a
// non-resident session gets a transparent transient load (idle status,
// same as GET /session/{id}). Unlike handleQueueDelete's cold path, this
// transient load is deliberately NOT registered into residency and takes no
// run-slot claim — a read must never have those side effects. Resident and
// non-resident answers can never disagree: both are folds of the exact same
// on-disk journal.
func (s *Server) handleQueueGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	sess, ok := s.lookupSession(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	watermark, prompts := sess.QueueState()
	resp := queueGetResponse{Watermark: watermark, Queued: []queuedItemJSON{}}
	for _, p := range prompts {
		resp.Queued = append(resp.Queued, queuedItemJSON{ID: p.ID, Text: p.Text, Seq: p.Seq})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleQueueDelete is DELETE /session/{id}/queue (invariant 10): drains the
// session's durable prompt queue, journaling a prompt.dequeued(reason=
// "cleared") record for every pending item (see
// engine.Session.DequeueAllPrompts), then 204. Idempotent on an already-empty
// queue (still 204, nothing journaled). A running turn is left completely
// untouched — this clears only prompts waiting for a FUTURE turn, never
// cancels the current one (see POST /session/{id}/abort for that); a running
// goal loop's later turn-boundary drains simply see an empty queue, exactly
// as if the clear had raced ahead of them.
//
// The session is resolved exactly like handleGoalDelete's cold path, NOT via
// a bare s.lookup: a not-resident session is loaded from disk OUTSIDE s.mu,
// then s.mu is re-acquired to re-check for a resident that appeared in the
// meantime (a concurrent POST /prompt_async or /goal racing us), using that
// winner instead — registering the freshly loaded instance into residency
// otherwise — so DequeueAllPrompts below always mutates the exact
// *engine.Session instance every future drain (maybeDispatchQueued) actually
// reads. A bare transient load (the old behavior) would let a concurrent
// cold-load-and-register elsewhere win residency with its OWN, divergent
// instance: the clear would land on a copy nothing else ever touches again
// — 204 and even a durable prompt.dequeued(cleared) record, journaled via
// the shared OnEvent wiring, while the session that matters keeps
// dispatching the "cleared" prompts. See
// TestDeleteQueueColdSessionSurvivesResidencyRace and claimForPrompt's doc
// comment for the same race argument.
//
// DequeueAllPrompts takes only the engine session's own mutex and persists
// synchronously to its log, so this works correctly whether the resolved
// session is idle, busy with a prompt, or mid goal-loop, with no run-slot
// claim involved at all. Unknown session (not resident, no log on disk) is
// 404.
func (s *Server) handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	// Same guard as every other mutating per-{id} route: without it, the
	// cold-load below builds a SECOND *engine.Session over a live managed
	// child's log and persists prompt.dequeued records on it concurrently
	// with the child's own Spawn-driven turn — see rejectManagedChildTurn's
	// doc comment.
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	s.mu.Unlock()
	if st == nil {
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		if s.queueDeleteRace != nil {
			// Test-only seam: let a test force a real concurrent claim (a
			// prompt_async cold-loading and registering its own instance) to
			// land here deterministically. Nil in production.
			s.queueDeleteRace()
		}
		s.mu.Lock()
		var evicted []*engine.Session
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			st = &sessionState{sess: sess, lastUsed: time.Now()}
			s.sessions[id] = st
			evicted = s.evictResidentLocked()
		}
		s.mu.Unlock()
		releaseEvicted(evicted)
	}
	st.sess.DequeueAllPrompts("cleared")
	w.WriteHeader(http.StatusNoContent)
}

// compactResponseJSON is the openapi POST /session/{id}/compact response
// shape (docs/design/context-compaction.md §1): turns_folded is 0 (not an
// error) when there was nothing worth folding — see engine.CompactResult.
// SkipReason names exactly why (never set on a real fold): the three
// TurnsFolded==0 shapes are otherwise wire-identical, indistinguishable to
// an operator polling this endpoint even though only one of them
// (summarizer_empty) actually cost a billed provider call (review
// follow-up on PR #136, Finding C).
type compactResponseJSON struct {
	TurnsFolded int              `json:"turns_folded"`
	FirstID     string           `json:"first_id,omitempty"`
	LastID      string           `json:"last_id,omitempty"`
	Summary     *message.Message `json:"summary,omitempty"`
	SkipReason  string           `json:"skip_reason,omitempty"`
}

// handleCompact is POST /session/{id}/compact (docs/design/context-
// compaction.md §1 "Explicit: POST /session/{id}/compact"): always
// available regardless of the automatic threshold. It claims the session's
// single run slot exactly like prompt_async/goal (409 if already running,
// 503 if draining) — compaction never runs concurrently with a turn — then
// runs synchronously (the response carries the full result, so there is no
// async job to poll). Optional JSON body {"keep_turns": N, "model": "..."}
// overrides Config.CompactionKeepTurns/CompactionModel for this call only;
// keep_turns has a hard floor of 1 — 0 or negative is a 400, never silently
// clamped, so a caller's mistake is visible rather than silently ignored.
//
// Its tail calls maybeDispatchQueued then maybeAutoArmGoal, the same
// order/precedence runPrompt's tail uses (queue beats goal auto-arm): a
// prompt queued or a goal armed while compact ran must drain/start the
// instant this call releases the run slot, not wait for some later
// runPrompt/runGoal tail to happen to fire first.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if s.rejectManagedChildTurn(w, id) {
		return
	}
	var body struct {
		KeepTurns *int   `json:"keep_turns"`
		Model     string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.KeepTurns != nil && *body.KeepTurns <= 0 {
		writeErr(w, http.StatusBadRequest, "keep_turns must be >= 1")
		return
	}
	var model message.ModelRef
	if body.Model != "" {
		m, err := message.ParseModelRef(body.Model)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		model = m
	}

	st, ctx, _, code, holder := s.claimForPrompt(id)
	if code != 0 {
		switch {
		case code == http.StatusConflict && holder != "":
			writeErr(w, code, fmt.Sprintf("workdir busy: held by session %s", holder))
		case code == http.StatusConflict:
			writeErr(w, code, "session is busy with another prompt")
		case code == http.StatusServiceUnavailable:
			writeErr(w, code, "server shutting down")
		default:
			writeErr(w, http.StatusNotFound, "no such session")
		}
		return
	}
	// Deferred (not called bare at the tail) so it runs even if Compact or
	// either of the tail's own maybeDispatchQueued/maybeAutoArmGoal calls
	// panics -- a bare call would never execute past a panic, leaking this
	// claim's wg.Add and hanging Drain forever. A defer here still runs
	// strictly after those tail calls (defers fire after the function body's
	// remaining statements, on any return path — panic or normal), so the
	// wg.Add-before-wg.Done ordering those calls rely on (see the comment at
	// the call site below) is unchanged; this only adds panic-safety, same
	// shape as runPrompt's/runGoal's own `defer s.wg.Done()`. See
	// TestCompactPanicReleasesClaim.
	defer s.wg.Done()
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})

	// ReportTurnStart/ReportTurnEnd bracket this claim exactly like
	// runPrompt's identical bracket (see its own doc comment for the full
	// reasoning) — an earlier revision of this handler claimed the run
	// slot without ever reporting it to SessionManager at all. A live
	// review caught what that breaks: triggerResumeLocked flips a root
	// to StatusRunning BEFORE calling its ExternalRunner
	// (resumeSessionForTaskNotification -> runOrQueueText ->
	// claimForPrompt), and runOrQueueText treats ANY non-404 claim
	// failure as handled=true — including THIS handler's own StatusConflict
	// claim above. Compact silently holding the slot with no bracket meant
	// a task notification arriving during a compact call saw the root
	// "handled" by a scheduler that was never actually going to call
	// ReportTurnEnd for it: the notification was accepted and then never
	// delivered, permanently — queue-or-resume dead for that root until an
	// unrelated human prompt happened to drain it.
	s.sessMgr.ReportTurnStart(st.sess)

	opts := engine.CompactOptions{Model: model}
	if body.KeepTurns != nil {
		opts.KeepTurns = *body.KeepTurns
	}
	res, err := st.sess.Compact(ctx, opts)
	// Session.Compact's own emits (EventMessage for the summary, then
	// EventHistoryCompacted — see engine/compact.go) already flowed through
	// Publish synchronously by the time Compact returns, journaling the
	// summary message and the durable history.compacted record in that
	// order (see publishHistoryCompacted). syncMessages here is a harmless,
	// idempotent extra pass — the same belt-and-suspenders every other
	// handler's tail already relies on.
	s.syncMessages(id)

	s.freeRunSlotAndEmitIdle(id, st)

	// ReportTurnEnd runs AFTER freeRunSlotAndEmitIdle — see runPrompt's
	// identical ordering and its doc comment for why (the real run slot
	// must be free before SessionManager's view of this root can show
	// idle/done, or a concurrent resume attempt racing in between would
	// find the slot still held and permanently strand its notification).
	// msg is nil: Compact never produces the kind of turn message
	// ReportTurnEnd's root branch would read (see its own doc comment —
	// only a CHILD's finalizeTurn branch reads msg, and a child never
	// reaches this handler at all, rejectManagedChildTurn above already
	// refused it).
	resume := s.sessMgr.ReportTurnEnd(id, nil, err)

	// Same drain-then-auto-arm-then-resume precedence as runPrompt's tail
	// (invariant 5): a prompt queued (or a goal armed) while this compact
	// call ran must not sit stranded just because the run slot happened to
	// be released by compact instead of an ordinary prompt or goal turn —
	// see maybeDispatchQueued/maybeAutoArmGoal's own doc comments for the
	// full race analysis, identical here. wg.Done for THIS claim is the
	// deferred call above, which fires after these tail calls (defers fire
	// after the function body's remaining statements), so the WaitGroup
	// never transiently reads zero between this claim's release and a
	// dispatched/auto-armed one's own wg.Add (mirrors runPrompt's
	// defer-at-function-exit shape) — and, unlike a bare call here, still
	// fires even if one of these tail calls (or Compact above) panics.
	if !s.maybeDispatchQueued(id, st) {
		if resume != nil {
			resume()
		} else {
			s.maybeAutoArmGoal(id, st)
		}
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, plugin.SanitizeSessionError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, compactResponseJSON{
		TurnsFolded: res.TurnsFolded,
		FirstID:     res.FirstID,
		LastID:      res.LastID,
		Summary:     res.Summary,
		SkipReason:  res.SkipReason,
	})
}

// sessionOnDisk reports whether a session log for id exists in the session
// directory, without loading the session.
func (s *Server) sessionOnDisk(id string) bool {
	return engine.SessionExists(s.opts.SessionDir, id)
}

// lookup resolves a session for read endpoints and returns its whole
// liveSession snapshot: the in-memory session (with live status) if
// present, else a transparent load from disk (always idle).
//
// It returns the snapshot rather than a bare (session, status) pair so a
// handler that also needs the session's lineage — buildSession, via
// lineageJSONFor — reads it out of the SAME snapshot instead of taking a
// second, later SessionManager read of its own. GET /session used to do
// exactly that: the body's session fields came from lookup's manager read
// and its lineage block from lineageJSONFor's, so a Reap landing between
// the two described two different nodes in one response.
//
// Order matters, and used to be wrong for every child. A root goes through
// s.sessions (this server's own residency bookkeeping) first, unchanged. For
// anything NOT in s.sessions — every child, always, since a child is never
// registered there — this used to fall straight to s.opts.LoadSession(id), a
// COLD DISK REREAD, before ever consulting sessMgr (this process's own live,
// in-memory tracker for anything it is actively running, root or child).
// That cold read wins near-unconditionally in the steady state: a child's
// log already exists on disk essentially immediately (Spawn persists on
// first Prompt append, same as any session), so LoadSession(id) succeeds and
// returns for the WHOLE remaining lifetime of a long-running child, never
// once falling through to the sessMgr branch the old code's own doc comment
// claimed was "the ONLY path that ever resolves a child session this
// process's SessionManager tracks" — true only in the narrow startup race
// that comment described, false for every child from then on. That cold
// reload is not merely wasteful (re-parsing the whole log
// on every single GET, including the box console's own 2s/2.5s subagent-card
// poll): LoadSession's scanLog replay runs message.ResolveOrphanToolCalls
// (store.go) unconditionally — a crash-repair backstop that synthesizes an
// is_error tool_result for ANY tool_use with no matching result yet, correct
// for a session that genuinely died mid-turn, WRONG for one simply still
// running its own in-flight tool call. A live child executing an ordinary
// long tool call (a subagent's `bash sleep 45`, the case that surfaced this)
// had every poll of GET /session/{id}/message re-derive a fabricated
// crashed/errored result for its perfectly healthy in-flight call, rendering
// it as failed in the console for as long as it kept running — and, since a
// subagent card's transcript poll only re-fetches while its own lineage
// status reads "running" (boxes' use-child-transcript.ts), a poll landing
// during that window right before the child's genuine completion could leave
// that false error as the last thing ever fetched, with nothing left to
// self-heal it.
//
// The fix: check sessMgr FIRST. Anything it is actively tracking — root or
// child, running, idle-between-turns, or terminal-but-not-yet-reaped — has
// its live, authoritative Go object read directly, with no disk round trip
// and no repair artifacts. Only an id sessMgr has never adopted (or has
// since forgotten — e.g. this process restarted and the id has not been
// re-adopted onto a NEW node yet) ever reaches the disk-load fallback below.
// lookupSession resolves id for a read that needs the session object and
// nothing else — no status, no lineage: the same three tiers lookup uses
// (residency, SessionManager, disk), minus the manager read when residency
// already answers. handleMessages, handleQueueGet, handleJournal, and the
// plugin client API all discard the rest of the snapshot, and two of them
// are polled, so paying the box-global SessionManager.mu and a discarded
// SessionNode copy per poll is waste (a live review finding — the same
// class already fixed on syncMessages and waitSnapshot).
//
// A caller that renders lineage must use lookup instead: lineageJSONFor
// reads the manager half even for a resident session.
func (s *Server) lookupSession(id string) (*engine.Session, bool) {
	if sess := s.liveSessionObject(id); sess != nil {
		return sess, true
	}
	sess, err := s.opts.LoadSession(id)
	if err != nil {
		return nil, false
	}
	return sess, true
}

func (s *Server) lookup(id string) (liveSession, bool) {
	lv := s.resolveLive(id)
	if lv.session() != nil {
		return lv, true
	}
	if sess, err := s.opts.LoadSession(id); err == nil {
		return lv.withLoaded(sess), true
	}
	return liveSession{}, false
}

// residentSession returns the resident *engine.Session for id, or nil if the
// session is not currently resident. Unlike claimForPrompt, this never loads
// from disk and never claims the run slot — it is only used by
// handleGoalBusy, whose caller (handleGoal) reaches it exclusively when
// claimForPrompt just reported id as resident and running.
func (s *Server) residentSession(id string) *engine.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.sessions[id]; st != nil {
		return st.sess
	}
	return nil
}

// currentSeq reads the durable journal's current sequence counter, for a
// response that (unlike claimForPrompt's fromSeq) does not correspond to a
// run-slot claim.
func (s *Server) currentSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// claimForPrompt atomically resolves the session for id and claims its single
// prompt slot. It replaces the old getOrLoad-then-claim two-step on the write
// path, which left a gap between resolving the resident session and setting
// st.running: in that gap a concurrent evictResidentLocked could unload the
// session and a racing cold-load could insert a second, divergent
// *engine.Session for the same log. Here the resolve and the claim complete in
// ONE s.mu critical section, so a claimed (running) session can never be evicted
// (evictResidentLocked skips running sessions) and no duplicate can appear.
//
// Loading an on-disk session may block, so it happens outside the lock; the
// re-lock then re-checks both that no resident appeared meanwhile and that Drain
// has not begun. On success it sets st.running, records the cancel func, and
// does wg.Add(1) — all before releasing the lock. The wg.Add sits in the same
// critical section that observed draining==false, so by mutex ordering it always
// happens-before Drain's draining=true (and thus before wg.Wait): a WaitGroup
// Add after Wait is impossible, and a prompt admitted during drain is impossible.
//
// On failure it returns a non-zero HTTP status and leaves nothing claimed:
// StatusServiceUnavailable (draining), StatusNotFound (unknown session), or
// StatusConflict (already running, or another running session holds the same
// workdir — see workdirHolderLocked — in which case holder names it). code ==
// 0 means success.
//
// A successful claim also resets st.goalLoop to false (see the field's doc
// comment in server.go): the claim site is the natural place for this
// because every occupant that wants goalLoop true sets it only after this
// function returns, so the reset here can never race a legitimate true. This
// makes the flag self-contained rather than relying solely on every prior
// occupant's tail having reset it.
func (s *Server) claimForPrompt(id string) (st *sessionState, ctx context.Context, fromSeq int64, code int, holder string) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusServiceUnavailable, ""
	}
	st = s.sessions[id]
	if st == nil {
		// Not resident: load from disk with the lock released, then re-acquire.
		s.mu.Unlock()
		sess, err := s.opts.LoadSession(id)
		if err != nil {
			return nil, nil, 0, http.StatusNotFound, ""
		}
		loaded := &sessionState{sess: sess, lastUsed: time.Now()}
		s.mu.Lock()
		// Drain may have begun during the unlocked load: re-check before we
		// insert or claim, so no wg.Add slips past the admission gate.
		if s.draining {
			s.mu.Unlock()
			return nil, nil, 0, http.StatusServiceUnavailable, ""
		}
		if ex := s.sessions[id]; ex != nil {
			st = ex // a resident appeared while we loaded; use the winner
		} else {
			s.sessions[id] = loaded
			st = loaded
		}
	}
	if st.running {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusConflict, ""
	}
	if h := s.workdirHolderLocked(id, st); h != "" {
		s.mu.Unlock()
		return nil, nil, 0, http.StatusConflict, h
	}
	fromSeq = s.seq
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	// Reset here too, not just at every prior occupant's tail: this makes the
	// claim self-contained rather than trusting every past and future tail to
	// reset it, and it is always correct because every runGoal-spawning call
	// site below sets it back to true only AFTER claimForPrompt returns (never
	// before), so this can never stomp a legitimate true.
	st.goalLoop = false
	s.wg.Add(1)
	// A cold load grew the resident set; cap it now. st is running, so
	// evictResidentLocked will not evict the session we just claimed.
	evicted := s.evictResidentLocked()
	s.mu.Unlock()
	releaseEvicted(evicted)
	return st, ctx, fromSeq, 0, ""
}

// workdirHolderLocked returns the session ID of another RUNNING session that
// holds the same workdir as st, unless st itself or that other session opted
// into share_workdir, or either is a 'worktree'-isolation session — in which
// case it returns "" (no conflict). The claim exists to stop two sessions
// interleaving writes in one shared tree; a 'worktree' session never shares
// its tree with anything (each gets its own dedicated git worktree, so their
// WorkDir()s can never even be equal), so the claim is moot for it by
// construction — this check is belt-and-suspenders, not load-bearing. Caller
// holds s.mu.
func (s *Server) workdirHolderLocked(id string, st *sessionState) string {
	if st.shareWorkdir || st.isolation == isolationWorktree {
		return ""
	}
	wd := st.sess.WorkDir()
	for otherID, other := range s.sessions {
		if otherID == id || !other.running || other.shareWorkdir || other.isolation == isolationWorktree {
			continue
		}
		if other.sess.WorkDir() == wd {
			return otherID
		}
	}
	return ""
}

// handleEnd ends a session: removes it from residency and, for a
// 'worktree'-isolation session, tears its git worktree down — removed when
// it has no uncommitted changes and no unpushed commits, otherwise kept in
// place with its path journaled (workdir.worktree_kept) so work is never
// destroyed (see teardownWorktree). A busy session is 409 (ripping the
// worktree out from under an in-flight tool call would corrupt whatever it
// is mid-writing — abort it first); unknown (not resident, no log on disk)
// is 404; ending a 'shared' or already-ended session is a plain 204.
//
// If id has live subagent-sessions children, they are cascade-canceled
// (sessMgr.Cancel, the same cascade DELETE .../cancel_tree already uses)
// before id itself is removed — a live review finding: this used to only
// ever touch server residency (s.sessions), never sessMgr, so ending a
// parent with a still-running child silently orphaned it: the child kept
// running to completion with no one left to ever check out its result
// (its parent's own row was already gone from s.sessions). Guarded on
// having children at all, mirroring cancel_tree's own scope, so an
// ordinary childless DELETE never recolors a session's SessionManager
// status to "canceled" it wasn't already heading toward.
func (s *Server) handleEnd(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	st := s.sessions[id]
	if st == nil {
		s.mu.Unlock()
		if !s.sessionOnDisk(id) {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		s.endSubagentLineage(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if st.running {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "session is busy; abort it before ending it")
		return
	}
	wt := st.worktree
	delete(s.sessions, id)
	delete(s.lastRequest, id)
	s.mu.Unlock()
	// id itself is now confirmed idle (the running check above passed) —
	// safe to cascade-cancel any live children without racing id's own
	// in-flight turn.
	s.endSubagentLineage(id)
	if wt != nil {
		s.teardownWorktree(id, wt)
	}
	w.WriteHeader(http.StatusNoContent)
}

// endSubagentLineage settles id's sessMgr-side bookkeeping as part of
// ending it — two live review findings on handleEnd, both stemming from
// the same root cause: ending a session used to only ever touch server
// residency (s.sessions), never sessMgr.
//
//  1. Cascade-cancels live children (the same sessMgr.Cancel cascade
//     DELETE .../cancel_tree already uses): a still-running child of a
//     deleted parent used to be silently orphaned — left running to
//     completion with no one left to ever check out its result, since
//     its parent's own row was already gone. Guarded on having children
//     at all, mirroring cancel_tree's own scope, so an ordinary DELETE
//     of a plain, childless session never recolors its SessionManager
//     status to "canceled" it wasn't already heading toward.
//
//  2. Best-effort ForgetRoot: a root's sessionNode (and the *Session it
//     pins) used to survive in sessMgr's m.nodes for the rest of the
//     PROCESS's life even after its caller explicitly deleted it here.
//     Errors are expected and silently ignored: id may not be a root at
//     all (a child — never ForgetRoot's job), may not be sessMgr-tracked
//     at all (a session that predates this feature), or may still have
//     live children at the instant this runs (the cascade-cancel above
//     only CANCELS them, it does not remove them — they stay in
//     m.nodes, canceled, until Reap's own bottom-up sweep eventually
//     collects them). That last case is NOT a dead end: ForgetRoot arms
//     the node's pendingForget flag on refusal specifically so Reap's
//     own sweep also collects THIS now-childless root once it gets
//     there — see pendingForget's own doc comment (session_manager.go)
//     for the full mechanism; a live review caught the gap where
//     nothing ever revisited a root refused for exactly this reason.
//     None of these are caller-visible failures: DELETE already
//     succeeds either way.
//
// Callers must ensure id itself is not concurrently mid-turn before
// calling this (handleEnd's resident branch checks st.running first; its
// non-resident branch has no server-tracked in-flight turn to race in
// the first place).
func (s *Server) endSubagentLineage(id string) {
	if info, ok := s.sessMgr.Info(id); ok && len(info.Children) > 0 {
		_ = s.sessMgr.Cancel(id) // only error is ErrUnknownSession, unreachable: id was just found tracked above
	}
	_ = s.sessMgr.ForgetRoot(id)
}

// teardownWorktree decides a 'worktree'-isolation session's fate at session
// end: removed (and evtWorktreeRemoved journaled) when worktreeClean reports
// no uncommitted changes and no unpushed commits; otherwise — including when
// the clean check itself fails, or removal unexpectedly fails after a clean
// read — left exactly where it is and evtWorktreeKept is journaled with its
// path, so an orchestrator polling the event stream can find and finish the
// work instead of losing it.
func (s *Server) teardownWorktree(sessionID string, wt *worktreeInfo) {
	clean, err := worktreeClean(wt.path, wt.baseCommit)
	if err == nil && clean {
		if rmErr := removeWorktree(wt.repoRoot, wt.path); rmErr == nil {
			os.Remove(wt.metaPath)
			s.emitDurable(Event{Type: evtWorktreeRemoved, SessionID: sessionID, WorktreePath: wt.path})
			return
		}
	}
	s.emitDurable(Event{Type: evtWorktreeKept, SessionID: sessionID, WorktreePath: wt.path})
}

// buildSession assembles the Session shape without holding s.mu across engine
// calls: session fields come from the engine, seq from the journal.
//
// It takes the caller's whole liveSession snapshot, not a (session, status)
// pair, so the response body and its lineage block describe ONE instant —
// see lookup's own doc comment for the split-instant response that shape
// replaces. A caller with no snapshot of its own builds one with
// Server.resolveLive.
//
// Precondition: lv.session() is non-nil. Every caller establishes it —
// lookup and lookupSpawned report ok only for a resolved session, and
// handleList passes a residency entry it already read. There is
// deliberately no nil guard: a zero *engine.Session renders a
// plausible-looking but malformed body (blank id and model beside a
// populated lineage block), which a live review already rejected once on
// handleSpawnChild. A caller with no session must return an error instead
// of calling this.
func (s *Server) buildSession(lv liveSession) sessionJSON {
	sess := lv.session()
	status := lv.status()
	id := sess.ID
	s.mu.Lock()
	seq := s.sessionSeqLocked(id)
	goal := goalJSONFrom(s.goalState[id])
	lastTurn := s.lastTurnJSONLocked(id)
	s.mu.Unlock()
	return sessionJSON{
		ID:                id,
		CreatedAt:         sess.CreatedAt(),
		Model:             sess.Model(),
		Effort:            sess.Effort(),
		Status:            status,
		State:             compositeState(status == "busy", goal != nil && goal.Active, forcesIdlePause(goal)),
		Messages:          len(sess.History()),
		Seq:               seq,
		Goal:              goal,
		WorkDir:           sess.WorkDir(),
		LastTurn:          lastTurn,
		Usage:             usageJSONForSession(sess),
		LastActivityAt:    sess.LastActivityAt(),
		ParentSession:     sess.ParentSession(),
		CompactionCount:   sess.CompactionCount(),
		LastCompactedAt:   sess.LastCompactedAt(),
		Plugins:           sess.Plugins(),
		Queued:            len(sess.QueuedPrompts()),
		Lineage:           lineageJSONFor(lv),
		SubscriptionUsage: sess.SubscriptionUsage(),
	}
}

// buildSessionFromIndex renders the wire Session for an id NO live source
// in this process holds, from its durable metadata index alone.
//
// It is buildSession's cold twin, and the split is along one line: what has
// a durable source and what does not. The index answers everything the
// session log records — created/activity timestamps, model, effort,
// workdir, message count, usage, durable goal state, queue depth,
// compaction, lineage. The three fields that are process-local answer
// exactly as they do in buildSession, from the same server-side maps:
// journal seq, the goal presentation (goalTracker, which survives a
// restart through the event journal, not the session log), and last_turn.
//
// Status is always "idle" here, by construction: a session running a turn
// in this process is, by definition, live in it, so it never reaches this
// path. That matches what the old cold path reported — a disk-loaded
// session contributed no status either (see liveSession.status).
//
// Plugins come from Options.Plugins, because plugins are process
// configuration rather than durable session state, and an index has no
// Session to ask.
func (s *Server) buildSessionFromIndex(ix engine.SessionIndex) sessionJSON {
	s.mu.Lock()
	seq := s.sessionSeqLocked(ix.ID)
	goal := goalJSONFrom(s.goalState[ix.ID])
	lastTurn := s.lastTurnJSONLocked(ix.ID)
	s.mu.Unlock()
	return sessionJSON{
		ID:        ix.ID,
		CreatedAt: ix.CreatedAt,
		Model:     ix.Model,
		Effort:    ix.Effort,
		Status:    "idle",
		State:     compositeState(false, goal != nil && goal.Active, forcesIdlePause(goal)),
		Messages:  ix.Messages,
		Seq:       seq,
		Goal:      goal,
		WorkDir:   ix.WorkDir,
		LastTurn:  lastTurn,
		Usage: usageJSON{
			InputTokens:      ix.Usage.InputTokens,
			OutputTokens:     ix.Usage.OutputTokens,
			CacheReadTokens:  ix.Usage.CacheReadTokens,
			CacheWriteTokens: ix.Usage.CacheWriteTokens,
			Messages:         ix.Messages,
			LastInputTokens:  ix.LastInputTokens,
		},
		LastActivityAt:  ix.LastActivityAt,
		ParentSession:   ix.ParentSession,
		CompactionCount: ix.CompactionCount,
		LastCompactedAt: ix.LastCompactedAt,
		Plugins:         s.pluginInfo(ix.ID),
		Queued:          ix.Queued,
		Lineage:         coldLineageJSON(ix.TaskParentID, ix.TaskAgentType, ix.TaskDepth, ix.SpawnedChildIDs),
		// SubscriptionUsage has no durable source (see sessionJSON's own
		// field doc comment) — null on every cold read, by construction.
		SubscriptionUsage: nil,
	}
}

// pluginInfo reports a session's configured plugins for a cold read (see
// Options.Plugins). It never returns nil: the wire contract for
// Session.plugins is an array, empty when nothing is configured.
func (s *Server) pluginInfo(sessionID string) []plugin.Info {
	if s.opts.Plugins == nil {
		return []plugin.Info{}
	}
	if infos := s.opts.Plugins(sessionID); infos != nil {
		return infos
	}
	return []plugin.Info{}
}

// lineageJSONFor returns lv.id's subagent-sessions lineage. It reads only
// the caller's already-taken liveSession snapshot (see that type's own doc
// comment) — never sessMgr again — so the lineage block always describes
// the same node the rest of the response does. It is independent of
// whether the session itself came from s.sessions, from SessionManager, or
// from a fresh disk load: a child session's log lives in the same SessionDir a plain
// LoadSession(id) already finds (Spawn's child Config inherits its
// parent's SessionDir verbatim), so GET /session/{id} already resolves a
// child with zero other changes — this only adds the tree metadata on
// top.
//
// Two sources, in order:
//
//  1. The snapshot's manager half (lv.isManaged): this process currently
//     tracks id in memory — the
//     full, precise lineage snapshot (live status, result/fail_reason),
//     PLUS Depth and Children, each reconciled against sess's own durable
//     record below rather than trusted from the live snapshot alone (see
//     their own paragraphs).
//
//  2. A durable cold fallback, built directly from sess's own persisted
//     Config.TaskParentID()/TaskAgentType()/TaskDepth()/SpawnedChildIDs()
//     (engine/store.go restores these on every LoadSession, unconditionally
//     — no SessionManager adoption needed) — a live review finding:
//     without this, a child Reaped or never touched since a process
//     restart reported NO lineage at all on GET /session/{id}, even though
//     its lineage is fully durable on disk; a caller had no way to learn
//     "this session has a parent" without first forcing a reload via an
//     unrelated write (a prompt/send call). Status/Result/FailReason still
//     have no durable source and are omitted rather than guessed — see
//     lineageJSON's own field comments for why that's safe on the wire
//     (omitempty distinguishes "unknown" from every real zero value).
//
// Depth: branch 1 (warm) reports info.Depth VERBATIM — never re-derived or
// overridden here from sess.TaskDepth() a second time. This is a
// deliberate adjudication, not an oversight: info.Depth IS the
// ENFORCEMENT-effective depth, the exact value adoptReloadedLocked
// computed and TaskToolAllowed gates this session's own `task` tool
// against RIGHT NOW (see that method's own doc comment for its full
// preference order, durable TaskDepth included). A caller reading
// lineage.depth needs to be able to PREDICT what this session can
// actually do — whether it can spawn a child of its own — not a
// separately-recomputed "more correct" number that could disagree with
// what is actually enforced. An earlier revision of this function
// re-preferred sess.TaskDepth() independently here, which happened to
// agree with info.Depth in every case that revision's own tests covered,
// but was two sources of truth for one fact by construction, free to
// drift the moment either side's derivation changed — a live review
// finding. Durable TaskDepth still matters, just not as a SECOND wire-side
// override: adoptReloadedLocked already folds it into info.Depth as
// enforcement's own PRIMARY source, and it remains the direct answer on
// the cold branch below, which has no live info.Depth to defer to at all.
//
// Sentinel semantics live where depth is actually computed
// (adoptReloadedLocked, engine/session_manager.go) — a REFUSAL SENTINEL
// (m.maxDepth) substituted whenever a node's true depth is unrecoverable
// can propagate forward through a live-tracked-but-poisoned ancestor (a
// legacy node with no durable TaskDepth of its own, adopted with ITS OWN
// parent untracked) into a legacy descendant that also has no durable
// depth of its own — see TestSentinelPoisonedChainWireDepthMatchesEnforcement
// (session_tree_test.go), which proves the wire and TaskToolAllowed agree
// on that exact propagated value, by construction, since both read the
// same info.Depth.
//
// Children: sessionNode.children (branch 1's info.Children) is the LIVE
// in-memory tree only — Reap() explicitly drops a settled leaf from its
// parent's Children list once reaped (see Reap's own doc comment: "Removing
// a leaf also drops its id from its parent's Children list"), while
// sess.SpawnedChildIDs() is the durable, append-only, NEVER-shrinking
// record of every child this session ever spawned (persisted at spawn
// time, unconditionally — see persistTaskSpawnLocked). A live audit
// caught exactly this gap: a parent whose only child had already settled
// and been reaped reported "children":[] even though SpawnedChildIDs()
// still listed it. childIDsUnion merges the two (durable first, in spawn
// order — see its own doc comment for why durable-first, not live-first,
// is what preserves spawn order overall) so a caller always sees every
// child this session ever spawned, live or long since reaped — never only
// whichever half of the bookkeeping happens to still be resident.
//
// The cold-fallback branch (2) has no live tree to cross-check against,
// so it does NOT run Children through childIDsUnion — an empty
// sess.SpawnedChildIDs() there is reported as genuinely unknown (nil),
// not confirmed zero — see lineageJSON.Children's own doc comment for
// why (a legacy, pre-recTaskSpawned log can lose that record entirely).
//
// nil only when NEITHER source has anything: a genuine root (empty
// TaskParentID) or a session predating this feature.
func lineageJSONFor(lv liveSession) *lineageJSON {
	sess := lv.session()
	if info := lv.info; lv.isManaged {
		// info.Depth reported verbatim — see this function's own doc
		// comment ("Depth:" paragraph) for why the wire must match
		// enforcement exactly rather than re-preferring sess.TaskDepth()
		// a second time here.
		depth := info.Depth
		// info.ParentID is empty when adoptReloadedLocked adopted id with
		// its parent untracked (the durable-TaskDepth branch sets depth but
		// never attachTo). The durable TaskParentID still names the true
		// parent. Fall back to it. Without this fallback, a warm orphan
		// reports depth > 0 with no parent_id — a shape lineageJSON.Depth's
		// doc comment rules out. A genuine root never reaches
		// adoptReloadedLocked's non-root branch and has an empty
		// TaskParentID, so the fallback changes nothing for roots.
		parentID := info.ParentID
		if parentID == "" {
			parentID = sess.TaskParentID()
		}
		return &lineageJSON{
			ParentID:   parentID,
			Depth:      depth,
			Status:     string(info.Status),
			Children:   childIDsUnion(info.Children, sess.SpawnedChildIDs()),
			AgentType:  info.AgentType,
			Result:     info.Result,
			FailReason: info.FailReason,
			FailKind:   info.FailKind,
		}
	}
	parentID := sess.TaskParentID()
	if parentID == "" {
		return nil
	}
	// Children is sess.SpawnedChildIDs() verbatim here — NOT run through
	// childIDsUnion, which always normalizes an empty result to a non-nil
	// []string{} ("known: zero"). A live review finding: SpawnedChildIDs()
	// is complete only for a log written AFTER recTaskSpawned records
	// shipped — a parent whose log predates that record, but genuinely did
	// spawn children before this process ever adopted it, has an empty
	// SpawnedChildIDs() with no way to tell that apart from a parent that
	// truly never spawned anything. This cold branch has no live tree to
	// cross-check against (unlike the warm branch above, where
	// info.Children — this process's OWN adoption history — corroborates
	// an empty durable result), so an empty SpawnedChildIDs() here is
	// honestly UNKNOWN, not confirmed zero: left nil, serializing as
	// "children":null, exactly like this same cold branch already treats
	// Status/Result/FailReason. A NON-empty SpawnedChildIDs() is still a
	// reliable positive signal regardless of log vintage (a durably
	// recorded child is a durably recorded child), so that case is
	// reported normally, unmodified.
	return coldLineageJSON(parentID, sess.TaskAgentType(), sess.TaskDepth(), sess.SpawnedChildIDs())
}

// coldLineageJSON builds the durable-only lineage block from the four
// fields a session log carries, whether they were read off a loaded
// Session (lineageJSONFor's cold branch) or off its metadata index
// (buildSessionFromIndex). Both callers describe a session no live source
// in this process holds, so both omit the live-only fields identically.
func coldLineageJSON(parentID, agentType string, depth int, children []string) *lineageJSON {
	if parentID == "" {
		return nil
	}
	return &lineageJSON{
		ParentID:  parentID,
		Depth:     depth,
		AgentType: agentType,
		Children:  children,
	}
}

// childIDsUnion merges live (the current in-memory tree's child list) with
// durable (Config-persisted SpawnedChildIDs, spawn order, never shrinks —
// see Session.SpawnedChildIDs' own doc comment) into ONE de-duplicated
// list. Durable entries come first, in spawn order. Live-only entries (a
// legacy parent whose log predates task.spawned records) follow, in their
// own order.
//
// De-duplication is uniform: one id appears exactly once in the result,
// whichever argument carried it and however many times. Neither argument
// is trusted to be duplicate-free — see the merge loop's own comment for
// the asymmetry that rule replaced.
//
// Durable-first preserves spawn order for the common, all-post-field
// case. The live list is a spawn-order subsequence of durable there:
// adoptLocked appends at spawn, and Reap's filter keeps survivor order.
// Live-first would reorder siblings the moment an elder child settles and
// is Reaped while a younger one still runs ([B, A] instead of [A, B]).
//
// A live review finding: this guarantee does NOT extend to a mixed
// legacy/non-legacy tree — a parent that spawned an elder child A before
// task.spawned records existed (A lives only in the live tree, never in
// durable SpawnedChildIDs) and a younger child B after the field shipped
// (durable) yields childIDsUnion(live=[A,B], durable=[B]) = [B, A]: B
// before A, reversed from true spawn order. Narrow and low severity —
// pre-field lineage was always best-effort, and this ordering was never a
// documented contract for that migration edge — but the guarantee above
// is specifically an all-post-field one, not universal.
//
// Always returns a non-nil slice — never omitted, see lineageJSON.Children's
// own doc comment on why that field has no omitempty. "children":[] means
// "known: zero children now, and none ever durably spawned either," never
// "unknown."
func childIDsUnion(live, durable []string) []string {
	out := make([]string, 0, len(durable)+len(live))
	seen := make(map[string]bool, len(durable)+len(live))
	// ONE merge loop over both sides, in durable-then-live order. An
	// earlier revision took a `len(durable) == 0` fast path that copied
	// live verbatim, on the argument that live can hold no duplicate
	// (adoptLocked appends a child id at most once). That reasoning was
	// true but it made the two sides answer differently: a repeated id in
	// live survived when durable was empty and collapsed when it was not
	// (a live review finding). This merge states the union's OWN contract
	// instead of borrowing each producer's invariant — one id appears
	// once, whichever side carried it — so a future change to either
	// producer cannot silently split the two answers apart again. The
	// dropped fast path saved one small map allocation per childless
	// lineage read; correctness of a wire contract is worth more.
	for _, src := range [...][]string{durable, live} {
		for _, id := range src {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// lastTurnJSONLocked builds the Session.last_turn / StatusEntry.last_turn
// shape from s.lastTurn, or nil if no turn has finished for id in this
// process. Caller holds s.mu.
func (s *Server) lastTurnJSONLocked(id string) *lastTurnJSON {
	t := s.lastTurn[id]
	if t == nil {
		return nil
	}
	return &lastTurnJSON{Outcome: t.outcome, Error: t.error}
}

func statusStr(running bool) string {
	if running {
		return "busy"
	}
	return "idle"
}

// decodeBody decodes an optional JSON request body into v. An absent body is
// not an error (v keeps its zero value).
func decodeBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
