package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

type stopTool struct {
	name    string
	fail    string
	started chan string
	release <-chan struct{}
}

func (s stopTool) Spec() protocol.ToolSpec {
	return protocol.ToolSpec{Name: s.name, Description: "test tool", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (s stopTool) Run(ctx context.Context, _ harness.Call) (protocol.ToolResult, error) {
	if s.started != nil {
		s.started <- s.name
		select {
		case <-s.release:
		case <-ctx.Done():
			return protocol.ToolResult{}, ctx.Err()
		}
	}
	if s.fail == "error" {
		return protocol.ToolResult{}, errors.New("tool failed")
	}
	return protocol.ToolResult{Text: s.name + " result", IsError: s.fail == "result"}, nil
}

func stopRuntime(t *testing.T, provider string, tools []harness.Tool, listed []string, steps ...harnesstest.Step) (*harness.Session, *harnesstest.Server) {
	t.Helper()
	var fake *harnesstest.Server
	var cfg config.Provider
	if provider == "anthropic" {
		fake = harnesstest.NewAnthropic(t, steps...)
		cfg = config.Provider{BaseURL: fake.URL(), APIKeyEnv: "KEY"}
	} else {
		fake = harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, steps...).Server
		cfg = config.Provider{Type: config.TypeOpenAI, BaseURL: fake.URL(), ResponsesPath: "/backend-api/codex/responses", APIKeyEnv: "KEY"}
	}
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Tools: tools, StopAfterTools: listed, Getenv: func(string) string { return "k" }, Config: config.Config{ContextWindowTokens: 100000, Providers: map[string]config.Provider{provider: cfg}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: provider + "/test-model"})
	if err != nil {
		t.Fatal(err)
	}
	return s, fake
}

func stopPrompt(t *testing.T, s *harness.Session, id, text string) *harness.Turn {
	t.Helper()
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: id, Parts: []protocol.Part{{Type: protocol.PartText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func stopWait(t *testing.T, turn *harness.Turn) {
	t.Helper()
	result, err := turn.Wait(t.Context())
	if err != nil || result.StopReason != protocol.StopCompleted {
		t.Fatalf("turn = %+v, %v; want completed", result, err)
	}
}

func stopCall(names ...string) harnesstest.Step {
	calls := make([]harnesstest.ToolCall, len(names))
	for i, name := range names {
		calls[i] = harnesstest.ToolCall{ID: "call_" + name, Name: name, Input: map[string]any{}}
	}
	return harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: calls}}
}

func TestContractRuntimeStopsAfterSuccessfulTool(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			s, fake := stopRuntime(t, provider, []harness.Tool{stopTool{name: "finish"}}, []string{"finish"}, stopCall("finish"))
			stopWait(t, stopPrompt(t, s, "a", "go"))
			if n := len(fake.Requests()); n != 1 {
				t.Errorf("model requests = %d, want 1", n)
			}
		})
	}
}

func TestContractRuntimeContinuesWithoutSuccessfulListedTool(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, row := range []struct {
			name, fail string
			listed     []string
		}{
			{"unset", "", nil}, {"unlisted", "", []string{"other"}},
			{"error_result", "result", []string{"finish"}}, {"run_error", "error", []string{"finish"}},
		} {
			t.Run(provider+"/"+row.name, func(t *testing.T) {
				done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("finish"), Reply: harnesstest.Reply{Text: "continued"}}
				s, fake := stopRuntime(t, provider, []harness.Tool{stopTool{name: "finish", fail: row.fail}}, row.listed, stopCall("finish"), done)
				stopWait(t, stopPrompt(t, s, "a", "go"))
				if n := len(fake.Requests()); n != 2 {
					t.Errorf("model requests = %d, want 2", n)
				}
			})
		}
	}
}

func TestContractRuntimeFinishesBatchBeforeStoppingAndKeepsNextTurnPrefix(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			started, release := make(chan string, 2), make(chan struct{})
			tools := []harness.Tool{stopTool{name: "finish", started: started, release: release}, stopTool{name: "other", started: started, release: release}}
			next := harnesstest.Step{Name: "next", Match: harnesstest.LastUserText("next"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntime(t, provider, tools, []string{"finish"}, stopCall("finish", "other"), next)
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			turn := stopPrompt(t, s, "a", "go")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			seen := map[string]bool{}
			for range 2 {
				select {
				case name := <-started:
					seen[name] = true
				case <-ctx.Done():
					t.Fatal("both calls did not start concurrently")
				}
			}
			if !seen["finish"] || !seen["other"] {
				t.Fatalf("started calls = %v", seen)
			}
			close(release)
			stopWait(t, turn)
			if n := len(fake.Requests()); n != 1 {
				t.Fatalf("first turn requests = %d, want 1", n)
			}
			stopWait(t, stopPrompt(t, s, "b", "next"))
			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("requests = %d, want 2", len(reqs))
			}
			for _, text := range []string{"finish result", "other result"} {
				if !strings.Contains(requestText(reqs[1]), text) {
					t.Errorf("next request lacks %q", text)
				}
			}
			checkPrefixes(t, fake.RawRequests(), nil)
		})
	}
}
