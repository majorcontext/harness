package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/fakemodel"
)

var idPattern = regexp.MustCompile(`^(msg|toolu|ses|call)_`)

var timePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

var sessionIDPattern = regexp.MustCompile(`ses_[0-9a-z]+`)

var enginePattern = regexp.MustCompile(`engine: harness \S+`)

func maskUnstable(s string) string {
	s = timePattern.ReplaceAllString(s, "<time>")
	s = sessionIDPattern.ReplaceAllString(s, "<session>")
	return enginePattern.ReplaceAllString(s, "engine: harness <version>")
}

const goalEvaluatorMarker = "MET: <one short sentence"

type normRequest struct {
	SystemHasGoalEvaluator bool             `json:"system_has_goal_evaluator"`
	Tools                  []string         `json:"tools"`
	Messages               []normReqMessage `json:"messages"`
}

type normReqMessage struct {
	Role  string        `json:"role"`
	Parts []normReqPart `json:"parts"`
}

type normReqPart struct {
	Kind      string         `json:"kind"`
	Text      string         `json:"text,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

type normMessage struct {
	ID    string     `json:"id"`
	Role  string     `json:"role"`
	Parts []normPart `json:"parts"`
}

type normPart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments any    `json:"arguments,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Content   string `json:"content,omitempty"`
}

type normalizer struct {
	aliases map[string]string
	counts  map[string]int
}

func (n *normalizer) id(s string) string {
	loc := idPattern.FindStringIndex(s)
	if loc == nil {
		return s
	}
	if a, ok := n.aliases[s]; ok {
		return a
	}
	kind := s[:loc[1]-1]
	n.counts[kind]++
	a := fmt.Sprintf("%s#%d", kind, n.counts[kind])
	n.aliases[s] = a
	return a
}

func normalize(reqs []fakemodel.Request, sessions map[string][]apiMessage) observation {
	n := &normalizer{aliases: map[string]string{}, counts: map[string]int{}}
	obs := observation{Sessions: map[string][]normMessage{}}
	for _, r := range reqs {
		nr := normRequest{
			SystemHasGoalEvaluator: strings.Contains(r.System, goalEvaluatorMarker),
			Tools:                  slices.Sorted(slices.Values(r.Tools)),
		}
		if len(r.Tools) == 0 {
			nr.Tools = nil
		}
		for _, m := range r.Messages {
			nm := normReqMessage{Role: m.Role}
			for _, p := range m.Parts {
				nm.Parts = append(nm.Parts, normReqPart{
					Kind: p.Kind, Text: maskUnstable(p.Text), ToolName: p.ToolName, ToolInput: p.ToolInput,
					ToolUseID: n.id(p.ToolUseID), IsError: p.IsError,
				})
			}
			nr.Messages = append(nr.Messages, nm)
		}
		obs.Requests = append(obs.Requests, nr)
	}
	for _, alias := range slices.Sorted(maps.Keys(sessions)) {
		msgs := make([]normMessage, 0, len(sessions[alias]))
		for _, m := range sessions[alias] {
			nm := normMessage{ID: n.id(m.ID), Role: m.Role, Parts: []normPart{}}
			for _, p := range m.Parts {
				np := normPart{Type: p.Type, Text: maskUnstable(p.Text), CallID: n.id(p.CallID), Name: p.Name, IsError: p.IsError}
				if len(p.Arguments) > 0 {
					_ = json.Unmarshal(p.Arguments, &np.Arguments)
				}
				var content []string
				for _, c := range p.Content {
					content = append(content, c.Text)
				}
				np.Content = maskUnstable(strings.Join(content, "\n"))
				nm.Parts = append(nm.Parts, np)
			}
			msgs = append(msgs, nm)
		}
		obs.Sessions[alias] = msgs
	}
	return obs
}

func uniqueIDViolations(kind string, ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		switch {
		case id == "":
			out = append(out, kind+" with empty id")
		case seen[id]:
			out = append(out, fmt.Sprintf("duplicate %s id %s", kind, id))
		}
		seen[id] = true
	}
	return out
}

