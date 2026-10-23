package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/internal/modelref"
	"github.com/majorcontext/harness/protocol"
)

// readingTime is the record time of the newest context.measured record that measured tokens.
func readingTime(t *testing.T, d *runtimeDriver, id string) time.Time {
	t.Helper()
	var at time.Time
	for _, ev := range d.events(t, id) {
		var m protocol.ContextMeasured
		if ev.Kind == protocol.KindContextMeasured && json.Unmarshal(ev.Data, &m) == nil && m.Tokens > 0 {
			at = ev.Time
		}
	}
	if at.IsZero() {
		t.Fatal("the log holds no context reading")
	}
	return at
}

func TestContractSessionCacheExpiryAndCompactionPoint(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a model API session expires its cache one hour after the reading and loses it on compaction", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), map[string]any{"compaction_keep_turns": 1},
				harnesstest.Step{Name: "summary", Match: summaryRequest, Reply: harnesstest.Reply{Text: "gist"}},
				replyText("hi"))
			window, ok := modelmeta.ContextWindow(modelref.ModelRef{Provider: "anthropic", Model: "claude-opus-5"})
			if !ok {
				t.Fatal("the table does not know anthropic/claude-opus-5")
			}
			id := createdID(t, d, "anthropic/claude-opus-5")
			for _, prompt := range []string{"one", "two"} {
				d.Submit(t, id, prompt)
				d.WaitIdle(t, id)
			}
			v := d.view(t, id)
			if want := readingTime(t, d, id).Add(time.Hour); !v.CacheExpiresAt.Equal(want) {
				t.Errorf("cache_expires_at = %v, want the reading time plus 1h: %v", v.CacheExpiresAt, want)
			}
			if want := int64(0.8 * float64(window)); v.Context.CompactAt != want {
				t.Errorf("compact_at = %d, want 0.8 of the window %d: %d", v.Context.CompactAt, window, want)
			}
			if res := d.Compact(t, id); res.Status != http.StatusOK {
				t.Fatalf("compact = %d %v", res.Status, res.Body)
			}
			if got := d.view(t, id).CacheExpiresAt; !got.IsZero() {
				t.Errorf("cache_expires_at after a compaction = %v, want none", got)
			}
		})
		t.Run("a claude-code session expires its cache one hour after a call that wrote 1h tokens", func(t *testing.T) {
			fake := harnesstest.NewAnthropic(t)
			cd := claudeLane{mode: "cache_1h"}.newDriver(t, h, fake.URL()).(*claudeDriver)
			d := cd.laneHost.(*runtimeDriver)
			id := createdID(t, d, "claude-code/sonnet")
			d.Submit(t, id, "run it")
			d.WaitIdle(t, id)
			if want, got := readingTime(t, d, id).Add(time.Hour), d.view(t, id).CacheExpiresAt; !got.Equal(want) {
				t.Errorf("cache_expires_at = %v, want the reading time plus 1h: %v", got, want)
			}
			raw, err := os.ReadFile(cd.envLog)
			if err != nil {
				t.Fatal(err)
			}
			var env []string
			if err := json.Unmarshal(raw, &env); err != nil || !slices.Contains(env, "ENABLE_PROMPT_CACHING_1H=1") {
				t.Errorf("the claude env (%v) lacks ENABLE_PROMPT_CACHING_1H=1", err)
			}
		})
	})
}
