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
		writeErr(w, http.StatusNotFound, "no such parent session")
		return
	}
	defs, err := engine.ResolveAgentDefs(parent.WorkDir())
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

// handleSessionSend implements session.send (design doc, Stage 4): deliver
// a user-role message to any session this server's SessionManager tracks —
// root or child. It is a NEW route rather than an extension of the
// existing POST /session/{id}/prompt_async (handlePrompt), deliberately:
// prompt_async is woven through this package's residency/eviction/queue/
// goal-loop machinery (claimForPrompt, maybeDispatchQueued,
// maybeAutoArmGoal — see AGENTS.md's "Goal loop" and "Prompt queue"
// sections), all of which assumes ONE canonical resident *engine.Session
// object per id, tracked in s.sessions. A CHILD session created via
// SessionManager.Spawn is never registered in s.sessions at all — routing
// it through that machinery would require either forcing every child into
// residency tracking too (out of scope for this stage) or teaching every
// one of those call sites to fall back to sessMgr, each a chance to get an
// existing invariant subtly wrong. A dedicated route sidesteps all of it:
// delivery always goes through SessionManager.Send, which has its own
// run-slot/concurrency bookkeeping independent of s.sessions entirely.
//
// Always asynchronous (like prompt_async): the turn runs in a background
// goroutine and this handler returns 202 immediately with nothing but the
// session id — the caller polls session.info (GET /session/{id}) for the
// outcome, exactly like the `task` tool's own callers do.
//
// KNOWN LIMITATION (see the design doc discrepancies noted in the
// implementation PR description): a ROOT session that this process's
// residency system (MaxResident) has evicted and later reloaded gets a
// FRESH *engine.Session object via s.opts.LoadSession on that reload path
// (lookup, handleList) — but SessionManager's node still references the
// ORIGINAL object AdoptRoot saw at creation. A session.send delivered
// through this endpoint after such an eviction/reload cycle drives that
// stale object, not the one prompt_async's residency machinery is using —
// two independent in-memory objects appending to the same on-disk log.
// This endpoint is fully reliable for every child session (never subject
// to eviction — SessionManager is their only owner) and for any root
// session that stays resident for its whole lifetime, which is the common
// case; it is not yet reconciled with eviction for a long-lived root.
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
	if _, ok := s.sessMgr.Info(id); !ok {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sessMgr.Send(context.Background(), id, body.Text) //nolint:errcheck // async: the outcome is read back via session.info's lineage/last_turn fields, not this call's return
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"session_id": id, "status": "sent"})
}

// handleCancelTree cancels id and its entire SessionManager subtree —
// cascade cancellation, wire-exposed (the design doc's cascade-cancel
// requirement: "canceling a parent cancels its entire subtree"). Distinct
// from the existing POST /session/{id}/abort, which only interrupts id's
// OWN current turn (its cancel func, s.sessions[id].cancel) and has no
// notion of children at all — abort remains exactly as it was for a
// session with no SessionManager-tracked children; this is additive.
func (s *Server) handleCancelTree(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sessionIDOrNotFound(w, r)
	if !ok {
		return
	}
	if err := s.sessMgr.Cancel(id); err != nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
