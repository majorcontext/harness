package e2e

import (
	"net/http"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func TestContractSessionAgent(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_task_child_reports_its_agent_type_and_a_root_session_has_none", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "delegate", Match: harnesstest.LastUserText("delegate"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{
					ID: "toolu_1", Name: "task", Input: map[string]any{"agent": "explore", "prompt": "child work"},
				}}}},
				harnesstest.Step{Name: "child", Match: harnesstest.LastUserText("child work"), Reply: harnesstest.Reply{Text: "child done", Block: true}},
				harnesstest.Step{Name: "ack", Match: harnesstest.LastToolResult("task"), Reply: harnesstest.Reply{Text: "waiting"}},
				harnesstest.Step{Name: "parent", Match: harnesstest.LastUserText("child done"), Reply: harnesstest.Reply{Text: "parent done"}},
			)
			root := d.Create(t)
			d.Submit(t, root, "delegate")
			// The child holds its reply until the parent's ack request exists, so the
			// report cannot ride on that request.
			if !fake.AwaitRequests(3, waitBound) {
				t.Fatal("the parent did not send its ack request")
			}
			fake.Release("child")
			d.WaitIdle(t, root)
			kid := d.Child(t, root, 0)
			d.WaitIdle(t, kid)

			if got := d.view(t, kid).Agent; got != "explore" {
				t.Errorf("GET /sessions/{id} of the child: agent = %q, want explore", got)
			}
			if got := d.view(t, root).Agent; got != "" {
				t.Errorf("GET /sessions/{id} of the root: agent = %q, want none", got)
			}
			var page protocol.SessionPage
			d.expect(t, http.StatusOK, http.MethodGet, "/sessions", nil, &page)
			agents := map[string]string{}
			for _, s := range page.Sessions {
				agents[s.ID] = s.Agent
			}
			if len(agents) != 2 || agents[kid] != "explore" || agents[root] != "" {
				t.Errorf("GET /sessions agents = %v, want the child explore and the root none", agents)
			}
		})
	})
}
