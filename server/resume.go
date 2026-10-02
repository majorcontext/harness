package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var errServerDraining = errors.New("server: draining")

// ResumeSession continues the unfinished turn of session id without a client
// request. An embedder calls it after it claims the session's lease. ctx
// bounds the load and the dispatch; the resumed turn runs on the server's own
// run context, like a prompt.
//
// It returns nil, and does nothing, when a turn already runs, when no turn is
// unfinished, or when the last turn was stopped. When the resume cap is
// reached it closes the turn as lost to restart. It returns
// ErrSessionNotOwned when the SessionOwner refuses id, and an error that
// wraps fs.ErrNotExist for an unknown session.
func (s *Server) ResumeSession(ctx context.Context, id string) error {
	st, err := s.ensureResident(ctx, id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	running := st.running
	s.mu.Unlock()
	if running {
		return nil
	}
	if st.sess.TurnStopped() || !st.sess.TurnUnfinished() {
		return nil
	}
	claimed, runCtx, ok, err := s.claimResume(id)
	if err != nil || !ok {
		return err
	}
	sess := claimed.sess
	switch {
	case sess.TurnStopped() || !sess.TurnUnfinished():
		s.releasePromptClaim(claimed)
		return nil
	case sess.ResumableTurn():
		s.emitBusy(id, claimed)
		go s.runTurn(runCtx, id, claimed, sess.ResumeTurn)
		return nil
	}
	return s.settleUnresumable(id, claimed)
}

func (s *Server) claimResume(id string) (*sessionState, context.Context, bool, error) {
	claimed, runCtx, _, code, holder := s.claimForPrompt(id)
	switch {
	case code == 0:
		return claimed, runCtx, true, nil
	case code == http.StatusConflict && holder == "":
		s.mu.Lock()
		_, refused := s.refused[id]
		s.mu.Unlock()
		if refused {
			return nil, nil, false, ErrSessionNotOwned
		}
		return nil, nil, false, nil
	case code == http.StatusServiceUnavailable:
		return nil, nil, false, errServerDraining
	}
	return nil, nil, false, fmt.Errorf("server: resume session %s: workdir held by session %q", id, holder)
}

// settleUnresumable runs the manager's restart recovery on the claimed
// session. Only a turn that reached the resume cap reports outcome lost; a
// turn that already holds a final answer or a committed outcome settles as
// recovery decides and emits no turn.end.
func (s *Server) settleUnresumable(id string, st *sessionState) error {
	defer s.releasePromptClaim(st)
	capped := st.sess.ResumeCapReached()
	s.sessMgr.RecoverRoot(st.sess)
	s.syncMessages(id)
	if st.sess.TurnUnfinished() {
		return fmt.Errorf("server: resume session %s: recovery left the turn unfinished", id)
	}
	if capped {
		s.recordTurnEnd(id, "", st.sess, "lost", nil)
	}
	return nil
}

// ensureResident returns the resident state of id and loads the session when
// it is not resident. It runs SessionOwner.Acquire through admitSession
// before the load, and checks again after the load, like claimForPrompt.
func (s *Server) ensureResident(ctx context.Context, id string) (*sessionState, error) {
	s.mu.Lock()
	if err := s.admitErrLocked(id); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if st := s.sessions[id]; st != nil {
		s.mu.Unlock()
		return st, nil
	}
	s.mu.Unlock()
	done, err := s.admitSession(ctx, id)
	if err != nil {
		return nil, err
	}
	defer done()
	sess, err := s.opts.LoadSession(id)
	if err != nil {
		return nil, fmt.Errorf("server: load session %s: %w", id, err)
	}
	loaded := &sessionState{sess: sess, lastUsed: time.Now()}
	s.mu.Lock()
	if err := s.admitErrLocked(id); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	st := s.sessions[id]
	if st == nil {
		s.sessions[id] = loaded
		st = loaded
	}
	evicted := s.evictResidentLocked()
	s.mu.Unlock()
	releaseEvicted(evicted)
	return st, nil
}

func (s *Server) admitErrLocked(id string) error {
	if s.draining {
		return errServerDraining
	}
	if _, lost := s.refused[id]; lost || s.journalErr != nil {
		return ErrSessionNotOwned
	}
	return nil
}