func messageIDViolations(msgs []apiMessage) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return uniqueIDViolations("message", ids)
}

func toolPairingViolations(msgs []apiMessage) []string {
	var out, callOrder, resultOrder []string
	calls, results := map[string]int{}, map[string]int{}
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch p.Type {
			case "tool_call":
				if calls[p.CallID]++; calls[p.CallID] == 1 {
					callOrder = append(callOrder, p.CallID)
				}
			case "tool_result":
				if results[p.CallID]++; results[p.CallID] == 1 {
					resultOrder = append(resultOrder, p.CallID)
				}
			}
		}
	}
	for _, id := range callOrder {
		switch n := results[id]; {
		case n == 0:
			out = append(out, fmt.Sprintf("tool call %s has no result", id))
		case n > 1:
			out = append(out, fmt.Sprintf("tool call %s has %d results", id, n))
		}
	}
	for _, id := range resultOrder {
		if calls[id] == 0 {
			out = append(out, fmt.Sprintf("tool result %s has no call", id))
		}
	}
	return out
}

func messageViolations(msgs []apiMessage) []string {
	return append(messageIDViolations(msgs), toolPairingViolations(msgs)...)
}

func journalViolations(events []apiEvent) []string {
	var out, msgIDs []string
	var prev int64
	for _, ev := range events {
		if ev.Seq != prev+1 {
			out = append(out, fmt.Sprintf("event seq %d follows %d", ev.Seq, prev))
		}
		prev = ev.Seq
		if ev.Type == "message" && ev.Message != nil {
			msgIDs = append(msgIDs, ev.Message.ID)
		}
	}
	return append(out, uniqueIDViolations("journal message", msgIDs)...)
}

func mustDecode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode %T: %v", v, err)
	}
	return v
}

func TestNormalize(t *testing.T) {
	skipShort(t)
	toolReq := func(ids ...string) fakemodel.Request {
		var parts []fakemodel.Part
		for _, id := range ids {
			parts = append(parts, fakemodel.Part{Kind: "tool_use", ToolName: "bash", ToolUseID: id})
		}
		return fakemodel.Request{Messages: []fakemodel.Message{{Role: "assistant", Parts: parts}}}
	}
	tests := []struct {
		name string
		reqs []fakemodel.Request
		msgs string
		want string
	}{
		{
			name: "ids_renumbered_by_first_seen",
			reqs: []fakemodel.Request{toolReq("toolu_zzz", "toolu_aaa")},
			msgs: `[{"id":"msg_9","role":"user","parts":[]},{"id":"msg_1","role":"assistant","parts":[]}]`,
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"assistant","parts":[{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#1"},{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#2"}]}]}],` +
				`"sessions":{"a":[{"id":"msg#1","role":"user","parts":[]},{"id":"msg#2","role":"assistant","parts":[]}]}}`,
		},
		{
			name: "same_id_same_alias",
			reqs: []fakemodel.Request{toolReq("toolu_x")},
			msgs: `[{"id":"msg_1","role":"assistant","parts":[{"type":"tool_call","call_id":"toolu_x","name":"bash","arguments":{"b":1,"a":2}}]}]`,
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"assistant","parts":[{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#1"}]}]}],` +
				`"sessions":{"a":[{"id":"msg#1","role":"assistant","parts":[{"type":"tool_call","call_id":"toolu#1","name":"bash","arguments":{"a":2,"b":1}}]}]}}`,
		},
		{
			name: "tools_sorted",
			reqs: []fakemodel.Request{{Tools: []string{"write", "bash", "read"}}},
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":["bash","read","write"],"messages":null}],"sessions":{}}`,
		},
		{
			name: "timestamps_removed",
			msgs: `[{"id":"msg_1","role":"user","created_at":"2026-10-02T10:00:00Z","parts":[{"type":"text","text":"hi"}]}]`,
			want: `{"requests":null,"sessions":{"a":[{"id":"msg#1","role":"user","parts":[{"type":"text","text":"hi"}]}]}}`,
		},
		{
			name: "system_reduced_to_flag",
			reqs: []fakemodel.Request{{System: "You are a strict goal-completion evaluator.\nMET: <one short sentence saying why>"}, {System: "secret prompt body"}},
			want: `{"requests":[{"system_has_goal_evaluator":true,"tools":null,"messages":null},{"system_has_goal_evaluator":false,"tools":null,"messages":null}],"sessions":{}}`,
		},
		{
			name: "timestamps_in_text_masked",
			reqs: []fakemodel.Request{{Messages: []fakemodel.Message{{Role: "user", Parts: []fakemodel.Part{{Kind: "text", Text: "engine started 2026-10-02T15:29:58Z"}}}}}},
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"user","parts":[{"kind":"text","text":"engine started \u003ctime\u003e"}]}]}],"sessions":{}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := map[string][]apiMessage{}
			if tc.msgs != "" {
				sessions["a"] = mustDecode[[]apiMessage](t, tc.msgs)
			}
			got, err := json.Marshal(normalize(tc.reqs, sessions))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("normalize =\n%s\nwant\n%s", got, tc.want)
			}
			if strings.Contains(string(got), "2026-10-02") || strings.Contains(string(got), "secret prompt") {
				t.Errorf("normalized output leaks unstable or system data: %s", got)
			}
		})
	}
}

