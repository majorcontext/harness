package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var idPattern = regexp.MustCompile(`^(msg|toolu|ses|call|cmd|cmpsum|wt|turn|item|input)_`)

var timePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

var sessionIDPattern = regexp.MustCompile(`ses_[0-9a-z]+`)

var enginePattern = regexp.MustCompile(`engine: harness \S+`)

// ownerPattern matches the owner name that a runtime records: the host name
// and the pid of its process.
var ownerPattern = func() *regexp.Regexp {
	host, _ := os.Hostname()
	return regexp.MustCompile(regexp.QuoteMeta(host) + `/\d+`)
}()

func maskUnstable(s string) string {
	s = ownerPattern.ReplaceAllString(s, "<owner>")
	s = timePattern.ReplaceAllString(s, "<time>")
	s = sessionIDPattern.ReplaceAllString(s, "<session>")
	return enginePattern.ReplaceAllString(s, "engine: harness <version>")
}

const goalEvaluatorMarker = "MET: <one short sentence"

// transcriptMessage and journalEntry are what a driver reports, in the
// oracle's own vocabulary. A driver maps its wire shapes to them, so the
// goldens do not depend on one API.
type transcriptMessage struct {
	ID    string
	Role  string
	Parts []transcriptPart
}

// transcriptPart is Type "text", "tool_call", or "tool_result". Content is the
// joined text of a tool result.
type transcriptPart struct {
	Type, Text, CallID, Name string
	Arguments                any
	IsError                  bool
	Content                  string
}

// journalEntry is one event of the session journal. MessageID is set only on
// an entry that records a message.
type journalEntry struct {
	Seq       int64
	IsMessage bool
	MessageID string
}

type normRequest struct {
	SystemHasGoalEvaluator bool             `json:"system_has_goal_evaluator"`
	Model                  string           `json:"model,omitempty"`
	ThinkingType           string           `json:"thinking_type,omitempty"`
	ThinkingBudgetTokens   int              `json:"thinking_budget_tokens,omitempty"`
	ServiceTier            string           `json:"service_tier,omitempty"`
	ReasoningEffort        string           `json:"reasoning_effort,omitempty"`
	User                   string           `json:"user,omitempty"`
	PromptCacheKey         string           `json:"prompt_cache_key,omitempty"`
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
	Images    []string       `json:"images,omitempty"`
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
	aliases  map[string]string
	counts   map[string]int
	workdirs []string
}

// workdirPaths lists the workdir as given and as the OS resolves it, longest
// first, so a symlinked temp dir masks under either spelling.
func workdirPaths(workdir string) []string {
	if workdir == "" {
		return nil
	}
	if real, err := filepath.EvalSymlinks(workdir); err == nil && real != workdir {
		return []string{real, workdir}
	}
	return []string{workdir}
}

func (n *normalizer) mask(s string) string {
	for _, w := range n.workdirs {
		s = strings.ReplaceAll(s, w, "<workdir>")
	}
	return maskUnstable(s)
}

// scrub masks the workdir and the session ids in every string of a decoded JSON value.
func (n *normalizer) scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = n.scrub(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.scrub(e)
		}
		return out
	case string:
		for _, w := range n.workdirs {
			x = strings.ReplaceAll(x, w, "<workdir>")
		}
		if strings.HasPrefix(x, "ses_") && fullIDPattern.MatchString(x) {
			return n.id(x)
		}
		return x
	}
	return v
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

func normalize(reqs []harnesstest.Request, sessions map[string][]transcriptMessage) observation {
	return normalizeRun(reqs, sessions, nil, nil, "")
}

// normCall is a recorded call result. Ids are aliased and times masked.
type normCall struct {
	Status   int           `json:"status"`
	Body     any           `json:"body,omitempty"`
	Messages []normMessage `json:"messages,omitempty"`
}

var fullIDPattern = regexp.MustCompile(`^((msg|toolu|ses|call|cmd|cmpsum|wt|turn|item)_[0-9A-Za-z_]+|input_[0-9a-z]{26})$`)

