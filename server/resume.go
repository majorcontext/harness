package server

import (
	"context"
	"errors"
	"fmt"
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
	sess := st.sess
	if sess.TurnStopped() || !sess.TurnUnfinished() {
		return nil
	}
	if !sess.ResumableTurn() {
		return s.closeLostTurn(id, st)
	}
	claimed, runCtx, _, code, holder := s.claimForPrompt(id)
	switch {
	case code == 0:
	case code == 409 && holder == "":
		s.mu.Lock()
		_, refused := s.refused[id]
		s.mu.Unlock()
		if refused {
			return ErrSessionNotOwned
		}
		return nil
	case code == 503:
		return errServerDraining
	default:
		return fmt.Errorf("server: resume session %s: workdir held by session %q", id, holder)
	}
	s.emitBusy(id, claimed)
	go s.runTurn(runCtx, id, claimed, claimed.sess.ResumeTurn)
	return nil
}

func (s *Server) closeLostTurn(id string, st *sessionState) error {
	_ = s.sessMgr.AdoptRoot(st.sess) // already managed means recovery already ran
	s.syncMessages(id)
	s.recordTurnEnd(id, "", st.sess, "lost", nil)
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
