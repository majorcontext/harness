package session

import (
	"context"
	"errors"
	"github.com/majorcontext/harness/internal/prompt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

type extra string

func (e extra) Spec() protocol.ToolSpec { return protocol.ToolSpec{Name: string(e)} }
func (extra) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

// warmBackend reports each Warm to calls and holds it until its ctx ends.
type warmBackend struct {
	calls chan warmCall
}

type warmCall struct {
	req turn.Request
	ctx context.Context
}

func (*warmBackend) CanWarm(string) bool { return true }

func (*warmBackend) Capabilities(string) turn.Capabilities { return turn.Capabilities{} }

func (*warmBackend) Run(context.Context, turn.Request, turn.Sink) error {
	return nil
}

func (b *warmBackend) Warm(ctx context.Context, req turn.Request) error {
	b.calls <- warmCall{req, ctx}
	<-ctx.Done()
	return context.Cause(ctx)
}

func warmConfig(ctx context.Context, wg *sync.WaitGroup, log *memLog, b turn.Backend) Config {
	return Config{ID: "s1", Store: log, Ownership: owned{}, Backend: b, Source: turn.Fixed{extra("echo")},
		Base: ctx, Go: wg.Go, Done: func(closeDone func()) { closeDone() }, Prompt: func(context.Context) prompt.Info { return prompt.Info{Segments: []string{"be brief"}} }}
}

func TestWarmEndsWithTheSessionContextOrItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		b := &warmBackend{calls: make(chan warmCall, 1)}
		a, err := Create(ctx, warmConfig(ctx, &wg, &memLog{}, b), protocol.SessionCreated{Model: "codex/gpt-5"}, nil)
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
		b := &warmBackend{calls: make(chan warmCall, 1)}
		a, err := Create(ctx, warmConfig(ctx, &wg, &memLog{}, b), protocol.SessionCreated{Model: "codex/gpt-5"}, nil)
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

// ranBackend reports each Run to ran.
type ranBackend struct {
	warmBackend
	ran chan struct{}
}

func (b *ranBackend) Run(context.Context, turn.Request, turn.Sink) error {
	b.ran <- struct{}{}
	return nil
}

func TestAWarmUpEndsAtItsDeadlineWhenThePromptReadWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		b := &ranBackend{warmBackend: warmBackend{calls: make(chan warmCall, 1)}, ran: make(chan struct{}, 1)}
		cfg := warmConfig(ctx, &wg, &memLog{}, b)
		var reads atomic.Int32
		ended := make(chan error, 1)
		cfg.Prompt = func(c context.Context) prompt.Info {
			if reads.Add(1) == 1 {
				<-c.Done()
				ended <- c.Err()
			}
			return prompt.Info{}
		}
		a, err := Create(ctx, cfg, protocol.SessionCreated{Model: "codex/gpt-5"}, firstInput())
		if err != nil {
			t.Fatal(err)
		}
		a.Run()
		timer := time.NewTimer(warmTimeout)
		<-timer.C
		synctest.Wait()
		if err := <-ended; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the prompt read of the warm-up ended with %v, want its deadline", err)
		}
		<-b.ran
		cancel()
		wg.Wait()
	})
}