func TestInvariants(t *testing.T) {
	skipShort(t)
	call := func(id string) string {
		return `{"id":"m` + id + `","role":"assistant","parts":[{"type":"tool_call","call_id":"` + id + `","name":"bash"}]}`
	}
	result := func(mid, id string) string {
		return `{"id":"` + mid + `","role":"tool","parts":[{"type":"tool_result","call_id":"` + id + `"}]}`
	}
	tests := []struct {
		name   string
		msgs   string
		events string
		want   string // substring of the single violation; empty means none
	}{
		{name: "tool_call_without_result", msgs: `[` + call("c1") + `]`, want: "tool call c1 has no result"},
		{name: "tool_result_without_call", msgs: `[` + result("r1", "c1") + `]`, want: "tool result c1 has no call"},
		{name: "doubled_tool_result", msgs: `[` + call("c1") + `,` + result("r1", "c1") + `,` + result("r2", "c1") + `]`, want: "tool call c1 has 2 results"},
		{name: "duplicate_message_id", msgs: `[{"id":"msg_1","role":"user","parts":[]},{"id":"msg_1","role":"assistant","parts":[]}]`, want: "duplicate message id msg_1"},
		{name: "empty_message_id", msgs: `[{"id":"","role":"user","parts":[]}]`, want: "message with empty id"},
		{name: "paired_calls", msgs: `[` + call("c1") + `,` + result("r1", "c1") + `]`, events: `[{"seq":1},{"seq":2}]`},
		{name: "seq_gap", events: `[{"seq":1},{"seq":3}]`, want: "seq 3 follows 1"},
		{
			name:   "duplicate_journal_message_id",
			events: `[{"seq":1,"type":"message","message":{"id":"msg_1"}},{"seq":2,"type":"message","message":{"id":"msg_1"}}]`,
			want:   "duplicate journal message id msg_1",
		},
		{name: "empty_journal_message_id", events: `[{"seq":1,"type":"message","message":{"id":""}}]`, want: "journal message with empty id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var msgs []apiMessage
			var events []apiEvent
			if tc.msgs != "" {
				msgs = mustDecode[[]apiMessage](t, tc.msgs)
			}
			if tc.events != "" {
				events = mustDecode[[]apiEvent](t, tc.events)
			}
			got := append(messageViolations(msgs), journalViolations(events)...)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("violations = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("violations = %v, want one containing %q", got, tc.want)
			}
		})
	}
}
