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
		// AdoptReloaded, not AdoptRoot: parentID is a caller-supplied
		// string this handler has no independent reason to believe names
		// a root rather than a child SessionManager's Reap (or a process
		// restart) had forgotten — using AdoptRoot here would risk the
		// exact depth-limit bypass AdoptRoot's own doc comment warns
		// about. "already managed" is ignored: a concurrent adopt (e.g.
		// ReportTurnStart) may have won the race.
		_ = s.sessMgr.AdoptReloaded(loaded)
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
	spawned, ok := lookupSpawned(s, childID)
	if !ok {
		// Unreachable in practice (see lookupSpawned's doc comment), but
		// a clean 500 here is a far better failure than the zero-value
		// session summary an earlier revision of this handler fell back
		// to — buildSession(&engine.Session{}, "idle") on a zero Session
		// produced a self-inconsistent 201: blank id/model/usage at the
		// top level next to a fully populated lineage block keyed off
		// the real childID, a plausible-looking but malformed success
		// response instead of a clear error. A live review caught this.
		writeErr(w, http.StatusInternalServerError, "spawned child not found in session tree")
		return
	}
	// "busy", not "idle": Spawn always hands the child work immediately
	// (see its own doc comment — "a spawned child is handed work
	// immediately, so it is never idle before running", reserving the
	// concurrency slot and setting StatusRunning synchronously before
	// returning). An earlier revision hard-coded "idle" here, producing
	// a self-inconsistent 201 body: the top-level status/state claimed
	// idle while the SAME response's own lineage block (sourced from the
	// live SessionManager node) correctly reported "running" beside it.
	// A live review caught this.
	writeJSON(w, http.StatusCreated, s.buildSession(spawned, "busy"))
}

// lookupSpawned returns the just-Spawned child by id. Spawn only just
// registered it under s.sessMgr, so ok is false only on an internal
// bookkeeping bug, never a caller error — see handleSpawnChild's own
// handling of that case.
func lookupSpawned(s *Server, id string) (*engine.Session, bool) {
	return s.sessMgr.Session(id)
}

// runOrQueueText claims id's run slot exactly like an ordinary
// prompt_async would and drives a turn with text — or, if a real prompt
// is already queued ahead of it, dispatches the queue's own head instead
// and drops text — exactly like handlePrompt's identical branch (see
// dispatchQueueHead) — so this can NEVER race an ordinary prompt_async
// request for the same session: both go through the exact same
// claimForPrompt admission gate, which also means it transparently
// cold-loads id from disk if it isn't currently resident (evicted, or
// simply never touched by this process instance — e.g. after a
// restart), exactly like claimForPrompt's own callers already get for
// free.
//
// This is engine.ExternalRunner's implementation
// (resumeSessionForTaskNotification, below — SessionManager delegates a
// root's engine-initiated resume turn here instead of calling
// Session.Prompt directly, see ExternalRunner's doc comment for why),
// and ONLY that: dropping text on a busy or already-queued root is
// harmless for a resume trigger (the pending notification stays queued
// and rides that turn's own next boundary via
// checkoutTaskNotificationsSegment regardless of what text drove it) but
// wrong for a real user message, so session.send uses the dedicated
// sendTextToRoot instead, which never drops text on any admission
// outcome — see its own doc comment for the two live reviews that caught
// runOrQueueText being reused for that purpose.
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
		s.dispatchQueueHead(id, st, ctx)
		return true
	}
	s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
	go s.runPrompt(ctx, id, st, text)
	return true
}

