package server

import "github.com/majorcontext/harness/engine"

// liveSession is ONE snapshot of every place a live, in-process
// *engine.Session for an id can be found: this server's own residency map
// (Server.sessions, populated by handleCreate and claimForPrompt's
// cold-load path) and SessionManager's node tree (populated by Spawn,
// AdoptRoot, and AdoptReloaded). The two are orthogonal — a Spawn-driven
// child is never a residency key, and a cold-loaded session the manager
// never adopted is never a node — so a caller that wants "the live object
// for this id" has to consult both, in a fixed preference order, every
// time.
//
// Before this type, four call sites each wrote that lookup out again:
// Server.lookup, resolveSessForSync (the journal path), waitSnapshot, and
// lineageJSONFor. Each one picked its own subset and its own order, and a
// bug fix in one — the residency-blindness class that made a spawned
// child's messages never reach the durable journal — had to be
// rediscovered separately in the next. One snapshot type, taken once per
// request, replaces all four.
//
// Consistency rules this type exists to hold:
//
//  1. The manager half (session AND lifecycle node) comes from ONE
//     SessionManager.SessionAndInfo call, so the pair always describes the
//     same node. Two separate Session + Info calls could straddle a Reap
//     and pair one node's *Session with another's status — see
//     SessionAndInfo's own doc comment for the finding that introduced it.
//  2. The residency half (session AND running flag) comes from ONE s.mu
//     hold, for the same reason.
//  3. The snapshot is immutable once taken. Every projection below reads
//     only captured fields, never a fresh manager or residency read, so a
//     handler that answers one request from one snapshot can never mix two
//     instants — GET /session used to read the manager twice (Server.lookup
//     for the session, then lineageJSONFor for the lineage block), which
//     let the body describe one node and its lineage another.
//
// The residency half and the manager half are still two separate holds of
// two separate mutexes, and that is deliberate: server.mu is a LEAF lock
// with respect to both a session's own mutex and SessionManager.mu (see
// syncMessages' lock-ordering note in journal.go), so taking m.mu inside
// s.mu to make the whole snapshot atomic would create exactly the cycle
// that rule forbids. The gap is tolerable because the two halves are never
// merged: residency wins outright when it has an answer (rule: a resident
// session's own state is authoritative for itself), and the manager half
// is read only for what residency cannot answer at all. A session that
// becomes resident, or stops being resident, in the gap is reported as it
// was at one of the two instants — the same freshness bound any read of a
// concurrently mutating server already has.
//
// Why both halves are always needed: Server.sessions exists purely for
// THIS server's own HTTP claim/eviction residency, an orthogonal concept
// from "does anything in this process hold a live *engine.Session for this
// id". SessionManager.Spawn drives a child's turn directly (child.Prompt,
// in its own goroutine), never through claimForPrompt or handleCreate, so
// a spawned child's id is NEVER a residency key — while adoptLocked
// registers it in the manager synchronously, before that turn even starts.
// For a child this process merely rediscovers (a follow-up touch after
// Reap, or after a restart), ReportTurnStart and handleSpawnChild's
// AdoptReloaded fallback register it there too.
//
// A liveSession is built and used on one request goroutine. It is a value,
// so copying it is safe, but nothing here is synchronized: never share one
// across goroutines.
type liveSession struct {
	id string

	// resident is s.sessions[id].sess, or nil when id is not resident.
	// running is that entry's own running flag, captured in the same s.mu
	// hold; it is meaningful only when resident != nil.
	resident *engine.Session
	running  bool

	// managed is SessionManager's own live object for id, and info its
	// lifecycle node, both from one SessionAndInfo call. isManaged reports
	// whether that call found id at all: it is exactly (managed != nil)
	// for a tracked node, and callers assert that pairing (see
	// TestResolveLivePairsManagedSessionWithItsOwnNode).
	managed   *engine.Session
	info      engine.SessionNode
	isManaged bool

	// loaded is a disk-loaded session, set only by Server.lookup's own
	// cold-load tier. It is never a live object anything else in this
	// process holds, so it is preferred last and contributes no status.
	loaded *engine.Session
}

// resolveLive takes id's full snapshot: the residency half under one s.mu
// hold, then the manager half under one SessionManager.mu hold.
//
// Never call this while holding s.mu or a session's own mutex. It acquires
// both s.mu and SessionManager.mu, in that order, and the engine holds a
// session's mutex while emitting some events into Server.Publish (see
// journal.go's lock-ordering note).
func (s *Server) resolveLive(id string) liveSession {
	s.mu.Lock()
	lv := s.liveResidentLocked(id)
	s.mu.Unlock()
	return lv.withManager(s.sessMgr)
}