// value normalizes decoded JSON. Object keys are visited in sorted order so
// the first-seen id numbering does not depend on map order. A session id that
// has no alias gets a number, so a scenario that lists a child binds it first.
func (n *normalizer) value(key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out[n.key(k)] = n.value(k, x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.value(key, e)
		}
		return out
	case string:
		if key == "workdir" {
			return "<workdir>"
		}
		if key == "code" {
			return x
		}
		return n.str(x)
	case json.Number:
		if key == "pid" {
			return n.pid(x.String())
		}
	}
	return v
}

// pid numbers each process id in order of first sight, so a golden keeps
// whether two reads name the same process.
func (n *normalizer) pid(s string) string {
	if a, ok := n.aliases["pid:"+s]; ok {
		return a
	}
	n.counts["pid"]++
	a := fmt.Sprintf("pid#%d", n.counts["pid"])
	n.aliases["pid:"+s] = a
	return a
}

// key normalizes an object key. A key named *_id is a field name, not an id.
func (n *normalizer) key(k string) string {
	if strings.HasSuffix(k, "_id") {
		return k
	}
	return n.str(k)
}

func (n *normalizer) str(s string) string {
	if fullIDPattern.MatchString(s) {
		return n.id(s)
	}
	return n.mask(s)
}

func scrubInput(n *normalizer, in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	return n.scrub(in).(map[string]any)
}

func (n *normalizer) messages(msgs []transcriptMessage) []normMessage {
	out := make([]normMessage, 0, len(msgs))
	for _, m := range msgs {
		nm := normMessage{ID: n.id(m.ID), Role: m.Role, Parts: []normPart{}}
		for _, p := range m.Parts {
			nm.Parts = append(nm.Parts, normPart{
				Type: p.Type, Text: n.mask(p.Text), CallID: n.id(p.CallID), Name: p.Name,
				Arguments: n.scrub(p.Arguments), IsError: p.IsError, Content: n.mask(p.Content),
			})
		}
		out = append(out, nm)
	}
	return out
}

// normalizeRun is normalize plus the recorded calls. ids maps scenario aliases
// to session ids, which normalize to "ses:<alias>".
func normalizeRun(reqs []harnesstest.Request, sessions map[string][]transcriptMessage, calls []recordedCall, ids map[string]string, workdir string) observation {
	n := &normalizer{aliases: map[string]string{}, counts: map[string]int{}, workdirs: workdirPaths(workdir)}
	for alias, id := range ids {
		n.aliases[id] = "ses:" + alias
	}
	obs := observation{Sessions: map[string][]normMessage{}}
	for _, r := range reqs {
		nr := normRequest{
			SystemHasGoalEvaluator: strings.Contains(r.System, goalEvaluatorMarker),
			Model:                  r.Model,
			ThinkingType:           r.ThinkingType,
			ThinkingBudgetTokens:   r.ThinkingBudget,
			ServiceTier:            r.ServiceTier,
			ReasoningEffort:        r.ReasoningEffort,
			User:                   n.id(r.User),
			PromptCacheKey:         n.id(r.PromptCacheKey),
			Tools:                  slices.Sorted(slices.Values(r.Tools)),
		}
		if len(r.Tools) == 0 {
			nr.Tools = nil
		}
		for _, m := range r.Messages {
			nm := normReqMessage{Role: m.Role}
			for _, p := range m.Parts {
				nm.Parts = append(nm.Parts, normReqPart{
					Kind: p.Kind, Text: n.mask(p.Text), ToolName: p.ToolName, ToolInput: scrubInput(n, p.ToolInput),
					ToolUseID: n.id(p.ToolUseID), IsError: p.IsError, Images: p.Images,
				})
			}
			nr.Messages = append(nr.Messages, nm)
		}
		obs.Requests = append(obs.Requests, nr)
	}
	for _, alias := range slices.Sorted(maps.Keys(sessions)) {
		obs.Sessions[alias] = n.messages(sessions[alias])
	}
	for _, c := range calls {
		if obs.Calls == nil {
			obs.Calls = map[string]normCall{}
		}
		nc := normCall{Status: c.res.Status, Body: n.value("", c.res.Body)}
		if len(c.res.Messages) > 0 {
			nc.Messages = n.messages(c.res.Messages)
		}
		obs.Calls[c.key] = nc
	}
	return obs
}

