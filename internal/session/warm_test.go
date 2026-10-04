package session

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/eventlog"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type extra string

func (e extra) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: string(e)} }
func (extra) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

// warmBackend reports each Warm to calls and holds it until its ctx ends, with block.
type warmBackend struct {
	calls chan warmCall
	block bool
	// off makes CanWarm report false.
	off bool
	// hold, when set, keeps Warm from returning until it closes.
	hold chan struct{}
	// ran, when set, receives a value for each Run.
	ran chan struct{}
}

func (b *warmBackend) CanWarm(string) bool { return !b.off }

type countSource struct{ calls atomic.Int32 }

func (s *countSource) Toolset(context.Context, []eventlog.Message, []string, string) turn.Toolset {
	s.calls.Add(1)
	return turn.Toolset{}
}

type warmCall struct {
	req turn.Request
	ctx context.Context
}

func (*warmBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (b *warmBackend) Run(context.Context, turn.Request, turn.Sink) (turn.Result, error) {
	if b.ran != nil {
		b.ran <- struct{}{}
	}
	return turn.Result{}, nil
}

func (b *warmBackend) Warm(ctx context.Context, req turn.Request) error {
	b.calls <- warmCall{req, ctx}
	if b.hold != nil {
		select {
		case <-b.hold:
		case <-ctx.Done():
		}
	}
	if b.block {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return nil
}

func warmConfig(ctx context.Context, wg *sync.WaitGroup, log *memLog, b turn.Backend) Config {
	return Config{ID: "s1", Log: log, Ownership: owned{}, Backend: b, Source: turn.Fixed{extra("echo")},
		Base: ctx, Go: wg.Go, Done: func() {}, Prompt: func(string) string { return "be brief" }}
}

func TestCreateAndOpenWarmTheBackendOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	b := &warmBackend{calls: make(chan warmCall, 4)}
	log := &memLog{}
	a, err := Create(ctx, warmConfig(ctx, &wg, log, b), eventlog.SessionCreated{Model: "codex/gpt-5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Run()
	c := <-b.calls
	if c.req.SessionID != "s1" || c.req.Model != "codex/gpt-5" || c.req.Instructions != "be brief" ||
		len(c.req.Tools) != 1 || c.req.Tools[0].Name != "echo" || len(c.req.Input) != 0 {
		t.Errorf("warm request on create = %+v", c.req)
	}
	converse(t, a, "hello")
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, err := Open(ctx, warmConfig(ctx, &wg, log, b))
	if err != nil {
		t.Fatal(err)
	}
	w.Run()
	c = <-b.calls
	if len(c.req.History) == 0 {
		t.Errorf("warm request on wake has no history: %+v", c.req)
	}
	if len(b.calls) != 0 {
		t.Errorf("warm calls beyond one per create and wake: %d", len(b.calls))
	}
}

func TestWarmEndsWithTheSessionContextOrItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		b := &warmBackend{calls: make(chan warmCall, 1), block: true}
		a, err := Create(ctx, warmConfig(ctx, &wg, &memLog{}, b), eventlog.SessionCreated{Model: "codex/gpt-5"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		c := <-b.calls
		timer := time.NewTimer(warmTimeout)
		<-timer.C
		synctest.Wait()
		if err := c.ctx.Err(); err == nil {
			t.Error("the warm-up ran past its timeout")
		}
		cancel()
		wg.Wait()
	})
}

func TestReleaseEndsAWarmUpThatIsStillRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		b := &warmBackend{calls: make(chan warmCall, 1), block: true}
		a, err := Create(ctx, warmConfig(ctx, &wg, &memLog{}, b), eventlog.SessionCreated{Model: "codex/gpt-5"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		c := <-b.calls
		if err := a.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if c.ctx.Err() == nil {
			t.Error("the warm-up outlived its session")
		}
		cancel()
		wg.Wait()
	})
}

func TestAModelThatCannotWarmCostsNoToolDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	src := &countSource{}
	cfg := warmConfig(ctx, &wg, &memLog{}, &warmBackend{calls: make(chan warmCall, 1), off: true})
	cfg.Source = src
	a, err := Create(ctx, cfg, eventlog.SessionCreated{Model: "anthropic/claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Run()
	if err := a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancel()
	wg.Wait()
	if n := src.calls.Load(); n != 0 {
		t.Errorf("tool discovery for a model that cannot warm = %d, want 0", n)
	}
}

func queuedLog(t *testing.T) *memLog {
	t.Helper()
	log := &memLog{}
	for i, ev := range []eventlog.Event{
		eventlog.SessionCreated{Model: "codex/gpt-5"},
		eventlog.OwnerAcquired{Epoch: 1},
		eventlog.InputAdmitted{InputID: "in1", Delivery: eventlog.DeliveryQueue, Source: "user",
			Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "hello"}}},
	} {
		data, err := eventlog.Envelope{Seq: uint64(i + 1), Time: time.Now(), Event: ev}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(context.Background(), uint64(i), data); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func TestFirstTurnWaitsForTheWarmUpInFlight(t *testing.T) {
	for _, wake := range []bool{false, true} {
		name := "create"
		if wake {
			name = "wake"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				var wg sync.WaitGroup
				b := &warmBackend{calls: make(chan warmCall, 2), hold: make(chan struct{}), ran: make(chan struct{}, 2)}
				var a *Actor
				var err error
				if wake {
					a, err = Open(ctx, warmConfig(ctx, &wg, queuedLog(t), b))
				} else {
					first := &eventlog.InputAdmitted{InputID: "in1", Delivery: eventlog.DeliveryQueue, Source: "user",
						Parts: []eventlog.Part{{Type: eventlog.PartText, Text: "hello"}}}
					a, err = Create(ctx, warmConfig(ctx, &wg, &memLog{}, b), eventlog.SessionCreated{Model: "codex/gpt-5"}, first)
				}
				if err != nil {
					t.Fatal(err)
				}
				a.Run()
				<-b.calls
				synctest.Wait()
				if len(b.ran) != 0 {
					t.Error("the first turn ran while the warm-up was in flight")
				}
				close(b.hold)
				<-b.ran
				cancel()
				wg.Wait()
			})
		})
	}
}
