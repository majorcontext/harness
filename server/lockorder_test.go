package server

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// TestGoalEmitVsSyncMessagesNoDeadlock reproduces the lock-ordering deadlock
// between the two opposite acquisition orders that meet at server.mu and the
// session's own mutex:
//
//   - goal-emit path: Session.RegisterGoal/ClearGoal hold Session.mu while
//     calling OnEvent synchronously (see engine/goal.go), which is wired to
//     Server.Publish -> publishGoal, acquiring server.mu. Order: session.mu ->
//     server.mu.
//   - syncMessages-with-persist-error path: Server.syncMessages holds
//     server.mu across checkPersistErrLocked, which (before the fix) called
//     sess.PersistErr(), acquiring the session's own mutex. Order: server.mu
//     -> session.mu.
//
// Racing the two deterministically triggers an AB-BA deadlock on the old
// code. The whole race is run in a separate goroutine and bounded by a
// done-channel + select against time.After, so a regression makes this test
// FAIL within the bound instead of hanging the test binary (and CI) forever.
func TestGoalEmitVsSyncMessagesNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test"}
	coll := newErrCollector()
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnError = coll.onError
	})

	sess, err := srv.opts.NewSession(message.ModelRef{Provider: "test", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}

	// Force a persistent, deterministic persist failure (same technique as
	// TestSessionPersistErrForwardedOnce): pre-create the session's log path
	// as a directory so every ensureLog attempt fails the same way. This
	// keeps checkPersistErrLocked's forwarding branch live for the whole
	// race, not just a bare PersistErr() read.
	blocker := filepath.Join(dir, sess.ID+".jsonl")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sess.Persist(); err == nil {
		t.Fatal("want a persist error from the blocked log path")
	}

	srv.mu.Lock()
	srv.sessions[sess.ID] = &sessionState{sess: sess}
	srv.mu.Unlock()

	const iterations = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		wg.Add(2)
		// goal-emit path: session.mu -> server.mu (via OnEvent -> Publish).
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if err := sess.RegisterGoal("reach the goal"); err != nil {
					t.Error(err)
					return
				}
				sess.ClearGoal()
			}
		}()
		// syncMessages-with-persist-error path: server.mu -> session.mu (via
		// checkPersistErrLocked -> sess.PersistErr()).
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				srv.syncMessages(sess.ID)
			}
		}()
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: goal-emit (session.mu -> server.mu) raced " +
			"syncMessages-with-persist-error (server.mu -> session.mu) and " +
			"never completed")
	}

	if coll.count() == 0 {
		t.Error("OnError was never invoked despite a persistent persist error")
	}
}
