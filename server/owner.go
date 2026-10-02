package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/majorcontext/harness/engine"
)

// SessionOwner decides which process may serve a session. An embedder with
// a shared store supplies one. Nil means the server asks nobody.
type SessionOwner interface {
	// Acquire runs once before this Server first loads or creates session id.
	// An error means this process may not serve id.
	Acquire(ctx context.Context, id string) (Ownership, error)
}

// Ownership is one held claim on a session.
type Ownership interface {
	// Lost is closed when ownership ends for any reason other than Release.
	Lost() <-chan struct{}
	// Release runs on eviction and on Server.Close. It is idempotent.
	Release()
}

// ErrSessionNotOwned is the refusal for a session this server may not serve.
var ErrSessionNotOwned = errors.New("server: session not owned by this server")

type ownerEntry struct {
	ready chan struct{}
	own   Ownership
	stop  chan struct{}
	users int
	keep  bool
}

func noopDone() {}

// admitSession makes this server the owner of id for one request and returns
// the func that ends the request's use. It refuses every id once the events
// log is fenced or ownership of id was lost. Never call it with s.mu held:
// Acquire runs unlocked.
func (s *Server) admitSession(ctx context.Context, id string) (func(), error) {
	for {
		s.mu.Lock()
		if s.journalErr != nil {
			s.mu.Unlock()
			return nil, ErrSessionNotOwned
		}
		if s.opts.SessionOwner == nil {
			s.mu.Unlock()
			return noopDone, nil
		}
		if _, lost := s.refused[id]; lost {
			s.mu.Unlock()
			return nil, ErrSessionNotOwned
		}
		e := s.owned[id]
		if e == nil {
			e = &ownerEntry{ready: make(chan struct{}), users: 1}
			s.owned[id] = e
			s.mu.Unlock()
			return s.acquireOwnership(ctx, id, e)
		}
		s.mu.Unlock()
		select {
		case <-e.ready:
		case <-ctx.Done():
			return nil, ErrSessionNotOwned
		}
		s.mu.Lock()
		if s.owned[id] == e && e.own != nil {
			e.users++
			s.mu.Unlock()
			return s.endUse(id, e), nil
		}
		s.mu.Unlock()
	}
}

func (s *Server) acquireOwnership(ctx context.Context, id string, e *ownerEntry) (func(), error) {
	own, err := s.opts.SessionOwner.Acquire(ctx, id)
	s.mu.Lock()
	if err != nil || own == nil {
		delete(s.owned, id)
		close(e.ready)
		s.mu.Unlock()
		return nil, ErrSessionNotOwned
	}
	e.own = own
	e.stop = make(chan struct{})
	s.ownerWG.Add(1)
	close(e.ready)
	s.mu.Unlock()
	go s.watchOwnership(id, e)
	return s.endUse(id, e), nil
}

func (s *Server) endUse(id string, e *ownerEntry) func() {
	return func() {
		s.mu.Lock()
		e.users--
		if e.users == 0 && !e.keep && s.owned[id] == e && s.sessions[id] == nil {
			s.releaseOwnershipLocked(id)
		}
		s.mu.Unlock()
	}
}

// releaseOwnershipLocked hands id's ownership back through its watcher.
// Caller holds s.mu.
func (s *Server) releaseOwnershipLocked(id string) {
	if e := s.owned[id]; e != nil && e.own != nil {
		delete(s.owned, id)
		close(e.stop)
	}
}

func (s *Server) watchOwnership(id string, e *ownerEntry) {
	defer s.ownerWG.Done()
	select {
	case <-e.stop:
		e.own.Release()
	case <-e.own.Lost():
		s.ownershipLost(id, e)
		e.own.Release()
	}
}

// ownershipLost suspends id's whole lineage without settling any turn, so
// the next holder resumes them, then evicts id and refuses it from now on.
func (s *Server) ownershipLost(id string, e *ownerEntry) {
	lineage := s.sessMgr.Subtree(id)
	s.mu.Lock()
	if s.owned[id] == e {
		delete(s.owned, id)
	}
	s.refused[id] = struct{}{}
	for _, c := range lineage {
		s.refused[c] = struct{}{}
	}
	var cancel context.CancelCauseFunc
	var sess *engine.Session
	if st := s.sessions[id]; st != nil {
		cancel = st.cancel
		sess = st.sess
		delete(s.sessions, id)
		delete(s.lastRequest, id)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
	s.sessMgr.Suspend(id)
	if sess != nil {
		sess.ReleaseFiles()
	}
}

// fenceLocked stops this server from serving after an events-log append
// conflict: another writer owns the log. Running turns, children included,
// are canceled without a settle record, and every held session is evicted.
// Caller holds s.mu.
func (s *Server) fenceLocked() {
	select {
	case <-s.fencedCh:
	default:
		close(s.fencedCh)
	}
	var evicted []*engine.Session
	for id, st := range s.sessions {
		if st.cancel != nil {
			st.cancel(nil)
		}
		evicted = append(evicted, st.sess)
		delete(s.sessions, id)
		delete(s.lastRequest, id)
	}
	for id := range s.owned {
		s.releaseOwnershipLocked(id)
	}
	go func() {
		s.sessMgr.SuspendAll()
		releaseEvicted(evicted)
	}()
}

// keepOwnership stops the end of the current request from releasing id.
func (s *Server) keepOwnership(id string) {
	s.mu.Lock()
	if e := s.owned[id]; e != nil {
		e.keep = true
	}
	s.mu.Unlock()
}

func (s *Server) releaseAllOwnership() {
	s.mu.Lock()
	for id := range s.owned {
		s.releaseOwnershipLocked(id)
	}
	s.mu.Unlock()
	s.ownerWG.Wait()
}

func writeNotOwned(w http.ResponseWriter) {
	writeErr(w, http.StatusConflict, ErrSessionNotOwned.Error())
}
