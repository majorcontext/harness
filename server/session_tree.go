package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
)

// handleSpawnChild implements session.create's "with a parent" form
// (design doc, Stage 4): "identical to a task call made from outside the
// model" — the wire-level equivalent of the `task` tool, callable from
// outside the model entirely (the design doc's first named consumer: the
// boxes control plane). Called from handleCreate when the request body
// carries a non-empty parent_id; shares NONE of handleCreate's
// worktree/residency machinery below that branch — a child's workdir,
// provider, and persistence all come from its parent's already-resolved
// Config, via SessionManager.Spawn, never from this request.
func (s *Server) handleSpawnChild(w http.ResponseWriter, parentID, agent, prompt string, model message.ModelRef) {
	if agent == "" || prompt == "" {
		writeErr(w, http.StatusBadRequest, "agent and prompt are required when parent_id is set")
		return
	}
	parent, ok := s.sessMgr.Session(parentID)
	if !ok {
		// Not yet a tracked node. ReportTurnStart's adopt-on-first-sight
		// (runPrompt/runGoal) only fires once a TURN actually runs against
		// a session in this process — a session sitting on disk since
		// before this process started (or evicted), with no ordinary
		// prompt having touched it yet here, would otherwise 404 even
		// though it is perfectly resumable. Cold-load and adopt it here
		// too, so session.create's parent_id form works for a reloaded
		// parent exactly like the `task` tool does once ANY prompt has
		// touched it.
		loaded, err := s.opts.LoadSession(parentID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "no such parent session")
			return
		}
		_ = s.sessMgr.AdoptRoot(loaded) // ignore "already managed": a concurrent adopt (e.g. ReportTurnStart) may have won the race
		parent, ok = s.sessMgr.Session(parentID)
		if !ok {
			writeErr(w, http.StatusNotFound, "no such parent session")
			return
		}
	}
	defs, err := parent.AgentDefs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot load agent definitions")
		return
	}
	def, ok := defs[agent]
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("unknown agent %q", agent))
		return
	}
	spawnModel := def.Model
	if !model.IsZero() {
		spawnModel = model
	}
	childID, err := s.sessMgr.Spawn(engine.SpawnOptions{
		ParentID:     parentID,
		Prompt:       prompt,
		Model:        spawnModel,
		SystemAppend: def.SystemAppend,
		ToolNames:    def.Tools,
		AgentType:    agent,
	})
	if err != nil {
		if errors.Is(err, engine.ErrUnknownSession) {
			writeErr(w, http.StatusNotFound, err.Error())
		} else {
			// ErrDepthLimit, ErrConcurrencyLimit, ErrSessionCanceled: all
			// short, fixed, secret-free sentinel strings — safe to surface
			// directly (see classifySpawnError's doc comment for the same
			// reasoning on the `task` tool's identical error set).
			writeErr(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, s.buildSession(mustLookupSpawned(s, childID), "idle"))
}

// mustLookupSpawned returns the just-Spawned child by id. Spawn only just
// registered it under s.sessMgr, so this cannot fail in practice; a nil
// fallback (an empty, zero-value session summary) is used defensively
// instead of panicking a request handler on what would be an internal
// bookkeeping bug, not a caller error.
func mustLookupSpawned(s *Server, id string) *engine.Session {
	if sess, ok := s.sessMgr.Session(id); ok {
		return sess
	}
	return &engine.Session{} // unreachable in practice; see doc comment
}

// runOrQueueText claims id's run slot exactly like an ordinary
// prompt_async would and drives a turn with text — or, if a real prompt
// is already queued ahead of it, dispatches the queue's own head instead,
// exactly like handlePrompt's identical branch (see dispatchQueueHead) —
// so this can NEVER race an ordinary prompt_async request for the same
// session: both go through the exact same claimForPrompt admission gate,
// which also means it transparently cold-loads id from disk if it isn't
// currently resident (evicted, or simply never touched by this process
// instance — e.g. after a restart), exactly like claimForPrompt's own
// callers already get for free.
//
// This is BOTH engine.ExternalRunner's implementation
// (resumeSessionForTaskNotification, below — SessionManager delegates a
// root's engine-initiated resume turn here instead of calling
// Session.Prompt directly, see ExternalRunner's doc comment for why) AND
// handleSessionSend's root branch: "wake a session with a synthetic
// trigger" and "deliver a real user message" are the same operation as
// far as this admission gate is concerned.
//
// handled reports whether id is a session this server knows how to run at
// all (resident or loadable from disk) — true even when nothing actually
// started THIS instant because something else already holds the run
// slot: that turn's own next request will pick up any pending
// notification via the ordinary queue-at-next-turn-boundary path
// (checkoutTaskNotificationsSegment, engine.go), so no further action is
// needed here either way.
func (s *Server) runOrQueueText(id, text string) (handled bool) {
	st, ctx, _, code, _ := s.claimForPrompt(id)
	if code != 0 {
		return code != http.StatusNotFound
	}
	if len(st.sess.QueuedPrompts()) > 0 {
		// Enqueue text durably behind whatever is already queued, then
		// dispatch the queue's HEAD (not necessarily this call's own
		// text) into the run slot just claimed — mirrors handlePrompt's
		// identical idle-with-queue branch (handlers.go, "Global FIFO on
		// an idle-with-queue session"). Without this, a session.send
		// landing here with a non-empty queue silently lost its own
		// text: only the pre-existing head ever ran, and the caller
		// still got 202 "sent" — a live review caught this. The
		// resume-trigger caller (resumeSessionForTaskNotification)
		// doesn't strictly need its synthetic trigger text preserved
		// (the dispatched head's own turn surfaces the pending
		// notification via checkoutTaskNotificationsSegment regardless
		// of what text drove it, and that text already becomes real
		// turn input via the empty-queue branch below anyway) — so one
		// unconditional enqueue-then-dispatch here stays correct for
		// both callers without a caller-specific branch.
		if _, err := st.sess.EnqueuePrompt(text); err != nil {
			// Only reachable for empty/whitespace-only text; both
			// callers already guard against that (handleSessionSend
			// rejects body.Text=="" before calling here, and
			// taskResumeTriggerText is a non-empty constant). Fail
			// closed rather than silently drop, releasing the claim
			// just taken.
			s.releasePromptClaim(st)
			return true
		}
		s.dispatchQueueHead(id, st, ctx)
		return true
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runPrompt(ctx, id, st, text)
	return true
}

// resumeSessionForTaskNotification is this server's engine.ExternalRunner
// (installed via sessMgr.SetExternalRunner in server.New) — see
// ExternalRunner's doc comment for why a root's engine-initiated resume
// turn must go through this server's OWN run-slot admission rather than
// SessionManager calling Session.Prompt directly.
func (s *Server) resumeSessionForTaskNotification(id, text string) bool {
	return s.runOrQueueText(id, text)
}

// handleSessionSend implements session.send (design doc, Stage 4): deliver
// a user-role message to any session this server's SessionManager tracks —
// root or child. It is a NEW route rather than an extension of the
// existing POST /session/{id}/prompt_async (handlePrompt): CHILD delivery
// always goes through SessionManager.Send directly (a child is never
// resident-tracked by anything else — SessionManager is its sole
// scheduler), but a ROOT session is routed through runOrQueueText, the
// EXACT SAME claimForPrompt admission gate prompt_async itself uses —
// never through SessionManager.Send, which would compete with an ordinary
// prompt_async request for the same root (see ExternalRunner's doc
// comment on the class of bug this avoids: two independent schedulers
// both able to start a Session.Prompt call on the same session).
//
// Always asynchronous (like prompt_async): the turn runs in a background
// goroutine (or is claimed-and-dispatched synchronously by
// runOrQueueText, itself launching its own goroutine) and this handler
// returns 202 immediately with nothing but the session id — the caller
// polls session.info (GET /session/{id}) for the outcome, exactly like
// the `task` tool's own callers do.
func (s *Server) handleSessionSend(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	info, ok := s.sessMgr.Info(id)
	if !ok {
		// Not a tracked node yet — could be a root that exists on disk but
		// has had no turn run against it in this process (see
		// handleSpawnChild's identical fallback: ReportTurnStart's
		// adopt-on-first-sight only fires once a turn actually runs).
		// runOrQueueText's own claimForPrompt cold-loads it and
		// ReportTurnStart adopts it as part of running the turn; an id
		// truly unknown to this server (no log on disk either) surfaces as
		// handled=false from there. A child is never in this situation —
		// SessionManager registers it the instant Spawn creates it, so
		// "not a node" here only ever means "an as-yet-unadopted root" or
		// "genuinely unknown."
		if !s.runOrQueueText(id, body.Text) {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"session_id": id, "status": "sent"})
		return
	}
	if info.ParentID == "" {
		// Root: route through the ordinary run-slot admission path — see
		// runOrQueueText's doc comment for why session.send must never
		// independently drive Session.Prompt for a root. This also
		// transparently handles a root evicted from residency and later
		// reloaded: claimForPrompt's own cold-load path covers it, unlike
		// an earlier version of this handler that drove a stale
		// SessionManager-cached object in that case.
		if !s.runOrQueueText(id, body.Text) {
			writeErr(w, http.StatusNotFound, "no such session")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"session_id": id, "status": "sent"})
		return
	}
	// Child: SessionManager is its sole scheduler, always safe. Unlike a
	// root, a child has no prompt queue (SessionManager.Send's own
	// ErrSessionBusy check has nowhere to defer to) — firing Send in a
	// background goroutine and discarding its error unconditionally, as
	// an earlier version of this handler did, meant a message sent to an
	// already-running child was silently dropped while the caller still
	// got 202 "sent". Refuse up front instead. info was read moments
	// ago, so there is a small residual race (the child could finish and
	// go idle in the gap) — the same class of benign, documented race
	// runOrQueueText/enqueueOrDispatch accept elsewhere in this package —
	// but this closes the common, reproducible case: a caller sending a
	// follow-up while a child's turn is visibly still in flight.
	if info.Status == engine.StatusRunning {
		writeErr(w, http.StatusConflict, "session is busy with another prompt")
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sessMgr.Send(context.Background(), id, body.Text) //nolint:errcheck // async: outcome read back via session.info; ErrSessionBusy is pre-checked above, ErrSessionCanceled/ErrConcurrencyLimit are rare residual races (the child settled or the tree budget was hit in the gap between the check above and this call) not worth blocking the request on
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"session_id": id, "status": "sent"})
}

// handleCancelTree cancels id and its entire SessionManager subtree —
// cascade cancellation, wire-exposed (the design doc's cascade-cancel
// requirement: "canceling a parent cancels its entire subtree"). Distinct
// from the existing POST /session/{id}/abort, which only interrupts id's
// OWN current turn and has no notion of children at all — abort remains
// exactly as it was for a session with no SessionManager-tracked
// children; this is additive.
//
// sessMgr.Cancel cancels every node's OWN context (node.ctx), which
// aborts a turn SessionManager itself drives directly (Spawn, Send, or an
// internally-driven resume with no ExternalRunner) — but a ROOT session's
// turn instead runs on THIS server's own run-slot machinery
// (claimForPrompt/runPrompt), on a DIFFERENT context (sessionState.cancel)
// that node.ctx does not reach — see engine.SessionManager.Cancel's doc
// comment. This mirrors handleAbort's own cancel call so cancel_tree
// actually stops a root's in-flight turn, not merely marks it canceled
// while it keeps running to completion underneath.
func (s *Server) handleCancelTree(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if err := s.sessMgr.Cancel(id); err != nil {
		writeErr(w, http.StatusNotFound, "no such session")
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
	}
	w.WriteHeader(http.StatusNoContent)
}
