package e2e

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

func TestContractRuntimeStopsOnResultPredicate(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, row := range []struct {
			name, tool, text, fail string
			listed                 []string
			wantRequests, wantSeen int
		}{
			{"question", "suggestReplies", `{"question":"Which service?"}`, "", nil, 1, 1},
			{"no_question", "suggestReplies", `{"replies":[]}`, "", nil, 2, 1},
			{"routed", "routeToService", `{"success":true}`, "", nil, 1, 1},
			{"not_routed", "routeToService", `{"success":false}`, "", nil, 2, 1},
			{"invalid_json", "routeToService", `invalid`, "", nil, 2, 1},
			{"error_result", "routeToService", `{"success":true}`, "result", nil, 2, 0},
			{"run_error", "routeToService", "", "error", nil, 2, 0},
			{"listed_overrides_false", "routeToService", `{"success":false}`, "", []string{"routeToService"}, 1, 1},
		} {
			t.Run(provider+"/"+row.name, func(t *testing.T) {
				seen := 0
				predicate := func(c protocol.ToolCall, r protocol.ToolResult) bool {
					seen++
					if c.ID != "call_"+row.tool || c.Name != row.tool || string(c.Arguments) != "{}" || r.Text != row.text {
						t.Errorf("predicate input = %+v, %+v", c, r)
					}
					var out struct {
						Question string `json:"question"`
						Success  bool   `json:"success"`
					}
					if json.Unmarshal([]byte(r.Text), &out) != nil {
						return false
					}
					return c.Name == "suggestReplies" && out.Question != "" || c.Name == "routeToService" && out.Success
				}
				steps := []harnesstest.Step{stopCall(row.tool)}
				if row.wantRequests == 2 {
					steps = append(steps, harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult(row.tool), Reply: harnesstest.Reply{Text: "continued"}})
				}
				s, fake := stopRuntimeOptions(t, provider, harness.Options{
					Tools:          []harness.Tool{stopTool{name: row.tool, text: row.text, fail: row.fail}},
					StopAfterTools: row.listed, StopAfter: predicate,
				}, steps...)
				stopWait(t, stopPrompt(t, s, "a", "go"))
				if n := len(fake.Requests()); n != row.wantRequests || seen != row.wantSeen {
					t.Errorf("requests = %d, predicate calls = %d; want %d, %d", n, seen, row.wantRequests, row.wantSeen)
				}
			})
		}
	}
}

func TestContractRuntimePredicateCanStopOnFailedResult(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, row := range []struct{ fail, text string }{{"result", "failed output"}, {"error", "tool failed"}} {
			t.Run(provider+"/"+row.fail, func(t *testing.T) {
				seen := 0
				s, fake := stopRuntimeOptions(t, provider, harness.Options{
					Tools:           []harness.Tool{stopTool{name: "suggestReplies", fail: row.fail, text: "failed output"}},
					StopAfterErrors: true,
					StopAfter: func(c protocol.ToolCall, r protocol.ToolResult) bool {
						seen++
						if c.Name != "suggestReplies" || !r.IsError || r.Text != row.text {
							t.Errorf("predicate input = %+v, %+v", c, r)
						}
						return true
					},
				}, stopCall("suggestReplies"))
				stopWait(t, stopPrompt(t, s, "a", "go"))
				if n := len(fake.Requests()); n != 1 || seen != 1 {
					t.Errorf("requests = %d, predicate calls = %d; want 1, 1", n, seen)
				}
			})
		}
	}
}