// sendTextToRoot delivers text to root id through this server's ordinary
// run-slot admission (claimForPrompt) — the SAME path prompt_async uses
// — but, unlike runOrQueueText above, NEVER drops text on ANY admission
// outcome: a real user message must survive exactly like prompt_async's
// own handlePrompt/enqueueOrDispatch pair guarantees, not just the
// idle-with-non-empty-queue case runOrQueueText itself now handles. Two
// live reviews caught runOrQueueText silently dropping session.send text
// while still reporting 202 "sent": first when the root was idle with an
// already-non-empty queue (fixed by making runOrQueueText itself durably
// enqueue), then again — a DIFFERENT gap in the SAME function — when the
// root was simply BUSY (claimForPrompt's ordinary 409), which
// runOrQueueText's early `return code != http.StatusNotFound` still
// drops unconditionally. That drop is CORRECT for
// resumeSessionForTaskNotification (a dropped synthetic trigger costs
// nothing — the pending notification stays queued and rides the busy
// turn's own next boundary via checkoutTaskNotificationsSegment
// regardless), so runOrQueueText itself is left alone for that caller;
// session.send gets its own function instead of a caller-specific branch
// bolted onto the shared one.
//
// status is "started" or "queued" — the same two values prompt_async's
// own status field ever reports — with queuedDepth meaningful only when
// status is "queued" (mirrors promptAsyncResponse). errCode is 0 on
// success; otherwise the HTTP status the caller should report instead
// (404 unknown session, 503 draining, 409 with holder set for a
// workdir-held conflict, 400 for the practically-unreachable empty-text
// case handleSessionSend already guards against).
func (s *Server) sendTextToRoot(id, text string) (status string, queuedDepth int, errCode int, holder string) {
	st, ctx, _, code, holder := s.claimForPrompt(id)
	switch {
	case code == http.StatusNotFound:
		return "", 0, http.StatusNotFound, ""
	case code == http.StatusServiceUnavailable:
		return "", 0, http.StatusServiceUnavailable, ""
	case code == http.StatusConflict && holder != "":
		return "", 0, http.StatusConflict, holder
	case code == http.StatusConflict:
		// Busy (not a workdir conflict): enqueue durably now, then race
		// ONE retry — mirrors enqueueOrDispatch (handlers.go) exactly,
		// closing the gap where the busy occupant's own tail
		// (runPrompt's maybeDispatchQueued) runs between the failed claim
		// above and this enqueue.
		if s.sendBusyEvictRace != nil {
			s.sendBusyEvictRace()
		}
		sess := s.residentSession(id)
		if sess == nil {
			// Benign race, identical to enqueueOrDispatch's own: the busy
			// occupant finished and was evicted in the gap. text was
			// NEVER durably enqueued in this branch (EnqueuePrompt below
			// never ran) — so this must report a RETRYABLE failure, not
			// success. An earlier revision of this branch returned
			// "queued" (errCode 0), which writeSendToRootResult turns
			// into a 202 the caller has no reason to ever retry —
			// permanently losing the text while claiming it was
			// accepted. enqueueOrDispatch's OWN identical race
			// (handlers.go) returns 409 "session is busy" for exactly
			// this reason; mirror it. A live review caught this.
			return "", 0, http.StatusConflict, ""
		}
		ourID, err := sess.EnqueuePrompt(text)
		if err != nil {
			return "", 0, http.StatusBadRequest, ""
		}
		st2, ctx2, _, code2, _ := s.claimForPrompt(id)
		if code2 != 0 {
			return "queued", len(sess.QueuedPrompts()), 0, ""
		}
		head, ok := s.dispatchQueueHead(id, st2, ctx2)
		if !ok {
			return "queued", len(sess.QueuedPrompts()), 0, ""
		}
		if head.ID == ourID {
			return "started", 0, 0, ""
		}
		return "queued", len(sess.QueuedPrompts()), 0, ""
	default: // code == 0: claimed cleanly
		if len(st.sess.QueuedPrompts()) > 0 {
			if _, err := st.sess.EnqueuePrompt(text); err != nil {
				s.releasePromptClaim(st)
				return "", 0, http.StatusBadRequest, ""
			}
			s.dispatchQueueHead(id, st, ctx)
			return "queued", len(st.sess.QueuedPrompts()), 0, ""
		}
		s.emitDurable(Event{Type: evtSessionStatus, SessionID: id, Status: "busy"})
		go s.runPrompt(ctx, id, st, text)
		return "started", 0, 0, ""
	}
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
// writeSendToRootResult writes handleSessionSend's HTTP response for a
// root, given sendTextToRoot's return values. session.send's wire
// contract reports "sent" (not "started", prompt_async's own vocabulary)
// for the common case so existing callers built against the original
// unconditional-202 contract keep working — a caller checking for a
// truthy status still sees success — but now ALSO reports "queued" with
// a depth, honestly, exactly like prompt_async already does, rather than
// claiming "sent" for a message that has not actually run yet.
func (s *Server) writeSendToRootResult(w http.ResponseWriter, id, status string, queuedDepth, errCode int, holder string) {
	switch errCode {
	case 0:
		// fall through to the success response below
	case http.StatusConflict:
		if holder != "" {
			writeErr(w, http.StatusConflict, fmt.Sprintf("workdir busy: held by session %s", holder))
		} else {
			// The ordinary busy-root case (sendTextToRoot's own
			// nil-residentSession race, or a workdir-holder-less
			// StatusConflict from claimForPrompt) — retryable, mirroring
			// enqueueOrDispatch's identical "session is busy with
			// another prompt" message for the same shape of race.
			writeErr(w, http.StatusConflict, "session is busy with another prompt")
		}
		return
	case http.StatusServiceUnavailable:
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	case http.StatusBadRequest:
		// Only reachable for empty/whitespace-only text — handleSessionSend
		// already rejects body.Text=="" before ever calling sendTextToRoot,
		// so this is not reachable in practice; fail closed rather than
		// silently drop.
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	default: // http.StatusNotFound
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	resp := map[string]any{"session_id": id, "status": "sent"}
	if status == "queued" {
		resp["status"] = "queued"
		resp["queued"] = queuedDepth
	}
	writeJSON(w, http.StatusAccepted, resp)
}

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
		// sendTextToRoot's own claimForPrompt cold-loads it and
		// ReportTurnStart adopts it as part of running the turn; an id
		// truly unknown to this server (no log on disk either) surfaces as
		// errCode==404 from there. A child is never in this situation —
		// SessionManager registers it the instant Spawn creates it, so
		// "not a node" here only ever means "an as-yet-unadopted root" or
		// "genuinely unknown."
		status, queuedDepth, errCode, holder := s.sendTextToRoot(id, body.Text)
		s.writeSendToRootResult(w, id, status, queuedDepth, errCode, holder)
		return
	}
	if info.ParentID == "" {
		// Root: route through the ordinary run-slot admission path — see
		// sendTextToRoot's doc comment for why session.send must never
		// independently drive Session.Prompt for a root, and never
		// silently drop the caller's text on any admission outcome. This
		// also transparently handles a root evicted from residency and
		// later reloaded: claimForPrompt's own cold-load path covers it,
		// unlike an earlier version of this handler that drove a stale
		// SessionManager-cached object in that case.
		status, queuedDepth, errCode, holder := s.sendTextToRoot(id, body.Text)
		s.writeSendToRootResult(w, id, status, queuedDepth, errCode, holder)
		return
	}
	// Child: SessionManager is its sole scheduler, always safe. Unlike a
	// root, a child has no prompt queue (SessionManager.Send's own
	// ErrSessionBusy check has nowhere to defer to) — firing Send in a
	// background goroutine and discarding its error unconditionally, as
	// an earlier version of this handler did, meant a message sent to an
	// already-running, already-canceled, or at-the-tree's-concurrency-cap
	// child was silently dropped while the caller still got 202 "sent".
	// CanSend surfaces all three of Send's real, deterministic admission
	// errors up front (an earlier revision of this fix only pre-checked
	// info.Status == StatusRunning, missing ErrConcurrencyLimit and
	// ErrSessionCanceled entirely — a live review caught this: a
	// concurrency-cap refusal is not a race, it is Send's ordinary,
	// expected outcome whenever the tree is already busy elsewhere, and a
	// canceled child is a permanent, deterministic state, not a fleeting
	// window). CanSend's own doc comment covers the genuinely small
	// residual race that remains between this check and the Send call
	// below.
	if err := s.sessMgr.CanSend(id); err != nil {
		if errors.Is(err, engine.ErrUnknownSession) {
			writeErr(w, http.StatusNotFound, "no such session")
		} else {
			// ErrSessionBusy/ErrConcurrencyLimit/ErrSessionCanceled: all
			// short, fixed, secret-free sentinel strings — safe to
			// surface directly (see classifySpawnError's doc comment for
			// the same reasoning on this error set elsewhere).
			writeErr(w, http.StatusConflict, err.Error())
		}
		return
	}
	// s.wg.Add must happen inside the SAME s.mu critical section that
	// observes s.draining==false — the invariant every other s.wg.Add
	// call site in this package upholds (claimForPrompt, handlers.go),
	// so that by mutex ordering every Add happens-before Drain sets
	// draining=true and calls wg.Wait(). A bare, unguarded Add here (an
	// earlier revision of this branch) could run concurrently with, or
	// after, wg.Wait() — a WaitGroup misuse that can panic, or let this
	// goroutine escape the drain wait entirely and keep writing to the
	// journal after Close, racing shutdown. A live review caught this.
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "server shutting down")
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		// context.Background(), not r.Context(): this handler has already
		// returned 202 by the time this runs, and a draining server
		// cannot cancel this specific turn through this path either way
		// (SessionManager.Send has no notion of the server's own drain
		// signal) — the same shape sessMgr.Send's other async callers in
		// this package already accept.
		s.sessMgr.Send(context.Background(), id, body.Text) //nolint:errcheck // async: outcome read back via session.info; CanSend above already surfaced the deterministic admission errors synchronously, so what remains here is only the genuinely racy window CanSend's own doc comment covers
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