// groupByConversation orders requests by the first user text, in order of
// first arrival, and keeps arrival order inside each group.
func groupByConversation(reqs []harnesstest.Request) []harnesstest.Request {
	var roots []string
	groups := map[string][]harnesstest.Request{}
	for _, r := range reqs {
		root := conversationRoot(r)
		if _, ok := groups[root]; !ok {
			roots = append(roots, root)
		}
		groups[root] = append(groups[root], r)
	}
	var out []harnesstest.Request
	for _, root := range roots {
		out = append(out, groups[root]...)
	}
	return out
}

func conversationRoot(r harnesstest.Request) string {
	if len(r.Messages) == 0 {
		return ""
	}
	var texts []string
	for _, p := range r.Messages[0].Parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
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

func messageIDViolations(msgs []transcriptMessage) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return uniqueIDViolations("message", ids)
}

func toolPairingViolations(msgs []transcriptMessage) []string {
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
		case n != calls[id]:
			out = append(out, fmt.Sprintf("tool call %s has %d results for %d calls", id, n, calls[id]))
		}
	}
	for _, id := range resultOrder {
		if calls[id] == 0 {
			out = append(out, fmt.Sprintf("tool result %s has no call", id))
		}
	}
	return out
}

func messageViolations(msgs []transcriptMessage) []string {
	return append(messageIDViolations(msgs), toolPairingViolations(msgs)...)
}