func TestContractRuntimeStopContextTracksTurnAcrossSteps(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, enabled := range []bool{false, true} {
			name := provider + "/errors_disabled"
			if enabled {
				name = provider + "/errors_enabled"
			}
			t.Run(name, func(t *testing.T) {
				first := stopCall("first")
				first.Reply.Text = "first text "
				second := harnesstest.Step{Name: "second", Match: harnesstest.LastToolResult("first"), Reply: harnesstest.Reply{Text: "second text", ToolCalls: []harnesstest.ToolCall{{ID: "call_second", Name: "second", Input: map[string]any{}}}}}
				seen := make(chan harness.StopContext, 2)
				s, fake := stopRuntimeOptions(t, provider, harness.Options{
					Tools:           []harness.Tool{stopTool{name: "first", fail: "result", text: "failed"}, stopTool{name: "second"}},
					StopAfterErrors: enabled,
					StopAfterContext: func(c protocol.ToolCall, r protocol.ToolResult, sc harness.StopContext) bool {
						if c.ID != "call_"+c.Name || r.Text == "" {
							t.Errorf("call and result = %+v, %+v", c, r)
						}
						seen <- sc
						return c.Name == "second"
					},
				}, first, second)
				stream, err := s.PromptStream(t.Context(), protocol.Input{ID: "input", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				result, err := stream.Wait(t.Context())
				if err != nil || result.StopReason != protocol.StopCompleted {
					t.Fatalf("turn = %+v, %v", result, err)
				}
				close(seen)
				var contexts []harness.StopContext
				for sc := range seen {
					contexts = append(contexts, sc)
				}
				want := 1
				if enabled {
					want = 2
				}
				if len(contexts) != want || len(fake.Requests()) != 2 {
					t.Fatalf("contexts = %d, requests = %d; want %d, 2", len(contexts), len(fake.Requests()), want)
				}
				last := contexts[len(contexts)-1]
				if last.SessionID != s.ID() || last.TurnID != result.TurnID || last.TurnID == "" || last.TurnText != "first text second text" {
					t.Errorf("context identity/text = %+v; turn = %+v", last, result)
				}
				if len(last.ToolCalls) != 2 || last.ToolCalls[0].Call.Name != "first" || !last.ToolCalls[0].Result.IsError || last.ToolCalls[0].Result.Text != "failed" || last.ToolCalls[1].Call.Name != "second" || last.ToolCalls[1].Result.Text != "second result" {
					t.Errorf("context calls = %+v", last.ToolCalls)
				}
				if enabled && (contexts[0].TurnText != "first text " || len(contexts[0].ToolCalls) != 1) {
					t.Errorf("first context = %+v", contexts[0])
				}
			})
		}
	}
}

func TestContractRuntimeStopContextIsolatesConcurrentSessions(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			step := stopCall("finish")
			step.Repeat = true
			step.Reply.Text = "session text"
			var fake *harnesstest.Server
			var cfg config.Provider
			if provider == "anthropic" {
				fake = harnesstest.NewAnthropic(t, step)
				cfg = config.Provider{BaseURL: fake.URL(), APIKeyEnv: "KEY"}
			} else {
				fake = harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, step).Server
				cfg = config.Provider{Type: config.TypeOpenAI, BaseURL: fake.URL(), ResponsesPath: "/backend-api/codex/responses", APIKeyEnv: "KEY"}
			}
			started, release := make(chan string, 2), make(chan struct{})
			seen := make(chan harness.StopContext, 2)
			r, err := harness.New(harness.Options{
				Store: store.NewMemStore(), Getenv: func(string) string { return "k" },
				Config:           config.Config{ContextWindowTokens: 100000, Providers: map[string]config.Provider{provider: cfg}},
				Tools:            []harness.Tool{stopTool{name: "finish", started: started, release: release}},
				StopAfterContext: func(_ protocol.ToolCall, _ protocol.ToolResult, sc harness.StopContext) bool { seen <- sc; return true },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			var turns []*harness.Turn
			for _, id := range []string{"one", "two"} {
				s, err := r.Create(t.Context(), protocol.CreateSession{ID: id, Model: provider + "/test-model"})
				if err != nil {
					t.Fatal(err)
				}
				turns = append(turns, stopPrompt(t, s, id, "go"))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			for range 2 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("sessions did not overlap")
				}
			}
			close(release)
			ids := make(map[string]string)
			for i, turn := range turns {
				result, err := turn.Wait(ctx)
				if err != nil || result.StopReason != protocol.StopCompleted {
					t.Fatalf("turn = %+v, %v", result, err)
				}
				ids[[]string{"one", "two"}[i]] = result.TurnID
			}
			for range 2 {
				sc := <-seen
				if sc.TurnID == "" || ids[sc.SessionID] != sc.TurnID || sc.TurnText != "session text" || len(sc.ToolCalls) != 1 || sc.ToolCalls[0].Call.Name != "finish" {
					t.Errorf("session context = %+v", sc)
				}
				delete(ids, sc.SessionID)
			}
			if len(ids) != 0 {
				t.Errorf("missing sessions: %v", ids)
			}
		})
	}
}

func TestContractRuntimeStopContextSurvivesReleaseAndReopen(t *testing.T) {
	skipShort(t)
	for _, row := range []struct {
		name, want string
		tools      []harness.Tool
	}{
		{"first", "failed output", []harness.Tool{stopTool{name: "prior"}, stopTool{name: "first", fail: "result", text: "failed output"}, stopTool{name: "finish"}}},
		{"ask_user_question", "no such tool available: ask_user_question", []harness.Tool{stopTool{name: "prior"}, stopTool{name: "finish"}}},
	} {
		t.Run(row.name, func(t *testing.T) { testStopContextSurvivesReleaseAndReopen(t, row.name, row.want, row.tools) })
	}
}

