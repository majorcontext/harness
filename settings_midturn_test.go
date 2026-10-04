package harness_test

import (
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

func TestSettingsChangeReachesTheNextModelCallOfARunningTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bash, f := newProbe("bash", true), newFake()
		f.ownsLoop = false
		r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Tools: []harness.Tool{bash}}, f)
		if err != nil {
			t.Fatal(err)
		}
		s := create(t, r)
		submit(t, s, text("a", "hi"))
		first := <-f.runs
		first.emit(callTool("c1"))
		first.end()
		<-bash.started
		model, effort := "test/other", "high"
		if _, err := s.Update(bg, protocol.SettingsPatch{Model: &model, Effort: &effort}); err != nil {
			t.Fatal(err)
		}
		close(bash.release)
		second := <-f.runs
		if second.req.Model != model || second.req.Settings.Effort != effort {
			t.Errorf("model call after the change = %s, %q; want %s, %q", second.req.Model, second.req.Settings.Effort, model, effort)
		}
		if first.req.Model != "test/model" {
			t.Errorf("model call before the change = %s, want test/model", first.req.Model)
		}
		second.end()
		closeRuntime(t, r)
	})
}
