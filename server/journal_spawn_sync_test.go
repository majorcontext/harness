package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// TestChildJournaledAfterParentIdleEvictedAndReloaded is the regression
// test for a live-audited bug (BUG A): two children spawned via the `task`
// tool from a root that had gone 13.5+ hours idle (long since evicted from
// server residency) showed ZERO rows in the shared durable journal
// (events.jsonl) — no message, no request.meta, nothing — even though
// their own per-session transcripts were complete. Seven other children,
// spawned promptly after their root's own creation, journaled fine.
//
// Root cause, confirmed by reading (not guessing): Server.syncMessages —
// the ONE function that journals durable "message" records, live (on each
// assistant turn, via Publish's engine.EventMessage case) and at boot
// (reconcile) alike — resolved its *engine.Session via a bare
// s.sessions[sessionID] read. s.sessions is this server's OWN HTTP claim/
// eviction residency map, populated ONLY by handleCreate and
// claimForPrompt's cold-load path. A session SessionManager.Spawn creates
// (the `task` tool, or session.create's parent_id form) is NEVER inserted
// into s.sessions by ANY code path — Spawn drives the child's turn
// directly (child.Prompt, in its own goroutine — see Spawn's doc comment),
// entirely bypassing claimForPrompt/handleCreate. So syncMessages silently
// and PERMANENTLY dropped every message from every spawned child's own
// turn — not merely a timing-dependent subset — unless something else
// LATER, independently, sent that exact child a direct HTTP prompt (which
// cold-loads it into s.sessions and backfills its entire history at once
// via runPrompt's own "catch any message not yet journaled" call). Whether
// an external caller happens to do that afterward — not spawn timing
// itself — is what actually decided "journals fine" vs. "zero rows": an
// idle root is simply the shape where an orchestrator has moved on and
// never revisits an autonomous, fire-and-forget child (the PARENT gets the
// completion notification, not the child), so nothing ever backfills it.
//
// The fix (Server.liveSessionObject, server/live.go): fall back to
// SessionManager's OWN live node — which Spawn registers synchronously,
// before the child's turn ever starts, read as one atomic pair through
// SessionAndInfo — whenever s.sessions has nothing. This test reproduces the exact reported
// sequence deterministically, entirely inside a synctest bubble (no real
// listener, no time.Sleep — see AGENTS.md's test time.Sleep ban): create
// root, evict it from residency by exceeding MaxResident
// (evictResidentLocked is pure LRU-by-count, not time-based, so nothing
// here depends on the bubble's fake clock advancing at all), reload it via
// a fresh prompt_async that spawns a child through the `task` tool, let
// synctest.Wait() settle every goroutine the bubble owns (root's turn, the
// tool loop, Spawn's own child-driving goroutine — none of which touch
// real NETWORK I/O; SessionDir is a real t.TempDir(), so Persist's disk
// writes are real but synchronous, settling cleanly under Wait()), then
// assert the child's own message events actually reached the shared
// journal.
func TestChildJournaledAfterParentIdleEvictedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		rootProv := &scriptedProvider{name: "root", turns: [][]provider.Event{
			// model is explicit here — omitting it would inherit the
			// PARENT's own live model (Spawn's "zero Model means inherit"
			// rule), sending the child's own turn through rootProv instead
			// of childProv and corrupting both scripts' call counters.
			toolCallTurn("tc1", "task", `{"agent":"general-purpose","prompt":"find the answer","model":"child/m1"}`),
			asstTurn("spawned it"),
			// A third turn absorbs the automatic resume runPrompt fires to
			// deliver the child's completion notification back to root
			// (nearestLiveAncestorLocked finds root idle the instant the
			// child settles and triggers resumeSessionForTaskNotification)
			// — a real production turn this test must account for, not an
			// artifact of the fix under test.
			asstTurn("noted"),
		}}
		childProv := &scriptedProvider{name: "child", turns: [][]provider.Event{
			asstTurn("the answer is 42"),
		}}
		blockerProv := &scriptedProvider{name: "blocker", turns: [][]provider.Event{asstTurn("noop")}}
		reg := provider.Registry{
			rootProv.Name():    rootProv,
			childProv.Name():   childProv,
			blockerProv.Name(): blockerProv,
		}

		var srv *Server
		opts := Options{
			SessionDir:  dir,
			RunToken:    "secret-run-token",
			Version:     "9.9.9",
			MaxResident: 1,
			NewSession: func(m message.ModelRef, workDir, parentSession string) (*engine.Session, error) {
				return engine.NewSession(engine.Config{
					Providers:     reg,
					Model:         m,
					WorkDir:       workDir,
					ParentSession: parentSession,
					SessionDir:    dir,
					OnEvent:       func(ev engine.Event) { srv.Publish(ev) },
					// SessionManager, not just Options.SessionManager below:
					// newSession only installs the `task` tool when THIS
					// field is set (see its own doc comment) — production's
					// cmd/harness mkCfg sets it identically.
					SessionManager: srv.sessMgr,
				}), nil
			},
			LoadSession: func(id string) (*engine.Session, error) {
				return engine.LoadSession(engine.Config{
					Providers:      reg,
					SessionDir:     dir,
					OnEvent:        func(ev engine.Event) { srv.Publish(ev) },
					SessionManager: srv.sessMgr,
				}, id)
			},
		}
		var err error
		srv, err = New(opts)
		if err != nil {
			t.Fatal(err)
		}

		rootID := createSessionDirect(t, srv, "root/m1")

		// Back-to-back time.Now() calls (root's own handleCreate register
		// step, immediately followed by blocker's) can read the EXACT SAME
		// instant — a synctest bubble's fake clock only advances once
		// every goroutine in it is durably blocked, and even under a real
		// clock two calls this close together can tie on some platforms.
		// A tie leaves evictResidentLocked's sort (Before, false on a tie
		// either way) with no ordering to go on, and Go's randomized map
		// iteration over s.sessions feeds it in no fixed order — so which
		// of the two gets evicted becomes non-deterministic. Backdating
		// root's own lastUsed directly (no time.Sleep, no dependency on
		// the clock advancing at all — see AGENTS.md's test time.Sleep
		// ban) makes it strictly earlier, so eviction below is
		// deterministic — a property of the TEST's own bookkeeping, not a
		// hidden real-time dependency in evictResidentLocked itself, which
		// is pure LRU-by-count and never polls a clock on its own.
		srv.mu.Lock()
		srv.sessions[rootID].lastUsed = srv.sessions[rootID].lastUsed.Add(-time.Minute)
		srv.mu.Unlock()

		// Push root out of residency deterministically: a second session's
		// own registration exceeds MaxResident=1, and evictResidentLocked
		// (pure LRU-by-count, never a background sweep) evicts the one
		// non-running resident with the oldest lastUsed — root, since
		// nothing has run on it yet and (per the tick above) it was
		// created strictly first.
		_ = createSessionDirect(t, srv, "blocker/m1")
		if srv.residentSession(rootID) != nil {
			t.Fatal("root still resident after blocker creation — test setup invalid")
		}

		// Reload root (claimForPrompt's cold-load path, inside handlePrompt)
		// and drive a turn that spawns a child via the `task` tool — the
		// exact idle-evict -> reload -> spawn sequence the live audit
		// reproduced.
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/session/"+rootID+"/prompt_async",
			strings.NewReader(`{"parts":[{"type":"text","text":"go"}]}`))
		req.SetPathValue("id", rootID)
		srv.handlePrompt(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("prompt_async status %d: %s", rec.Code, rec.Body)
		}

		// Lets root's turn, the task-tool call, Spawn's own child-driving
		// goroutine, and the child's turn all run to completion — every one
		// of them belongs to this bubble (transitively launched from code
		// running inside it). None of them touch real NETWORK I/O
		// (scripted providers), so nothing here blocks on anything a
		// bubble forbids — but SessionDir is a real t.TempDir(), so
		// Persist's own MkdirAll/OpenFile/append calls ARE real, synchronous
		// disk writes; they simply complete rather than blocking durably,
		// so synctest.Wait() still settles cleanly. This blocks until all
		// of it has genuinely finished, deterministically, with zero real
		// wall-clock cost.
		synctest.Wait()

		info, ok := srv.sessMgr.Info(rootID)
		if !ok || len(info.Children) != 1 {
			t.Fatalf("root lineage children = %v, want exactly 1 spawned child", info.Children)
		}
		childID := info.Children[0]

		var childMessages int
		for _, ev := range srv.journal {
			if ev.SessionID == childID && ev.Type == evtMessage {
				childMessages++
			}
		}
		if childMessages == 0 {
			t.Fatalf("child %s has ZERO message events in the shared journal after idle-evict -> reload -> spawn (BUG A); full journal: %+v", childID, srv.journal)
		}
	})
}