func journalViolations(events []journalEntry) []string {
	var out, msgIDs []string
	var prev int64
	for _, ev := range events {
		if ev.Seq != prev+1 {
			out = append(out, fmt.Sprintf("event seq %d follows %d", ev.Seq, prev))
		}
		prev = ev.Seq
		if ev.IsMessage {
			msgIDs = append(msgIDs, ev.MessageID)
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
	t.Parallel()
	toolReq := func(ids ...string) harnesstest.Request {
		var parts []harnesstest.Part
		for _, id := range ids {
			parts = append(parts, harnesstest.Part{Kind: "tool_use", ToolName: "bash", ToolUseID: id})
		}
		return harnesstest.Request{Messages: []harnesstest.Message{{Role: "assistant", Parts: parts}}}
	}
	tests := []struct {
		name string
		reqs []harnesstest.Request
		msgs string
		want string
	}{
		{
			name: "ids_renumbered_by_first_seen",
			reqs: []harnesstest.Request{toolReq("toolu_zzz", "toolu_aaa")},
			msgs: `[{"id":"msg_9","role":"user","parts":[]},{"id":"msg_1","role":"assistant","parts":[]}]`,
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"assistant","parts":[{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#1"},{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#2"}]}]}],` +
				`"sessions":{"a":[{"id":"msg#1","role":"user","parts":[]},{"id":"msg#2","role":"assistant","parts":[]}]}}`,
		},
		{
			name: "same_id_same_alias",
			reqs: []harnesstest.Request{toolReq("toolu_x")},
			msgs: `[{"id":"msg_1","role":"assistant","parts":[{"type":"tool_call","call_id":"toolu_x","name":"bash","arguments":{"b":1,"a":2}}]}]`,
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"assistant","parts":[{"kind":"tool_use","tool_name":"bash","tool_use_id":"toolu#1"}]}]}],` +
				`"sessions":{"a":[{"id":"msg#1","role":"assistant","parts":[{"type":"tool_call","call_id":"toolu#1","name":"bash","arguments":{"a":2,"b":1}}]}]}}`,
		},
		{
			name: "tools_sorted",
			reqs: []harnesstest.Request{{Tools: []string{"write", "bash", "read"}}},
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":["bash","read","write"],"messages":null}],"sessions":{}}`,
		},
		{
			name: "timestamps_removed",
			msgs: `[{"id":"msg_1","role":"user","created_at":"2026-10-02T10:00:00Z","parts":[{"type":"text","text":"hi"}]}]`,
			want: `{"requests":null,"sessions":{"a":[{"id":"msg#1","role":"user","parts":[{"type":"text","text":"hi"}]}]}}`,
		},
		{
			name: "system_reduced_to_flag",
			reqs: []harnesstest.Request{{System: "You are a strict goal-completion evaluator.\nMET: <one short sentence saying why>"}, {System: "secret prompt body"}},
			want: `{"requests":[{"system_has_goal_evaluator":true,"tools":null,"messages":null},{"system_has_goal_evaluator":false,"tools":null,"messages":null}],"sessions":{}}`,
		},
		{
			name: "model_thinking_and_service_tier_kept",
			reqs: []harnesstest.Request{{Model: "claude-fable-5", ThinkingType: "enabled", ThinkingBudget: 16384, ServiceTier: "priority"}},
			want: `{"requests":[{"system_has_goal_evaluator":false,"model":"claude-fable-5","thinking_type":"enabled","thinking_budget_tokens":16384,"service_tier":"priority","tools":null,"messages":null}],"sessions":{}}`,
		},
		{
			name: "timestamps_in_text_masked",
			reqs: []harnesstest.Request{{Messages: []harnesstest.Message{{Role: "user", Parts: []harnesstest.Part{{Kind: "text", Text: "engine started 2026-10-02T15:29:58Z"}}}}}},
			want: `{"requests":[{"system_has_goal_evaluator":false,"tools":null,"messages":[{"role":"user","parts":[{"kind":"text","text":"engine started \u003ctime\u003e"}]}]}],"sessions":{}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessions := map[string][]transcriptMessage{}
			if tc.msgs != "" {
				sessions["a"] = transcriptOf(mustDecode[[]apiMessage](t, tc.msgs))
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

func TestNormalizeCalls(t *testing.T) {
	skipShort(t)
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		msgs    string
		workdir string
		want    string
	}{
		{
			name:    "workdir_masked_in_body_strings_and_messages",
			body:    `{"note":"saved to /w/proj/a.txt"}`,
			msgs:    `[{"id":"msg_1","role":"tool","parts":[{"type":"tool_result","call_id":"c","content":[{"type":"text","text":"/w/proj/a.txt: 1 line"}]}]}]`,
			workdir: "/w",
			want:    `{"status":200,"body":{"note":"saved to \u003cworkdir\u003e/proj/a.txt"},"messages":[{"id":"msg#1","role":"tool","parts":[{"type":"tool_result","call_id":"c","content":"\u003cworkdir\u003e/proj/a.txt: 1 line"}]}]}`,
		},
		{
			name: "known_session_is_its_alias_unknown_is_numbered",
			body: `{"id":"ses_abc","children":["ses_def"],"workdir":"/tmp/x","created_at":"2026-10-02T10:00:00Z"}`,
			want: `{"status":200,"body":{"children":["ses#1"],"created_at":"\u003ctime\u003e","id":"ses:a","workdir":"\u003cworkdir\u003e"}}`,
		},
		{
			name: "id_as_object_key",
			body: `{"ses_abc":{"state":"idle"}}`,
			want: `{"status":200,"body":{"ses:a":{"state":"idle"}}}`,
		},
		{
			name: "id_field_name_key_is_kept",
			body: `{"turn_id":"turn_x","item_id":"item_y"}`,
			want: `{"status":200,"body":{"item_id":"item#1","turn_id":"turn#1"}}`,
		},
		{
			name: "transcript_ids_numbered_with_the_run",
			body: `{"total":1}`,
			msgs: `[{"id":"msg_9","role":"user","parts":[]}]`,
			want: `{"status":200,"body":{"total":1},"messages":[{"id":"msg#1","role":"user","parts":[]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := callResult{Status: 200, Body: mustDecode[any](t, tc.body)}
			if tc.msgs != "" {
				res.Messages = transcriptOf(mustDecode[[]apiMessage](t, tc.msgs))
			}
			obs := normalizeRun(nil, nil, []recordedCall{{key: "k", res: res}}, map[string]string{"a": "ses_abc"}, tc.workdir)
			got, err := json.Marshal(obs.Calls["k"])
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("call =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestInvariants(t *testing.T) {
	skipShort(t)
	t.Parallel()
	call := func(id string) string {
		return `{"id":"m` + id + `","role":"assistant","parts":[{"type":"tool_call","call_id":"` + id + `","name":"bash"}]}`
	}
	callAs := func(mid, id string) string {
		return `{"id":"` + mid + `","role":"assistant","parts":[{"type":"tool_call","call_id":"` + id + `","name":"bash"}]}`
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
		{name: "duplicate_call_id_one_result", msgs: `[` + callAs("a1", "c1") + `,` + callAs("a2", "c1") + `,` + result("r1", "c1") + `]`, want: "tool call c1 has 1 results for 2 calls"},
		{name: "duplicate_call_id_two_results", msgs: `[` + callAs("a1", "c1") + `,` + callAs("a2", "c1") + `,` + result("r1", "c1") + `,` + result("r2", "c1") + `]`},
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
			var msgs []transcriptMessage
			var events []journalEntry
			if tc.msgs != "" {
				msgs = transcriptOf(mustDecode[[]apiMessage](t, tc.msgs))
			}
			if tc.events != "" {
				events = journalOf(mustDecode[[]apiEvent](t, tc.events))
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

func TestGroupByConversation(t *testing.T) {
	skipShort(t)
	t.Parallel()
	req := func(root, last string) harnesstest.Request {
		msgs := []harnesstest.Message{{Role: "user", Parts: []harnesstest.Part{{Kind: "text", Text: root}}}}
		if last != "" {
			msgs = append(msgs, harnesstest.Message{Role: "user", Parts: []harnesstest.Part{{Kind: "text", Text: last}}})
		}
		return harnesstest.Request{Messages: msgs}
	}
	label := func(rs []harnesstest.Request) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.LastUserText())
		}
		return strings.Join(out, ",")
	}
	// The child request arrives between the parent's first and second request,
	// or after its second; both arrival orders must normalize alike.
	arrivals := [][]harnesstest.Request{
		{req("p", ""), req("c", ""), req("p", "ack")},
		{req("p", ""), req("p", "ack"), req("c", "")},
	}
	for _, in := range arrivals {
		if got, want := label(groupByConversation(in)), "p,ack,c"; got != want {
			t.Errorf("groupByConversation(%s) = %s, want %s", label(in), got, want)
		}
	}
}

// apiMessage and apiEvent are the messages and the events of the engine wire,
// which the normalizer tests decode.
type apiMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
	Parts     []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		IsError   bool            `json:"is_error"`
		Content   []apiContent    `json:"content"`
	} `json:"parts"`
}

type apiEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	Message   *struct {
		ID string `json:"id"`
	} `json:"message"`
	GoalCondition string `json:"goal_condition"`
	GoalReason    string `json:"goal_reason"`
	GoalMet       bool   `json:"goal_met"`
	GoalTurn      int    `json:"goal_turn"`
	// Compaction fields (docs/design/context-compaction.md §4); see
	// compaction_test.go.
	CompactFirstID     string `json:"compact_first_id"`
	CompactLastID      string `json:"compact_last_id"`
	CompactTurnsFolded int    `json:"compact_turns_folded"`
	CompactSummaryID   string `json:"compact_summary_id"`
}

// apiContent is one item of a tool result: text, or a blob with its bytes.
type apiContent struct {
	Type, Text, Data string
	MediaType        string `json:"media_type"`
}

func transcriptOf(msgs []apiMessage) []transcriptMessage {
	out := make([]transcriptMessage, len(msgs))
	for i, m := range msgs {
		out[i] = transcriptMessage{ID: m.ID, Role: m.Role}
		for _, p := range m.Parts {
			tp := transcriptPart{Type: p.Type, Text: p.Text, CallID: p.CallID, Name: p.Name, IsError: p.IsError}
			if len(p.Arguments) > 0 {
				_ = json.Unmarshal(p.Arguments, &tp.Arguments)
			}
			var content []string
			for _, c := range p.Content {
				if c.Type == "blob" {
					content = append(content, "[blob "+c.MediaType+" "+c.Data+"]")
					continue
				}
				content = append(content, c.Text)
			}
			tp.Content = strings.Join(content, "\n")
			out[i].Parts = append(out[i].Parts, tp)
		}
	}
	return out
}

func journalOf(events []apiEvent) []journalEntry {
	out := make([]journalEntry, len(events))
	for i, ev := range events {
		out[i] = journalEntry{Seq: ev.Seq}
		if ev.Type == "message" && ev.Message != nil {
			out[i].IsMessage, out[i].MessageID = true, ev.Message.ID
		}
	}
	return out
}
