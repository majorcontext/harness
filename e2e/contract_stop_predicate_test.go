package e2e

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/plugin"
	"github.com/majorcontext/harness/protocol"
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
