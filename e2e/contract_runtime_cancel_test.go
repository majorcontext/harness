package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

type heldEmbedderTool struct {
	started  chan context.Context
	release  chan struct{}
	returned chan struct{}
}

func (*heldEmbedderTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: "probe", Description: "waits for release", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (h *heldEmbedderTool) Run(ctx context.Context, call harness.Call) (protocol.ToolResult, error) {
	if call.ID == "call_done" {
		return protocol.ToolResult{Text: "completed tool result"}, nil
	}
	if call.ID == "call_done_error" {
		return protocol.ToolResult{}, errors.New("accepted callback error")
	}
	h.started <- ctx
	<-h.release
	defer close(h.returned)
	return protocol.ToolResult{Text: "late tool result"}, nil
}

func heldToolRuntime(t *testing.T, owner harness.Owner) (*harness.Runtime, *store.MemStore, *heldEmbedderTool, func()) {
	t.Helper()
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{Text: "stream prefix", ToolCalls: []harnesstest.ToolCall{{ID: "call_done", Name: "probe", Input: map[string]any{}}, {ID: "call_done_error", Name: "probe", Input: map[string]any{}}, {ID: "call_1", Name: "probe", Input: map[string]any{}}}}})
	tool := &heldEmbedderTool{make(chan context.Context, 1), make(chan struct{}), make(chan struct{})}
	st := store.NewMemStore()
	r, err := harness.New(harness.Options{Store: st, Owner: owner, Tools: []harness.Tool{tool}, Getenv: os.Getenv, Config: config.Config{ContextWindowTokens: 100000, Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(tool.release) }) }
	t.Cleanup(func() { release(); _ = r.Close(context.Background()) })
	return r, st, tool, release
}

func TestContractRuntimeToolCancellation(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	for _, mode := range []string{"cancel", "stream_context", "end", "end_with_stream", "end_with_slow_subscriber", "end_with_connection"} {
		t.Run(mode, func(t *testing.T) { rowIgnoredToolCancellation(t, mode) })
	}
}

