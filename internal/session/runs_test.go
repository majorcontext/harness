package session

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
)

// kindBackend answers each model call at once and keeps the request of the
// newest call of each kind of run.
type kindBackend struct {
	mu   sync.Mutex
	reqs map[runKind]turn.Request
}

func (*kindBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *kindBackend) Run(_ context.Context, req turn.Request, out turn.Sink) (turn.Result, error) {
	kind, text := kindTurn, "ok"
	switch req.Instructions {
	case evaluatorPrompt:
		kind, text = kindJudge, "MET: ok"
	case "":
	default:
		kind, text = kindCompaction, "summary"
	}
	b.mu.Lock()
	if b.reqs == nil {
		b.reqs = map[runKind]turn.Request{}
	}
	b.reqs[kind] = req
	b.mu.Unlock()
	return turn.Result{}, out.Item(eventlog.Message{Role: eventlog.RoleAssistant, Parts: []eventlog.Part{{Type: eventlog.PartText, Text: text}}})
}

func TestSideCallsKeepTheParametersOfTheEngine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &kindBackend{}
		cfg := actorConfig(t, &memLog{}, owned{}, b)
		cfg.Evaluator, cfg.KeepTurns, cfg.Threshold = "m/eval", 1, 0.8
		a, err := Create(context.Background(), cfg, eventlog.SessionCreated{Model: "m/m", Settings: eventlog.Settings{Effort: "high"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		converse(t, a, "one", "two")
		if _, _, err := a.Compact(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if err := a.SetGoal(context.Background(), "say done", 0); err != nil {
			t.Fatal(err)
		}
		settled(a)
		if r := b.reqs[kindJudge]; r.MaxTokens != 256 || r.Settings.Effort != "off" {
			t.Errorf("evaluator request: MaxTokens %d, effort %q, want 256 and off", r.MaxTokens, r.Settings.Effort)
		}
		if r := b.reqs[kindCompaction]; r.MaxTokens != 1024 || r.Settings.Effort != "high" {
			t.Errorf("summary request: MaxTokens %d, effort %q, want 1024 and the session effort", r.MaxTokens, r.Settings.Effort)
		}
		if r := b.reqs[kindTurn]; r.MaxTokens != 0 {
			t.Errorf("turn request MaxTokens = %d, want the backend default (0)", r.MaxTokens)
		}
	})
}
