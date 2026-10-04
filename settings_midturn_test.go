package harness_test

import (
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/turn"
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

// byModel owns the loop only for model test/own.
type byModel struct{ *fake }

func (byModel) Capabilities(model string) turn.Capabilities {
	return turn.Capabilities{OwnsLoop: model == "test/own"}
}

func TestSettingsChangeToAnotherKindOfBackendWaitsForTheNextTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bash, f := newProbe("bash", true), newFake()
		r, err := harness.NewWithBackend(harness.Options{Store: harness.NewMemStore(), Tools: []harness.Tool{bash}}, byModel{f})
		if err != nil {
			t.Fatal(err)
		}
		s := create(t, r)
		submit(t, s, text("a", "hi"))
		first := <-f.runs
		first.emit(callTool("c1"))
		first.end()
		<-bash.started
		own := "test/own"
		if _, err := s.Update(bg, protocol.SettingsPatch{Model: &own}); err != nil {
			t.Fatal(err)
		}
		close(bash.release)
		second := <-f.runs
		if second.req.Model != "test/model" {
			t.Errorf("model call after a change to a loop-owning model = %s, want test/model until the next turn", second.req.Model)
		}
		second.end()
		submit(t, s, text("b", "again"))
		if next := <-f.runs; next.req.Model != own {
			t.Errorf("next turn model = %s, want %s", next.req.Model, own)
		} else {
			next.end()
		}
		closeRuntime(t, r)
	})
}