func rowIgnoredToolCancellation(t *testing.T, mode string) {
	r, st, tool, release := heldToolRuntime(t, nil)
	s, checkConnection := cancellationSession(t, r, mode)
	turn, stream, cancelPrompt := cancellationPrompt(t, s, mode)
	defer cancelPrompt()
	ending := mode != "cancel" && mode != "stream_context"
	var err error
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	var toolCtx context.Context
	select {
	case toolCtx = <-tool.started:
	case <-ctx.Done():
		t.Fatal("tool did not start")
	}
	awaitKeptToolResult(t, ctx, s)
	if mode == "end_with_slow_subscriber" {
		defer slowEvents(t, ctx, s)()
	}
	if ending {
		if _, err := s.Prompt(ctx, protocol.Input{ID: "queued", Delivery: protocol.DeliveryQueue, Parts: []protocol.Part{{Type: protocol.PartText, Text: "never run"}}}); err != nil {
			t.Fatal(err)
		}
		err = r.End(ctx, s.ID())
	} else if mode == "stream_context" {
		cancelPrompt()
		_, err = turn.Wait(ctx)
	} else {
		err = turn.Cancel(ctx)
	}
	if err != nil {
		t.Fatalf("%s waited for an ignored-context tool: %v", mode, err)
	}
	select {
	case <-toolCtx.Done():
	default:
		t.Fatal("tool context was not canceled")
	}
	if ending {
		checkCause(t, s, harness.ErrSessionEnded)
		checkConnection()
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := protocol.CauseStopped
	if ending {
		want = protocol.CauseEnded
	}
	if result.Cause != want {
		t.Fatalf("cause = %s, want %s", result.Cause, want)
	}
	before, err := st.Head(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	release()
	<-tool.returned
	after, err := st.Head(ctx, s.ID())
	if err != nil || after != before {
		t.Fatalf("late result changed log: %d -> %d, %v", before, after, err)
	}
	checkSyntheticCancellation(t, st, s.ID())
	if stream != nil && ending {
		checkEndedStream(t, ctx, stream)
	}
	if ending {
		checkEndReopen(t, r, s)
	}
}

func cancellationPrompt(t *testing.T, s *harness.Session, mode string) (*harness.Turn, *harness.Stream, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	in := protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}
	if mode == "stream_context" || mode == "end_with_stream" {
		stream, err := s.PromptStream(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stream.Close)
		return stream.Turn, stream, cancel
	}
	turn, err := s.Prompt(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return turn, nil, cancel
}

func checkEndedStream(t *testing.T, ctx context.Context, stream *harness.Stream) {
	t.Helper()
	var completed, ended bool
	var text strings.Builder
	for e, err := range stream.Events(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == protocol.KindItemDelta {
			var frame protocol.ItemFrame
			if err := json.Unmarshal(e.Data, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Type == "text" {
				text.WriteString(frame.Text)
			}
		}
		if e.Kind == protocol.KindItemCompleted && strings.Contains(string(e.Data), "completed tool result") {
			completed = true
		}
		if e.Kind == protocol.KindTurnEnded {
			ended = true
		}
	}
	if !completed || !ended || text.String() != "stream prefix" {
		t.Fatal("End discarded queued stream deltas, a result, or its terminal event")
	}
}

func cancellationSession(t *testing.T, r *harness.Runtime, mode string) (*harness.Session, func()) {
	t.Helper()
	if mode != "end_with_connection" {
		return lifetimeSession(t, r), func() {}
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	servers := map[string]config.MCPServerSpec{"weather": {
		Command: []string{"sh", "-c", `echo $$ > "$1"; exec "$2"`, "sh", pidFile, mcpStubBin(t)},
		Env:     mcpWeather("").StdioEnv(),
	}}
	s := sessionMCPCreate(t, r, servers)
	return s, func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("session server still runs after busy End: %v", err)
		}
	}
}

func checkEndReopen(t *testing.T, r *harness.Runtime, s *harness.Session) {
	t.Helper()
	reopened, err := r.Open(t.Context(), s.ID())
	if err != nil || reopened == s {
		t.Fatalf("reopen = %p, %v", reopened, err)
	}
	queued, err := reopened.Turn(protocol.Admitted{InputID: "queued", Seq: 1}).Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !queued.Withdrawn {
		t.Fatal("End did not withdraw queued input")
	}
}

func slowEvents(t *testing.T, ctx context.Context, s *harness.Session) func() {
	t.Helper()
	started, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(unblock)
	go func() {
		first := true
		for _, err := range s.Events(ctx, 0) {
			if first {
				close(started)
				<-resume
				first = false
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("subscriber did not start")
	}
	return func() {
		unblock()
		select {
		case err := <-done:
			if !errors.Is(err, harness.ErrSessionEnded) {
				t.Errorf("subscriber ended with %v", err)
			}
		case <-ctx.Done():
			t.Error("subscriber did not end")
		}
	}
}

func awaitKeptToolResult(t *testing.T, ctx context.Context, s *harness.Session) {
	t.Helper()
	for e, err := range s.Events(ctx, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == protocol.KindItemCompleted && strings.Contains(string(e.Data), "completed tool result") {
			return
		}
	}
	t.Fatal("completed tool result was not recorded")
}

func checkSyntheticCancellation(t *testing.T, st harness.Store, id string) {
	t.Helper()
	kept, acceptedError, canceled := false, false, false
	for e, err := range harness.ReadEvents(t.Context(), st, id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(e.Data), "late tool result") {
			t.Fatal("cancellation recorded a late tool result")
		}
		if e.Kind != protocol.KindItemCompleted {
			continue
		}
		var item protocol.ItemCompleted
		if err := json.Unmarshal(e.Data, &item); err != nil {
			t.Fatal(err)
		}
		for _, part := range item.Message.Parts {
			if part.Type != protocol.LogPartToolResult {
				continue
			}
			switch part.CallID {
			case "call_done":
				kept = !part.IsError && part.Text == "completed tool result"
			case "call_done_error":
				acceptedError = part.IsError && part.Text == "accepted callback error"
			case "call_1":
				canceled = kept && acceptedError && part.IsError && part.Text == context.Canceled.Error()
			}
		}
	}
	if !kept || !acceptedError || !canceled {
		t.Fatalf("tool results: kept=%t, accepted error=%t, synthetic cancellation=%t", kept, acceptedError, canceled)
	}
}

type heldCanceledTransport struct {
	started, canceled, release chan struct{}
}

func (h heldCanceledTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	close(h.started)
	<-req.Context().Done()
	close(h.canceled)
	<-h.release
	return nil, context.Cause(req.Context())
}

func TestContractRuntimeEndRefusesNewControlCommands(t *testing.T) {
	skipShort(t)
	for _, mode := range []string{"end", "release_then_end"} {
		t.Run(mode, func(t *testing.T) { rowEndRefusesNewControlCommands(t, mode == "release_then_end") })
	}
}

func heldTransportRuntime(t *testing.T) (*harness.Runtime, heldCanceledTransport, func()) {
	t.Helper()
	transport := heldCanceledTransport{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: func(string) string { return "k" },
		ModelTransport: func(string) http.RoundTripper { return transport },
		Config: config.Config{GoalEvaluatorModel: "bifrost/gpt-test", ContextWindowTokens: 100000,
			Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1", APIKeyEnv: "KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(transport.release) }) }
	t.Cleanup(func() { release(); _ = r.Close(context.Background()) })
	return r, transport, release
}

func rowEndRefusesNewControlCommands(t *testing.T, releaseFirst bool) {
	r, transport, release := heldTransportRuntime(t)
	s := lifetimeSession(t, r)
	turn := stopPrompt(t, s, "a", "go")
	queued, err := s.Prompt(t.Context(), protocol.Input{ID: "queued", Delivery: protocol.DeliveryQueue, Parts: []protocol.Part{{Type: protocol.PartText, Text: "never run"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	select {
	case <-transport.started:
	case <-ctx.Done():
		t.Fatal("model call did not start")
	}
	released := make(chan error, 1)
	if releaseFirst {
		go func() { released <- s.Release(ctx) }()
		select {
		case <-transport.canceled:
		case <-ctx.Done():
			t.Fatal("Release did not request handoff")
		}
	}
	ended := make(chan error, 1)
	go func() { ended <- r.End(ctx, s.ID()) }()
	withdrawn, err := queued.Wait(ctx)
	if err != nil || !withdrawn.Withdrawn {
		t.Fatalf("End did not withdraw the queue: %+v, %v", withdrawn, err)
	}
	if err := s.SetGoal(ctx, protocol.Goal{Condition: "never restart"}); !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("SetGoal after End = %v, want not owned", err)
	}
	title := "never update"
	if _, err := s.Update(ctx, protocol.SettingsPatch{Title: &title}); !errors.Is(err, harness.ErrSessionNotOwned) {
		t.Errorf("Update after End = %v, want not owned", err)
	}
	release()
	select {
	case err := <-ended:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("End did not finish after the transport returned")
	}
	result, err := turn.Wait(ctx)
	if err != nil || result.Cause != protocol.CauseEnded {
		t.Errorf("End left the turn resumable: %+v, %v", result, err)
	}
	cause := harness.ErrSessionEnded
	if releaseFirst {
		cause = harness.ErrSessionReleased
		if err := <-released; err != nil {
			t.Fatal(err)
		}
	}
	checkCause(t, s, cause)
}

type panickingEmbedderTool struct{ stopTool }

func (panickingEmbedderTool) Run(context.Context, harness.Call) (protocol.ToolResult, error) {
	panic("private panic detail")
}

func TestContractRuntimeEmbedderPanicBecomesAnErrorResult(t *testing.T) {
	skipShort(t)
	done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("probe"), Reply: harnesstest.Reply{Text: "continued"}}
	s, fake := stopRuntime(t, "openai", []harness.Tool{panickingEmbedderTool{stopTool{name: "probe"}}}, nil, stopCall("probe"), done)
	stopWait(t, stopPrompt(t, s, "a", "go"))
	reqs := fake.Requests()
	if len(reqs) != 2 || !strings.Contains(requestText(reqs[1]), "the tool panicked") || strings.Contains(requestText(reqs[1]), "private panic detail") {
		t.Fatal("tool panic did not become a safe error result")
	}
}

type heldReleaseOwner struct {
	harness.Owner
	started chan struct{}
	release chan struct{}
}

func (o heldReleaseOwner) Acquire(ctx context.Context, id string) (harness.Ownership, error) {
	own, err := o.Owner.Acquire(ctx, id)
	return heldReleaseGrant{own, o.started, o.release}, err
}

type heldReleaseGrant struct {
	harness.Ownership
	started chan struct{}
	release chan struct{}
}

func (o heldReleaseGrant) Release(ctx context.Context) error {
	o.started <- struct{}{}
	select {
	case <-o.release:
		return o.Ownership.Release(ctx)
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func TestContractRuntimeEndOutlivesItsCaller(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	for _, mode := range []string{"during_release", "already_canceled", "zero_deadline"} {
		t.Run(mode, func(t *testing.T) { rowEndOutlivesCaller(t, mode) })
	}
}

func rowEndOutlivesCaller(t *testing.T, mode string) {
	owner := heldReleaseOwner{harness.NewLocalOwner(1), make(chan struct{}, 1), make(chan struct{})}
	r, _, tool, _ := heldToolRuntime(t, owner)
	t.Cleanup(func() { close(owner.release) })
	s := lifetimeSession(t, r)
	if _, err := s.Prompt(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal("tool did not start")
	}
	endCtx, endCancel := context.WithCancel(ctx)
	defer endCancel()
	if mode == "already_canceled" {
		endCancel()
	}
	if mode == "zero_deadline" {
		var cut context.CancelFunc
		endCtx, cut = context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cut()
	}
	ended := make(chan error, 1)
	go func() { ended <- r.End(endCtx, s.ID()) }()
	select {
	case <-owner.started:
	case <-ctx.Done():
		t.Fatal("End did not cancel the tool and begin cleanup")
	}
	endCancel()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Cause(endCtx)) {
			t.Fatalf("End = %v, want %v", err, context.Cause(endCtx))
		}
	case <-ctx.Done():
		t.Fatal("End did not return when its context ended")
	}
	owner.release <- struct{}{}
	checkCause(t, s, harness.ErrSessionEnded)
	if err := endOfEvents(t, s); !errors.Is(err, harness.ErrSessionEnded) {
		t.Fatalf("Events = %v, want ended", err)
	}
}