func testStopContextSurvivesReleaseAndReopen(t *testing.T, name, want string, tools []harness.Tool) {
	t.Helper()
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			prior := harnesstest.Step{Name: "prior", Match: harnesstest.LastUserText("prior"), Reply: harnesstest.Reply{Text: "unrelated text", ToolCalls: []harnesstest.ToolCall{{ID: "call_prior", Name: "prior", Input: map[string]any{}}}}}
			done := harnesstest.Step{Name: "prior done", Match: harnesstest.LastToolResult("prior"), Reply: harnesstest.Reply{Text: "unrelated ending"}}
			first := stopCall(name)
			first.Reply.Text = "before handoff "
			resume := harnesstest.Step{Name: "resume", Match: harnesstest.LastToolResult(name), Repeat: true, Reply: harnesstest.Reply{Block: true, Text: "after handoff", ToolCalls: []harnesstest.ToolCall{{ID: "call_finish", Name: "finish", Input: map[string]any{}}}}}
			var fake *harnesstest.Server
			var cfg config.Provider
			if provider == "anthropic" {
				fake = harnesstest.NewAnthropic(t, prior, done, first, resume)
				cfg = config.Provider{BaseURL: fake.URL(), APIKeyEnv: "KEY"}
			} else {
				fake = harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{}, prior, done, first, resume).Server
				cfg = config.Provider{Type: config.TypeOpenAI, BaseURL: fake.URL(), ResponsesPath: "/backend-api/codex/responses", APIKeyEnv: "KEY"}
			}
			seen := make(chan harness.StopContext, 1)
			r, err := harness.New(harness.Options{
				Store: store.NewMemStore(), Getenv: func(string) string { return "k" },
				Config: config.Config{ContextWindowTokens: 100000, Providers: map[string]config.Provider{provider: cfg}},
				Tools:  tools,
				StopAfterContext: func(c protocol.ToolCall, _ protocol.ToolResult, sc harness.StopContext) bool {
					if c.Name == "finish" {
						seen <- sc
						return true
					}
					return false
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close(context.Background()) })
			s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: provider + "/test-model"})
			if err != nil {
				t.Fatal(err)
			}
			stopWait(t, stopPrompt(t, s, "prior", "prior"))
			turn := stopPrompt(t, s, "input", "go")
			if !fake.AwaitRequests(4, 10*time.Second) {
				t.Fatal("model did not reach the handoff boundary")
			}
			if err := s.Release(t.Context()); err != nil {
				t.Fatal(err)
			}
			fake.Release("resume")
			reopened, err := r.Open(t.Context(), "s1")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			result, err := reopened.Turn(turn.Admitted()).Wait(ctx)
			if err != nil || result.StopReason != protocol.StopCompleted {
				t.Fatalf("resumed turn = %+v, %v", result, err)
			}
			select {
			case sc := <-seen:
				if sc.SessionID != "s1" || sc.TurnID != result.TurnID || sc.TurnID == "" || sc.TurnText != "before handoff after handoff" {
					t.Errorf("resumed context = %+v; turn = %+v", sc, result)
				}
				if len(sc.ToolCalls) != 2 || sc.ToolCalls[0].Call.ID != "call_"+name || !sc.ToolCalls[0].Result.IsError || sc.ToolCalls[0].Result.Text != want || sc.ToolCalls[1].Call.ID != "call_finish" || sc.ToolCalls[1].Result.Text != "finish result" {
					t.Errorf("resumed calls = %+v", sc.ToolCalls)
				}
			case <-ctx.Done():
				t.Fatal("resumed predicate did not run")
			}
		})
	}
}

func TestContractRuntimeFailedNamedToolDoesNotStop(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		for _, failure := range []string{"result", "error"} {
			t.Run(provider+"/"+failure, func(t *testing.T) {
				seen := 0
				done := harnesstest.Step{Name: "done", Match: harnesstest.LastToolResult("finish"), Reply: harnesstest.Reply{Text: "continued"}}
				s, fake := stopRuntimeOptions(t, provider, harness.Options{
					Tools: []harness.Tool{stopTool{name: "finish", fail: failure}}, StopAfterTools: []string{"finish"}, StopAfterErrors: true,
					StopAfterContext: func(_ protocol.ToolCall, r protocol.ToolResult, sc harness.StopContext) bool {
						seen++
						if !r.IsError || len(sc.ToolCalls) != 1 || !sc.ToolCalls[0].Result.IsError {
							t.Errorf("failed result = %+v; context = %+v", r, sc)
						}
						return false
					},
				}, stopCall("finish"), done)
				stopWait(t, stopPrompt(t, s, "input", "go"))
				if len(fake.Requests()) != 2 || seen != 1 {
					t.Errorf("requests = %d, callbacks = %d; want 2, 1", len(fake.Requests()), seen)
				}
			})
		}
	}
}