// liveResidentLocked captures id's residency half — the resident session
// and its running flag together, never one without the other. Caller holds
// s.mu. A caller that also needs other server state for the same instant
// (waitSnapshot reads queueDrainPending and goalState) calls this inside
// its own critical section, then completes the snapshot with withManager
// after unlocking.
func (s *Server) liveResidentLocked(id string) liveSession {
	lv := liveSession{id: id}
	if st := s.sessions[id]; st != nil {
		lv.resident = st.sess
		lv.running = st.running
	}
	return lv
}

// withManager returns lv with the SessionManager half filled in, read as
// one atomic (session, node) pair. Never call it while holding s.mu — see
// resolveLive's own doc comment for the lock order.
func (lv liveSession) withManager(sessMgr *engine.SessionManager) liveSession {
	if sess, info, ok := sessMgr.SessionAndInfo(lv.id); ok {
		lv.managed = sess
		lv.info = info
		lv.isManaged = true
	}
	return lv
}

// withManagerIfUnresolved fills the manager half ONLY when residency has no
// answer, and is the shape a caller uses when it needs nothing but the
// session object or its status.
//
// session() and status() both prefer residency outright, so for a resident
// id the manager half changes neither answer — and reading it costs the
// box-global SessionManager.mu plus a discarded SessionNode copy, on paths
// that run per journaled message and per wait poll (a live review finding).
//
// NEVER build a snapshot this way for a response that renders lineage:
// lineageJSONFor reads the manager half even for a resident session, and a
// skipped half would silently demote it to the durable cold branch. Use
// resolveLive there.
func (lv liveSession) withManagerIfUnresolved(sessMgr *engine.SessionManager) liveSession {
	if lv.resident != nil {
		return lv
	}
	return lv.withManager(sessMgr)
}

// liveSessionObject resolves ONLY the live *engine.Session for id, with the
// same residency-then-manager preference resolveLive uses and none of its
// manager read when residency already answers. It returns the session
// itself, not a snapshot, so a partial snapshot can never reach a status or
// lineage projection — see withManagerIfUnresolved's own doc comment.
func (s *Server) liveSessionObject(id string) *engine.Session {
	s.mu.Lock()
	lv := s.liveResidentLocked(id)
	s.mu.Unlock()
	return lv.withManagerIfUnresolved(s.sessMgr).session()
}

// liveFromResident builds a snapshot around a residency entry the caller
// already read under its OWN s.mu hold — handleList takes one bulk
// residency snapshot and renders every session from it. sess and running
// must come from one hold, for the pairing rule in this type's doc comment.
// The caller completes the manager half itself.
func liveFromResident(id string, sess *engine.Session, running bool) liveSession {
	return liveSession{id: id, resident: sess, running: running}
}

// withLoaded returns lv carrying sess as its disk-loaded tier. A caller
// uses it when it has already loaded (or already holds) a session no live
// source knows about; it never displaces a resident or managed object.
func (lv liveSession) withLoaded(sess *engine.Session) liveSession {
	lv.loaded = sess
	return lv
}

// session returns the *engine.Session to serve for lv.id, or nil when no
// source has one.
//
// Residency first: a resident entry may be a fresher reload of the same
// durable session than whatever the manager currently points at (see
// SessionManager.ReportTurnStart's "always re-attach to the LIVE object"
// note), and it is the object every claim/eviction-aware call site already
// uses. The manager's own node is next — it is the only source for a
// Spawn-driven child, which is never resident. A disk-loaded session is
// last: it is a private copy, correct to read, but nothing else in this
// process shares it.
func (lv liveSession) session() *engine.Session {
	switch {
	case lv.resident != nil:
		return lv.resident
	case lv.managed != nil:
		return lv.managed
	default:
		return lv.loaded
	}
}

// status reports "busy" or "idle" for lv.id, from the same source that
// produced lv.session().
//
// A resident session's own running flag is authoritative for itself and is
// never second-guessed against the manager: freeRunSlotAndEmitIdle clears
// running and wakes waiters BEFORE ReportTurnEnd flips the manager node off
// StatusRunning, so a manager read taken in that window reports a turn that
// this server already finished (a live review finding — see waitSnapshot's
// own doc comment). The manager answers only for an id residency does not
// know at all, which is the one case residency has no answer for: a
// Spawn-driven child mid-turn.
func (lv liveSession) status() string {
	switch {
	case lv.resident != nil:
		return statusStr(lv.running)
	case lv.isManaged:
		return statusStr(lv.info.Status == engine.StatusRunning)
	default:
		return "idle"
	}
}
