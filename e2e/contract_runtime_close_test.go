package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// stubborn is a tool that ignores its context: it reports the call on started
// and returns when the test closes release.
type stubborn struct {
	started chan struct{}
	release chan struct{}
}

func (stubborn) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "stuck", Description: "stuck", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (s stubborn) Run(context.Context, protocol.ToolCall) (protocol.ToolResult, error) {
	s.started <- struct{}{}
	<-s.release
	return protocol.ToolResult{Text: "done"}, nil
}

// counted is an Owner that counts the releases of its grants.
type counted struct{ released *atomic.Int32 }

func (c counted) Acquire(context.Context, string) (harness.Ownership, error) { return c, nil }
func (c counted) Epoch() uint64                                              { return 1 }
func (c counted) Lost() <-chan struct{}                                      { return nil }
func (c counted) Release()                                                   { c.released.Add(1) }

// stuckRuntime runs a session whose turn waits in a tool that ignores ctx.
func stuckRuntime(t *testing.T, owner harness.Owner) (*harness.Runtime, stubborn) {
	t.Helper()
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "call", Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "c1", Name: "stuck"}}}})
	tool := stubborn{started: make(chan struct{}, 1), release: make(chan struct{})}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Owner: owner, Tools: []harness.Tool{tool}, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	<-tool.started
	t.Cleanup(func() {
		select {
		case <-tool.release:
		default:
			close(tool.release)
		}
	})
	return r, tool
}

func TestContractRuntimeClose(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"close_twice_returns_the_first_result_and_releases_once", rowCloseTwiceReturnsTheFirstResultAndReleasesOnce},
		{"close_returns_at_the_end_of_its_context_while_a_tool_ignores_it", rowCloseReturnsAtTheEndOfItsContextWhileAToolIgnoresIt},
		{"a_later_close_returns_at_the_end_of_its_own_context", rowALaterCloseReturnsAtTheEndOfItsOwnContext},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func rowCloseTwiceReturnsTheFirstResultAndReleasesOnce(t *testing.T) {
	var released atomic.Int32
	r := lifetimeRuntime(t, counted{&released})
	lifetimeSession(t, r)
	for i := range 2 {
		if err := r.Close(t.Context()); err != nil {
			t.Fatalf("Close %d = %v, want nil", i+1, err)
		}
	}
	if n := released.Load(); n != 1 {
		t.Errorf("releases = %d, want 1", n)
	}
}

func rowCloseReturnsAtTheEndOfItsContextWhileAToolIgnoresIt(t *testing.T) {
	r, tool := stuckRuntime(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	closed := make(chan error, 1)
	go func() { closed <- r.Close(ctx) }()
	cancel()
	if err := <-closed; !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want Canceled", err)
	}
	close(tool.release)
}

func rowALaterCloseReturnsAtTheEndOfItsOwnContext(t *testing.T) {
	r, tool := stuckRuntime(t, nil)
	first, cancelFirst := context.WithCancel(t.Context())
	firstDone := make(chan error, 1)
	go func() { firstDone <- r.Close(first) }()
	second, cancelSecond := context.WithCancel(t.Context())
	secondDone := make(chan error, 1)
	go func() { secondDone <- r.Close(second) }()
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second Close = %v, want Canceled", err)
	}
	cancelFirst()
	want := <-firstDone
	if !errors.Is(want, context.Canceled) {
		t.Fatalf("first Close = %v, want Canceled", want)
	}
	close(tool.release)
	if got := r.Close(t.Context()); got == nil || got.Error() != want.Error() {
		t.Errorf("third Close = %v, want the first result %v", got, want)
	}
}