func TestContractRuntimePredicateSeesHookedResultBeforeRetention(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			full := strings.Repeat("line\n", 6000) + "question: Which service?"
			seen := 0
			p := &plugin.Plugin{Name: "result", ToolExecuteAfter: func(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteAfterRequest) (*plugin.ToolExecuteAfterResponse, error) {
				if req.Tool != "suggestReplies" {
					t.Errorf("hook tool = %q", req.Tool)
				}
				return &plugin.ToolExecuteAfterResponse{Output: []plugin.Part{plugin.TextPart(full)}}, nil
			}}
			next := harnesstest.Step{Name: "next", Match: harnesstest.LastUserText("next"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntimeOptions(t, provider, harness.Options{
				WorkDir: t.TempDir(), Tools: []harness.Tool{stopTool{name: "suggestReplies"}}, Plugins: []*plugin.Plugin{p},
				StopAfter: func(_ protocol.ToolCall, r protocol.ToolResult) bool {
					seen++
					if r.Text != full {
						t.Errorf("predicate saw %d bytes, want %d", len(r.Text), len(full))
					}
					return strings.HasSuffix(r.Text, "question: Which service?")
				},
			}, stopCall("suggestReplies"), next)
			stopWait(t, stopPrompt(t, s, "a", "go"))
			if len(fake.Requests()) != 1 || seen != 1 {
				t.Fatalf("requests = %d; predicate calls = %d", len(fake.Requests()), seen)
			}
			stopWait(t, stopPrompt(t, s, "b", "next"))
			text := requestText(fake.Requests()[1])
			if !strings.Contains(text, "trh_1") || strings.Contains(text, "question: Which service?") {
				t.Fatal("next turn did not keep the retained preview")
			}
		})
	}
}

func TestContractRuntimeCancellationOverridesPredicateStop(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			seen, started := make(chan struct{}), make(chan string, 1)
			s, fake := stopRuntimeOptions(t, provider, harness.Options{
				Tools: []harness.Tool{stopTool{name: "finish", readOnly: true}, stopTool{name: "other", started: started, release: make(chan struct{})}},
				StopAfter: func(c protocol.ToolCall, _ protocol.ToolResult) bool {
					if c.Name == "finish" {
						close(seen)
						return true
					}
					t.Error("error result invoked predicate")
					return false
				},
			}, stopCall("finish", "other"))
			turn := stopPrompt(t, s, "a", "go")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			select {
			case <-seen:
			case <-ctx.Done():
				t.Fatal("predicate did not run")
			}
			if err := turn.Cancel(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := turn.Wait(ctx)
			if err != nil || result.StopReason != protocol.StopInterrupted || len(fake.Requests()) != 1 {
				t.Fatalf("turn = %+v, %v; model requests = %d", result, err, len(fake.Requests()))
			}
		})
	}
}

func TestContractRuntimePredicateObservesEveryCallInOrder(t *testing.T) {
	skipShort(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			var seen []string
			started, release := make(chan string, 1), make(chan struct{})
			next := harnesstest.Step{Name: "next", Match: harnesstest.LastUserText("next"), Reply: harnesstest.Reply{Text: "done"}}
			s, fake := stopRuntimeOptions(t, provider, harness.Options{
				Tools: []harness.Tool{stopTool{name: "finish", readOnly: true}, stopTool{name: "other", started: started, release: release}},
				StopAfter: func(c protocol.ToolCall, _ protocol.ToolResult) bool {
					seen = append(seen, c.Name)
					if c.Name == "finish" {
						close(release)
						return true
					}
					return false
				},
			}, stopCall("finish", "other"), next)
			stopWait(t, stopPrompt(t, s, "a", "go"))
			if !slices.Equal(seen, []string{"finish", "other"}) || len(fake.Requests()) != 1 {
				t.Fatalf("predicate order = %v; model requests = %d", seen, len(fake.Requests()))
			}
			stopWait(t, stopPrompt(t, s, "b", "next"))
			for _, text := range []string{"finish result", "other result"} {
				if !strings.Contains(requestText(fake.Requests()[1]), text) {
					t.Errorf("next request lacks %q", text)
				}
			}
			checkPrefixes(t, fake.RawRequests(), nil)
		})
	}
}
