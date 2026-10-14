package e2e

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/internal/testpoll"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// acpAwaitElicitation reads frames until the agent sends elicitation/create, keeps its request id under as, and passes its params to check.
type acpAwaitElicitation struct {
	as    string
	check func(t *testing.T, params map[string]any)
}

// acpAnswerElicitation answers the elicitation kept under as. A nil content sends none.
type acpAnswerElicitation struct {
	as, action string
	content    map[string]any
}

// acpAwaitCancelRequest reads frames until the agent sends $/cancel_request for the elicitation kept under as.
type acpAwaitCancelRequest struct{ as string }

// acpAwaitResolution waits until the log of the session holds a request.resolved record with the resolution. The agent sends no frame for it, and the agent can be another process.
type acpAwaitResolution struct {
	as         string
	resolution protocol.ResolutionKind
}

// acpCloseSession sends session/close and waits for its answer.
type acpCloseSession struct{ as string }

// awaitMessage returns the first frame from the agent that match accepts, after the frames that earlier await steps took.
func (c *acpConn) awaitMessage(match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	for i := c.scanned; i < len(c.wire); i++ {
		var m map[string]any
		if c.wire[i].Dir == "agent" && json.Unmarshal(c.wire[i].Frame, &m) == nil && match(m) {
			c.scanned = i + 1
			return m
		}
	}
	for {
		f := c.read()
		c.scanned = len(c.wire)
		if match(f) {
			return f
		}
	}
}

func (s acpAwaitElicitation) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.awaitMessage(func(m map[string]any) bool { return m["method"] == "elicitation/create" && m["id"] != nil })
	if c.asked == nil {
		c.asked = map[string]any{}
	}
	c.asked[s.as] = f["id"]
	params, _ := f["params"].(map[string]any)
	if s.check != nil {
		s.check(t, params)
	}
}

func (s acpAnswerElicitation) run(t *testing.T, c *acpConn) {
	t.Helper()
	result := map[string]any{"action": s.action}
	if s.content != nil {
		result["content"] = s.content
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.asked[s.as], "result": result})
}

func (s acpAwaitCancelRequest) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.awaitMessage(func(m map[string]any) bool { return m["method"] == "$/cancel_request" })
	if params, _ := f["params"].(map[string]any); !reflect.DeepEqual(params["requestId"], c.asked[s.as]) {
		t.Errorf("$/cancel_request names %v, want the elicitation %v", params["requestId"], c.asked[s.as])
	}
}

func (s acpAwaitResolution) run(t *testing.T, c *acpConn) {
	t.Helper()
	st, id := store.NewDiskStore(c.env.sessDir), c.session(s.as)
	testpoll.Until(t, waitBound, "the log holds no "+string(s.resolution)+" request", func() bool {
		for e, err := range harness.ReadEvents(t.Context(), st, id, 0) {
			if err != nil {
				return false
			}
			if p, _ := e.Payload(); e.Kind == protocol.KindRequestResolved && p.(*protocol.RequestResolved).Resolution == s.resolution {
				return true
			}
		}
		return false
	})
}

func (s acpCloseSession) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.mustBeResult(c.response(c.request("session/close", map[string]any{"sessionId": c.session(s.as)})), "CloseSessionResponse")
}

// acpElicitations returns the frames of the agent that are elicitation/create requests.
func acpElicitations(c *acpConn) []map[string]any {
	var out []map[string]any
	for _, f := range c.wire {
		var m map[string]any
		if f.Dir == "agent" && json.Unmarshal(f.Frame, &m) == nil && m["method"] == "elicitation/create" {
			out = append(out, m)
		}
	}
	return out
}

var acpQuestionLogin = &acpLoginSpec{claude: true, claudeQuestion: true}

