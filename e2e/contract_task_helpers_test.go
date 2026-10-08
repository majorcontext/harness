package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// userStarts matches a request whose last user text starts with text.
func userStarts(text string) harnesstest.Matcher {
	return func(r harnesstest.Request) bool { return strings.HasPrefix(r.LastUserText(), text) }
}

// rootStarts matches a request of the conversation whose first message
// starts with text.
func rootStarts(text string) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		return len(r.Messages) > 0 && len(r.Messages[0].Parts) > 0 && strings.HasPrefix(r.Messages[0].Parts[0].Text, text)
	}
}

// lastResultHas matches a request whose last message holds a tool result with
// substr in its text.
func lastResultHas(substr string) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		if len(r.Messages) == 0 {
			return false
		}
		for _, p := range r.Messages[len(r.Messages)-1].Parts {
			if p.Kind == "tool_result" && strings.Contains(p.Text, substr) {
				return true
			}
		}
		return false
	}
}

var (
	childIDPattern    = regexp.MustCompile(`ses_[0-9a-z]+`)
	parentIDPattern   = regexp.MustCompile(`"parent_id":"(ses_[0-9a-z]+)"`)
	childrenIDPattern = regexp.MustCompile(`"children":\["(ses_[0-9a-z]+)"`)
)

// resultID returns the first match of re in the tool results of r, its first
// group when it has one, or "" when none holds one.
func resultID(r harnesstest.Request, re *regexp.Regexp) string {
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			if p.Kind != "tool_result" {
				continue
			}
			if sub := re.FindStringSubmatch(p.Text); sub != nil {
				return sub[len(sub)-1]
			}
		}
	}
	return ""
}

// kidOf returns the ID of the first child that a task result of r names.
func kidOf(r harnesstest.Request) string { return resultID(r, childIDPattern) }

func spawn(agent, prompt string) map[string]any { return ftArgs("agent", agent, "prompt", prompt) }

func onSession(action, id string, kv ...any) map[string]any {
	return ftArgs(append([]any{"action", action, "session_id", id}, kv...)...)
}

// taskStep replies with one task call for each argument map that build returns for the request.
func taskStep(name string, when harnesstest.Matcher, build func(r harnesstest.Request) []map[string]any) harnesstest.Step {
	return harnesstest.Step{Name: name, Match: when, Reply: harnesstest.Reply{Calls: taskCalls(build)}}
}

func taskCalls(build func(r harnesstest.Request) []map[string]any) func(harnesstest.Request) []harnesstest.ToolCall {
	return func(r harnesstest.Request) []harnesstest.ToolCall {
		var calls []harnesstest.ToolCall
		for i, args := range build(r) {
			calls = append(calls, harnesstest.ToolCall{ID: fmt.Sprintf("toolu_task%d", i+1), Name: "task", Input: args})
		}
		return calls
	}
}

// busyTaskStep is taskStep with a bash call after the task calls, which holds the
// turn open while a child that the task calls stop reports to its parent.
func busyTaskStep(name string, when harnesstest.Matcher, build func(r harnesstest.Request) []map[string]any) harnesstest.Step {
	calls := taskCalls(build)
	return harnesstest.Step{Name: name, Match: when, Reply: harnesstest.Reply{Calls: func(r harnesstest.Request) []harnesstest.ToolCall {
		return append(calls(r), harnesstest.ToolCall{ID: "toolu_sleep", Name: "bash", Input: ftArgs("command", "sleep 0.5")})
	}}}
}

func fixed(args ...map[string]any) func(harnesstest.Request) []map[string]any {
	return func(harnesstest.Request) []map[string]any { return args }
}

// onKid builds the calls of a turn from the ID of the child.
func onKid(build func(kid string) []map[string]any) func(harnesstest.Request) []map[string]any {
	return func(r harnesstest.Request) []map[string]any { return build(kidOf(r)) }
}

// delegation is the model script of a parent that spawns one child of agent
// and answers with child. A child reply with Block holds until the scenario
// releases the step child, so its report never rides on the request that
// follows the spawn; a reply without Block lets the child settle on its own.
func delegation(agent string, child harnesstest.Reply, more ...harnesstest.Step) []harnesstest.Step {
	steps := []harnesstest.Step{
		taskStep("delegate", userStarts("delegate"), fixed(spawn(agent, "child work"))),
		{Name: "child", Match: userStarts("child work"), Reply: child},
		{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
	}
	steps = append(steps, more...)
	return append(steps, harnesstest.Step{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true})
}

// childDone is the reply of a child that settles when the scenario releases it.
var childDone = harnesstest.Reply{Text: "child done", Block: true}

// childRuns is the reply of a child that keeps running.
var childRuns = harnesstest.Reply{Text: "partial", Block: true}

// spawned runs the delegation until the child has reported to its idle parent.
var spawned = spawnedAfter(4)

// spawnedAfter is spawned for a child that makes requests of its own: the
// scenario waits for n model requests in all after it releases the child.
func spawnedAfter(n int) []action {
	return []action{
		create{as: "a"},
		submit{as: "a", text: "delegate"},
		awaitRequests{n: 3},
		release{step: "child"},
		awaitRequests{n: n},
		waitIdle{as: "a"},
		bindChild{as: "kid", parent: "a", record: true},
	}
}

// spawnedRunning runs the delegation until the parent is idle and the child runs.
var spawnedRunning = []action{
	create{as: "a"},
	submit{as: "a", text: "delegate"},
	awaitRequests{n: 3},
	waitIdle{as: "a"},
	bindChild{as: "kid", parent: "a", record: true, staysActive: true},
}

// recordSystemLine records the line of the system prompt that holds contains,
// for each request whose last user text starts with user.
type recordSystemLine struct{ user, contains string }

func (a recordSystemLine) run(t *testing.T, r *run) {
	lines := []any{}
	for _, req := range r.fake.Requests() {
		if !strings.HasPrefix(req.LastUserText(), a.user) {
			continue
		}
		line := ""
		for _, l := range strings.Split(req.System, "\n") {
			if strings.Contains(l, a.contains) {
				line = l
			}
		}
		lines = append(lines, line)
	}
	r.record(t, "system_line", a.user, callResult{Status: 200, Body: lines})
}

// limited sets a task limit to n: serve reads it from the environment variable
// env, and the runtime from the config key.
func limited(key, env string, n int) func(*testing.T, host, string) driver {
	return func(t *testing.T, h host, modelURL string) driver {
		return h.open(t, writeGoalConfigWith(t, modelURL, scenarioConfig(map[string]any{key: n})), map[string]string{env: strconv.Itoa(n)})
	}
}
