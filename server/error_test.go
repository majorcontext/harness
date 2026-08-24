package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// errCollector is a channel-backed OnError sink: tests block on <-ch for a
// deterministic wait (no sleeps) and inspect the accumulated errors under mu
// afterward.
type errCollector struct {
	mu   sync.Mutex
	errs []error
	ch   chan struct{}
}

func newErrCollector() *errCollector {
	return &errCollector{ch: make(chan struct{}, 64)}
}

func (c *errCollector) onError(_ context.Context, err error) {
	c.mu.Lock()
	c.errs = append(c.errs, err)
	c.mu.Unlock()
	c.ch <- struct{}{}
}

func (c *errCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.errs)
}

func (c *errCollector) last() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs[len(c.errs)-1]
}

// TestJournalWriteFailureInvokesOnError verifies that a journal write failure
// — which today only sets s.lastErr and vanishes — is forwarded to
// Options.OnError, wrapped with "journal write: %w".
//
// The failure is injected deterministically by closing the journal file
// handle (srv.jf, opened by New) directly: any subsequent write to a closed
// *os.File returns a stable "file already closed" error, with no dependence
// on filesystem permissions (which behave inconsistently across sandboxes,
// e.g. running as root).
func TestJournalWriteFailureInvokesOnError(t *testing.T) {
	dir := t.TempDir()
	coll := newErrCollector()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0, func(o *Options) {
		o.OnError = coll.onError
	})

	if srv.jf == nil {
		t.Fatal("journal file was not opened by New")
	}
	if err := srv.jf.Close(); err != nil {
		t.Fatal(err)
	}

	// Creating a session emits a durable session.created record, which
	// exercises writeJournalLocked.
	createSessionDirect(t, srv, "")

	<-coll.ch
	err := coll.last()
	if err == nil {
		t.Fatal("OnError not called")
	}
	if !strings.Contains(err.Error(), "journal write:") {
		t.Errorf("error = %q, want it wrapped with %q", err, "journal write:")
	}
}

// TestNilOnErrorIsSafe verifies that a server with no Options.OnError set
// never panics on the same error paths — nil-guarding OnError, not merely
// "usually don't hit it", is the contract.
func TestNilOnErrorIsSafe(t *testing.T) {
	dir := t.TempDir()
	srv := newServer(t, dir, &scriptedProvider{name: "test"}, 0) // no OnError

	if srv.jf == nil {
		t.Fatal("journal file was not opened by New")
	}
	if err := srv.jf.Close(); err != nil {
		t.Fatal(err)
	}

	// Must not panic, despite the journal write failing.
	createSessionDirect(t, srv, "")
}

// TestSessionPersistErrForwardedOnce verifies that a per-session engine
// persist failure is forwarded to Options.OnError exactly once while the
// same underlying error persists, not on every syncMessages poll (which
// happens after every assistant turn).
//
// The failure is injected deterministically: the session's log path
// (<SessionDir>/<id>.jsonl, per the package doc in store.go) is pre-created
// as a directory, so the engine's OpenFile(..., O_WRONLY) on first append
// fails with a stable "is a directory" error every time it retries.
func TestSessionPersistErrForwardedOnce(t *testing.T) {
	dir := t.TempDir()
	prov := &scriptedProvider{name: "test", turns: [][]provider.Event{
		asstTurn("one"), asstTurn("two"),
	}}
	coll := newErrCollector()
	srv := newServer(t, dir, prov, 0, func(o *Options) {
		o.OnError = coll.onError
	})

	sess, err := srv.opts.NewSession(message.ModelRef{Provider: "test", Model: "m1"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, sess.ID+".jsonl")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	srv.sessions[sess.ID] = &sessionState{sess: sess}
	srv.mu.Unlock()

	if _, err := sess.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	<-coll.ch // the first Prompt's trailing EventMessage -> syncMessages call

	if got := coll.count(); got != 1 {
		t.Fatalf("after first prompt, OnError called %d times, want 1", got)
	}
	first := coll.last()
	if !strings.Contains(first.Error(), "session "+sess.ID+" persist:") {
		t.Errorf("error = %q, want it wrapped with session persist context", first)
	}

	if _, err := sess.Prompt(context.Background(), "go again"); err != nil {
		t.Fatal(err)
	}
	// syncMessages runs again (same EventMessage path); the persist error is
	// unchanged, so no additional OnError call should follow. There is no
	// event to block on for "it did not happen again", so directly re-invoke
	// the polling path the same way Publish would and assert the count is
	// still 1 — synchronous, no sleep involved.
	srv.syncMessages(sess.ID)

	if got := coll.count(); got != 1 {
		t.Fatalf("after second prompt, OnError called %d times, want 1 (not re-forwarded)", got)
	}
}
