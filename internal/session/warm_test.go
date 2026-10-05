package session

import (
	"context"
	"sync"
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

func (*warmBackend) Run(context.Context, turn.Request, turn.Sink) (turn.Result, error) {
	return turn.Result{}, nil
}

func (b *warmBackend) Warm(ctx context.Context, req turn.Request) error {
	b.calls <- warmCall{req, ctx}
	<-ctx.Done()
	return context.Cause(ctx)
}

func warmConfig(ctx context.Context, wg *sync.WaitGroup, log *memLog, b turn.Backend) Config {
	return Config{ID: "s1", Store: log, Ownership: owned{}, Backend: b, Source: turn.Fixed{extra("echo")},
		Base: ctx, Go: wg.Go, Done: func() {}, Prompt: func() string { return "be brief" }}
}

func TestWarmEndsWithTheSessionContextOrItsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		b := &warmBackend{calls: make(chan warmCall, 1)}
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
		b := &warmBackend{calls: make(chan warmCall, 1)}
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
