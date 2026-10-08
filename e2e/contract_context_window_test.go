package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/message"
	"github.com/majorcontext/harness/internal/modelmeta"
	"github.com/majorcontext/harness/protocol"
)

const unknownModel = "anthropic/no-such-model"

// turnOn creates a session on model, runs one prompt to idle, and returns its view.
func turnOn(t *testing.T, d *runtimeDriver, model, prompt string) protocol.Session {
	t.Helper()
	id := createdID(t, d, model)
	d.Submit(t, id, prompt)
	d.WaitIdle(t, id)
	return d.view(t, id)
}

func TestContractContextWindowOfAModel(t *testing.T) {
	skipShort(t)
	noWindow := map[string]any{"context_window_tokens": 0}
	onHosts(t, func(t *testing.T, h host) {
		t.Run("an unknown model runs with the default window marked estimated", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), noWindow, replyText("hi"))
			v := turnOn(t, d, unknownModel, "hello")
			if v.Context.Window != 128000 || !v.Context.WindowEstimated {
				t.Errorf("context = %+v, want window 128000 estimated", v.Context)
			}
			if v.LastTurn == nil || v.LastTurn.StopReason != "completed" || v.Context.Tokens == 0 {
				t.Errorf("last turn %+v, context tokens %d: want a completed turn that measured its prompt", v.LastTurn, v.Context.Tokens)
			}
		})
		t.Run("a known model reports its table window, not estimated", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), noWindow, replyText("hi"))
			want, ok := modelmeta.ContextWindow(message.ModelRef{Provider: "anthropic", Model: "claude-opus-5"})
			if !ok {
				t.Fatal("the table does not know anthropic/claude-opus-5")
			}
			v := turnOn(t, d, "anthropic/claude-opus-5", "hello")
			if v.Context.Window != int64(want) || v.Context.WindowEstimated {
				t.Errorf("context = %+v, want window %d not estimated", v.Context, want)
			}
		})
		t.Run("a model API provider keyed claude-code has no table window and runs on the default, marked estimated", func(t *testing.T) {
			fake := harnesstest.NewChat(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "hi"}, Repeat: true})
			cfg := scenarioConfig(map[string]any{
				"context_window_tokens": 0,
				"providers": map[string]any{
					"anthropic":   map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": fake.URL()},
					"claude-code": map[string]any{"type": "openai-compat", "api_key_env": "ANTHROPIC_API_KEY", "base_url": fake.URL()},
				},
			})
			d := h.openIn(t, writeGoalConfigWith(t, fake.URL(), cfg), t.TempDir(), nil).(*runtimeDriver)
			v := turnOn(t, d, "claude-code/sonnet", "hello")
			if v.Context.Window != 128000 || !v.Context.WindowEstimated {
				t.Errorf("context = %+v, want window 128000 estimated", v.Context)
			}
		})
		t.Run("a configured window beats the table", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), map[string]any{"context_window_tokens": 1000}, replyText("hi"))
			v := turnOn(t, d, "anthropic/claude-opus-5", "hello")
			if v.Context.Window != 1000 || v.Context.WindowEstimated {
				t.Errorf("context = %+v, want window 1000 not estimated", v.Context)
			}
		})
		t.Run("a configured window is not an estimate", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), map[string]any{"context_window_tokens": 1000}, replyText("hi"))
			v := turnOn(t, d, unknownModel, "hello")
			if v.Context.Window != 1000 || v.Context.WindowEstimated {
				t.Errorf("context = %+v, want window 1000 not estimated", v.Context)
			}
		})
	})
}