var acpElicitationScenarios = []acpScenario{
	{
		name:  "a_question_is_elicited_after_the_prompt_answer_and_the_answer_resumes_the_session",
		cite:  "When the turn that opened it ends `awaiting_input` and the client has the capability, the agent sends the request `elicitation/create` for it.",
		login: acpQuestionLogin,
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "pick a db", stop: "end_turn"},
			acpAwaitElicitation{as: "q", check: func(t *testing.T, params map[string]any) {
				schema, _ := params["requestedSchema"].(map[string]any)
				want := map[string]any{
					"type": "object", "required": []any{"question_1"},
					"properties": map[string]any{"question_1": map[string]any{
						"type": "string", "title": "DB", "description": "Which database?",
						"oneOf": []any{
							map[string]any{"const": "PostgreSQL", "title": "PostgreSQL - a"},
							map[string]any{"const": "SQLite", "title": "SQLite - b"},
						},
					}},
				}
				if !reflect.DeepEqual(schema, want) {
					t.Errorf("requestedSchema = %v, want %v", schema, want)
				}
				if params["mode"] != "form" || params["message"] != "Which database?" || params["toolCallId"] != "toolu_q" {
					t.Errorf("params = %v, want mode form, the question as message, and toolCallId toolu_q", params)
				}
			}},
			acpCheck(func(t *testing.T, c *acpConn) {
				response, elicitation := -1, -1
				for i, f := range c.wire {
					var m map[string]any
					if f.Dir != "agent" || json.Unmarshal(f.Frame, &m) != nil {
						continue
					}
					if m["result"] != nil && m["id"] == float64(3) {
						response = i
					}
					if m["method"] == "elicitation/create" {
						elicitation = i
					}
				}
				if response < 0 || elicitation < response {
					t.Errorf("the prompt response is at frame %d and the elicitation at %d, want the response first", response, elicitation)
				}
			}),
			acpAnswerElicitation{as: "q", action: "accept", content: map[string]any{"question_1": "SQLite"}},
			acpAwaitText{text: "Noted."},
			acpAwaitUpdate{kind: "session_info_update"},
		},
	},
	{
		name:  "a_declined_elicitation_dismisses_the_question",
		cite:  "An `accept` with no value for any question, the `decline` action, and the `cancel` action dismiss the request.",
		login: acpQuestionLogin,
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "pick a db", stop: "end_turn"},
			acpAwaitElicitation{as: "q"},
			acpAnswerElicitation{as: "q", action: "decline"},
			acpAwaitResolution{as: "a", resolution: protocol.ResolutionDismissed},
		},
	},
	{
		name:  "a_client_without_the_capability_gets_no_elicitation",
		cite:  "Without the capability, the agent does not set it, no question opens, and the agent sends no `elicitation/create`.",
		login: acpQuestionLogin,
		steps: []acpStep{
			acpInit{version: 1, want: 1, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "pick a db", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpElicitations(c); len(got) != 0 {
					t.Errorf("elicitations = %v, want none", got)
				}
			}),
		},
	},
	{
		name:  "an_input_while_the_question_is_open_cancels_the_elicitation",
		cite:  "The agent then sends `$/cancel_request` for the `elicitation/create` that has no answer yet, and it drops a late answer.",
		login: acpQuestionLogin,
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "pick a db", stop: "end_turn"},
			acpAwaitElicitation{as: "q"},
			acpPrompt{as: "a", text: "never mind", stop: "end_turn"},
			acpAwaitCancelRequest{as: "q"},
			acpAnswerElicitation{as: "q", action: "accept", content: map[string]any{"question_1": "SQLite"}},
		},
	},
	{
		name:  "closing_the_session_cancels_the_elicitation",
		cite:  "`session/close` sends `$/cancel_request` for each `elicitation/create` of the session that has no answer, after the answers of its prompts.",
		login: acpQuestionLogin,
		steps: []acpStep{
			acpInit{version: 1, want: 1, elicitation: true, methods: []string{"claude-code"}},
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "pick a db", stop: "end_turn"},
			acpAwaitElicitation{as: "q"},
			acpCloseSession{as: "a"},
			acpAwaitCancelRequest{as: "q"},
		},
	},
}
