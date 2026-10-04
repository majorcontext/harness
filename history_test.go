package harness_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/protocol"
)

// counting is a Store that counts the reads of session records.
type counting struct {
	harness.Store
	reads atomic.Int64
}

func (c *counting) Read(ctx context.Context, id string, after uint64, limit int) ([]harness.Record, error) {
	c.reads.Add(1)
	return c.Store.Read(ctx, id, after, limit)
}

func TestAPluginReadsTheHistoryOfARunningSessionFromTheActor(t *testing.T) {
	st := &counting{Store: harness.NewMemStore()}
	f := newFake()
	f.ownsLoop = false
	r, err := harness.NewWithBackend(harness.Options{Store: st, Config: config.Config{Plugins: pluginFixture(t, `{"recall":true}`)}}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeRuntime(t, r) })
	sess, err := r.Create(bg, protocol.CreateSession{ID: "s1", Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Submit(bg, text("a", "go")); err != nil {
		t.Fatal(err)
	}
	run := <-f.runs
	if !strings.Contains(run.req.Instructions, "LAST-USER: go") {
		t.Fatalf("instructions %q do not show that the plugin read the history", run.req.Instructions)
	}
	if n := st.reads.Load(); n != 0 {
		t.Errorf("store reads for the history of a running session = %d, want 0", n)
	}
	close(run.items)
	awaitTurns(t, sess, 1)
}