func TestContractGaugeOfAStoppedSessionMatchesTheLiveGauge(t *testing.T) {
	skipShort(t)
	same := func(t *testing.T, d *runtimeDriver, id string, live protocol.Session) {
		t.Helper()
		d.Restart(t, false)
		if got := d.view(t, id).Context; got.Window != live.Context.Window || got.WindowEstimated != live.Context.WindowEstimated {
			t.Errorf("gauge after a restart = %+v, live gauge = %+v: want the same window and estimated flag", got, live.Context)
		}
	}
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a configured window", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), map[string]any{"context_window_tokens": 1000}, replyText("hi"))
			id := createdID(t, d, unknownModel)
			d.Submit(t, id, "hello")
			d.WaitIdle(t, id)
			live := d.view(t, id)
			if live.Context.Window != 1000 || live.Context.WindowEstimated {
				t.Fatalf("live gauge = %+v, want window 1000 not estimated", live.Context)
			}
			same(t, d, id, live)
		})
		t.Run("the default window", func(t *testing.T) {
			d, _ := startOn(t, h, t.TempDir(), map[string]any{"context_window_tokens": 0}, replyText("hi"))
			id := createdID(t, d, unknownModel)
			d.Submit(t, id, "hello")
			d.WaitIdle(t, id)
			live := d.view(t, id)
			if live.Context.Window != 128000 || !live.Context.WindowEstimated {
				t.Fatalf("live gauge = %+v, want window 128000 estimated", live.Context)
			}
			same(t, d, id, live)
		})
	})
}

func TestContractUnknownModelWarnsOncePerModel(t *testing.T) {
	skipShort(t)
	t.Parallel()
	fake := harnesstest.New(t, replyText("hi"))
	d := newServeDriverIn(t, writeGoalConfigWith(t, fake.URL(), scenarioConfig(map[string]any{"context_window_tokens": 0})), nil, t.TempDir())
	for _, model := range []string{unknownModel, unknownModel, unknownModel + "-either"} {
		turnOn(t, d, model, "hello")
	}
	awaitLog(t, d, `"model":"`+unknownModel+`-either"`)
	warns := func(model string) int {
		n := 0
		for line := range strings.SplitSeq(d.Stderr(), "\n") {
			if strings.Contains(line, `"level":"WARN"`) && strings.Contains(line, fmt.Sprintf(`"model":%q`, model)) && strings.Contains(line, `"window":128000`) {
				n++
			}
		}
		return n
	}
	for _, model := range []string{unknownModel, unknownModel + "-either"} {
		if n := warns(model); n != 1 {
			t.Errorf("WARN lines for %s = %d, want 1\n%s", model, n, d.Stderr())
		}
	}
}

type recordedWarns struct {
	mu    sync.Mutex
	lines []string
}

func (w *recordedWarns) Enabled(context.Context, slog.Level) bool { return true }
func (w *recordedWarns) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *recordedWarns) WithGroup(string) slog.Handler            { return w }
func (w *recordedWarns) Handle(_ context.Context, r slog.Record) error {
	if r.Level != slog.LevelWarn {
		return nil
	}
	var model string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "model" {
			model = a.Value.String()
		}
		return true
	})
	w.mu.Lock()
	w.lines = append(w.lines, model)
	w.mu.Unlock()
	return nil
}

func (w *recordedWarns) count(model string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Count(strings.Join(w.lines, "\n")+"\n", model+"\n")
}

func TestContractUnknownModelWarnsOncePerProcess(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	warns := &recordedWarns{}
	prev := slog.Default()
	slog.SetDefault(slog.New(warns))
	t.Cleanup(func() { slog.SetDefault(prev) })
	model := fmt.Sprintf("bifrost/vendor/process-wide-unknown-%d", time.Now().UnixNano())
	st := harness.NewMemStore()
	for i := range 2 {
		fake := harnesstest.NewChat(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "hi"}})
		r, err := harness.New(harness.Options{Store: st, WorkDir: t.TempDir(), Config: config.Config{
			Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("s%d", i)
		s, err := r.Create(t.Context(), protocol.CreateSession{ID: id, Model: model})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "hello"}}}); err != nil {
			t.Fatal(err)
		}
		for e, err := range s.Events(t.Context(), 0) {
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind == "turn.ended" {
				break
			}
		}
		if err := r.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := warns.count(model); n != 1 {
		t.Errorf("WARN lines for %s across two runtimes = %d, want 1", model, n)
	}
	v, err := harness.OpenView(t.Context(), st, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Session().Context; got.Window != 128000 || !got.WindowEstimated {
		t.Errorf("OpenView gauge = %+v, want window 128000 estimated, as the live session reported", got)
	}
}
