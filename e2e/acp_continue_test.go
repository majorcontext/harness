package e2e

import (
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// runACPContinue runs steps against a Codex double on each native ACP host, then calls check with
// the session ID and a reader of the stored session.
func runACPContinue(t *testing.T, steps []harnesstest.Step, body func(t *testing.T, c *acpConn, o *harnesstest.OpenAI, snap func() protocol.Session)) {
	t.Helper()
	skipShort(t)
	schema := loadACPSchema(t)
	for _, host := range acpHosts() {
		t.Run(host.name, func(t *testing.T) {
			t.Parallel()
			o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{APIKey: codexAPIKey}, steps...)
			env := acpEnv{sessDir: t.TempDir(), defaultDir: t.TempDir()}
			env.configPath = writeGoalConfigWith(t, "", codexConfig(o.URL(), false, nil))
			in, out, stop := host.start(t, env)
			c := newACPConn(t, schema, in, out)
			c.fake, c.env = o.Server, env
			c.paths[env.configPath] = "<user-config>"
			c.dirs["main"] = writeACPDir(t, nil)
			acpNew{as: "a"}.run(t, c)
			snap := func() protocol.Session {
				v, err := harness.OpenView(t.Context(), store.NewDiskStore(env.sessDir), c.session("a"))
				if err != nil {
					t.Fatalf("read the stored session: %v", err)
				}
				return v.Snapshot()
			}
			body(t, c, o, snap)
			c.drain()
			c.finish(stop)
		})
	}
}

func TestACPCancelBeforeTheUsageLimitResetsCancelsTheContinue(t *testing.T) {
	runACPContinue(t, []harnesstest.Step{{Name: "limit", Reply: codexLimit(time.Hour)}},
		func(t *testing.T, c *acpConn, _ *harnesstest.OpenAI, snap func() protocol.Session) {
			acpPromptStart{as: "a", text: "go"}.run(t, c)
			c.response(c.pending["a"])
			if snap().ContinueAt == 0 {
				t.Fatal("no pending continue after the usage limit")
			}
			acpCancel{as: "a"}.run(t, c)
			testpoll.Until(t, waitBound, "the stored session still has a pending continue after session/cancel", func() bool { return snap().ContinueAt == 0 })
		})
}

func TestACPCancelStopsTheAutomaticTurnAfterTheUsageLimitResets(t *testing.T) {
	steps := []harnesstest.Step{
		{Name: "limit", Reply: codexLimit(-time.Minute)},
		{Name: "slow", Match: harnesstest.LastUserText(resetCue), Reply: harnesstest.Reply{HTTPStatus: 429, RetryAfter: "1", ErrorMessage: "slow down"}},
	}
	runACPContinue(t, steps, func(t *testing.T, c *acpConn, o *harnesstest.OpenAI, snap func() protocol.Session) {
		acpPromptStart{as: "a", text: "go"}.run(t, c)
		c.response(c.pending["a"])
		if !o.AwaitRequests(2, waitBound) {
			t.Fatal("the automatic turn never called the model")
		}
		acpCancel{as: "a"}.run(t, c)
		testpoll.Until(t, waitBound, "the automatic turn is still running after session/cancel", func() bool {
			l := snap().LastTurn
			return l != nil && l.StopReason == protocol.StopInterrupted
		})
		acpAwaitUpdate{kind: "session_info_update"}.run(t, c)
	})
}

func TestACPPromptAfterAnIdleCancelRunsToCompletion(t *testing.T) {
	steps := []harnesstest.Step{{Name: "limit", Reply: codexLimit(time.Hour)}, {Name: "ok", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}}
	runACPContinue(t, steps, func(t *testing.T, c *acpConn, _ *harnesstest.OpenAI, _ func() protocol.Session) {
		acpPromptStart{as: "a", text: "go"}.run(t, c)
		c.response(c.pending["a"])
		for i := range 20 {
			acpCancel{as: "a"}.run(t, c)
			id := "again" + string(rune('a'+i))
			acpPromptStart{as: "a", id: id, text: "again"}.run(t, c)
			acpResult{id: id, stop: "end_turn"}.run(t, c)
		}
		acpAwaitUpdate{kind: "session_info_update"}.run(t, c)
	})
}

func TestACPCloseCancelsThePendingContinue(t *testing.T) {
	runACPContinue(t, []harnesstest.Step{{Name: "limit", Reply: codexLimit(time.Hour)}},
		func(t *testing.T, c *acpConn, _ *harnesstest.OpenAI, snap func() protocol.Session) {
			acpPromptStart{as: "a", text: "go"}.run(t, c)
			c.response(c.pending["a"])
			if snap().ContinueAt == 0 {
				t.Fatal("no pending continue after the usage limit")
			}
			c.mustBeResult(c.response(c.request("session/close", map[string]any{"sessionId": c.session("a")})), "CloseSessionResponse")
			if at := snap().ContinueAt; at != 0 {
				t.Fatalf("continue_at after session/close = %d, want none, so a later load runs no continue", at)
			}
		})
}
