package e2e

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// acpLoad sends session/load, or session/resume when resume is set, for the
// session as, with the directory cwd ("" is the main one). A non-zero code
// expects that error. noHistory expects no conversation update before the
// answer, and noUpdates expects no update at all.
type acpLoad struct {
	as, cwd   string
	resume    bool
	code      int
	noHistory bool
	noUpdates bool
}

// acpClose sends session/close and waits for its answer. A non-zero code
// expects that error.
type acpClose struct {
	as   string
	code int
}

// acpCloseInFlight sends session/close for a session whose prompt runs, and
// reads until both the prompt and the close are answered.
type acpCloseInFlight struct{ as, prompt string }

// acpList sends session/list and expects the sessions as, in order. A non-zero
// code expects that error instead.
type acpList struct {
	cwd, rawCWD, cursor string
	want                []string
	code                int
}

// acpReplayEquals checks that the updates a load replayed equal the updates the
// turns sent live, and that the replay holds the user message.
type acpReplayEquals struct{}

func (s acpLoad) run(t *testing.T, c *acpConn) {
	t.Helper()
	method, def := "session/load", "LoadSessionResponse"
	if s.resume {
		method, def = "session/resume", "ResumeSessionResponse"
	}
	start := len(c.wire)
	f := c.response(c.request(method, map[string]any{"sessionId": c.session(s.as), "cwd": c.dir(s.cwd), "mcpServers": []any{}}))
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	res := c.mustBeResult(f, def)
	if _, ok := res["configOptions"].([]any); !ok {
		t.Errorf("%s answered no configOptions: %v", method, res)
	}
	for _, fr := range c.wire[start:] {
		u := updateKind(fr)
		if s.noUpdates && u != "" {
			t.Errorf("%s sent the update %s before its answer", method, u)
		}
		if s.noHistory && (u == "user_message_chunk" || u == "agent_message_chunk" || u == "agent_thought_chunk") {
			t.Errorf("%s sent the history update %s", method, u)
		}
	}
}

func (s acpClose) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.response(c.request("session/close", map[string]any{"sessionId": c.session(s.as)}))
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	c.mustBeResult(f, "CloseSessionResponse")
}

func (s acpCloseInFlight) run(t *testing.T, c *acpConn) {
	t.Helper()
	closeID := c.request("session/close", map[string]any{"sessionId": c.session(s.as)})
	promptID := c.pending[s.prompt]
	got := map[int]map[string]any{}
	for len(got) < 2 {
		f := c.read()
		if _, isMessage := f["method"]; isMessage {
			continue
		}
		id, _ := f["id"].(float64)
		got[int(id)] = f
	}
	if res := c.mustBeResult(got[promptID], "PromptResponse"); res["stopReason"] != "cancelled" {
		t.Errorf("the prompt in flight answered %v, want cancelled", res["stopReason"])
	}
	c.mustBeResult(got[closeID], "CloseSessionResponse")
}

func (s acpList) run(t *testing.T, c *acpConn) {
	t.Helper()
	params := map[string]any{}
	if s.cwd != "" {
		params["cwd"] = c.dir(s.cwd)
	}
	if s.rawCWD != "" {
		params["cwd"] = s.rawCWD
	}
	if s.cursor != "" {
		params["cursor"] = s.cursor
	}
	f := c.response(c.request("session/list", params))
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	res := c.mustBeResult(f, "ListSessionsResponse")
	if _, ok := res["nextCursor"]; ok {
		t.Errorf("session/list answered nextCursor %v for a store that holds %d sessions", res["nextCursor"], len(s.want))
	}
	var got, want []string
	list, _ := res["sessions"].([]any)
	for _, e := range list {
		info, _ := e.(map[string]any)
		id, _ := info["sessionId"].(string)
		got = append(got, id)
		if title, ok := info["title"]; !ok || title != nil {
			t.Errorf("session %s has the title %v, want null", id, title)
		}
		if at, _ := info["updatedAt"].(string); at == "" {
			t.Errorf("session %s has no updatedAt", id)
		}
		if cwd, _ := info["cwd"].(string); s.cwd != "" && cwd != c.dir(s.cwd) {
			t.Errorf("session/list with cwd %q answered a session of %q", c.dir(s.cwd), cwd)
		}
	}
	for _, as := range s.want {
		want = append(want, c.session(as))
	}
	if !slices.Equal(got, want) {
		t.Errorf("session/list answered %v, want %v", got, want)
	}
}

// updateKind returns the sessionUpdate of an agent session/update frame, or "".
func updateKind(f acpFrame) string {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				Kind string `json:"sessionUpdate"`
			} `json:"update"`
		} `json:"params"`
	}
	if f.Dir != "agent" || json.Unmarshal(f.Frame, &m) != nil || m.Method != "session/update" {
		return ""
	}
	return m.Params.Update.Kind
}

func (acpReplayEquals) run(t *testing.T, c *acpConn) {
	t.Helper()
	wire := c.goldenWire()
	split := slices.IndexFunc(wire, func(f acpFrame) bool {
		return f.Dir == "client" && strings.Contains(string(f.Frame), `"method":"session/load"`)
	})
	if split < 0 {
		t.Fatal("no session/load on the wire")
	}
	turn := func(frames []acpFrame) (out []string) {
		for _, f := range frames {
			switch updateKind(f) {
			case "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update":
				out = append(out, string(f.Frame))
			}
		}
		return out
	}
	live, replayed := turn(wire[:split]), turn(wire[split:])
	unnumbered := regexp.MustCompile(`"messageId":"<message:\d+>"`)
	for _, list := range [][]string{live, replayed} {
		for i := range list {
			list[i] = unnumbered.ReplaceAllString(list[i], `"messageId":"<message>"`)
		}
	}
	if got, want := agentMessageIDs(c.wire[split:]), agentMessageIDs(c.wire[:split]); !slices.Equal(got, want) {
		t.Errorf("the replayed message IDs are %v, want the live IDs %v", got, want)
	}
	if len(live) == 0 || !strings.Contains(strings.Join(live, ""), `"type":"diff"`) {
		t.Fatalf("the live transcript holds no diff: %v", live)
	}
	if !slices.Equal(live, replayed) {
		t.Errorf("the replayed transcript differs from the live one\nlive:\n%s\nreplayed:\n%s", strings.Join(live, "\n"), strings.Join(replayed, "\n"))
	}
	if !slices.ContainsFunc(wire[split:], func(f acpFrame) bool { return updateKind(f) == "user_message_chunk" }) {
		t.Error("the replay holds no user_message_chunk")
	}
}

// agentMessageIDs returns the messageId of each agent message chunk of the raw
// frames, so an ID change shows in a row that hides the numbers.
func agentMessageIDs(frames []acpFrame) (ids []string) {
	for _, f := range frames {
		if updateKind(f) != "agent_message_chunk" {
			continue
		}
		var m struct {
			Params struct {
				Update struct {
					MessageID string `json:"messageId"`
				} `json:"update"`
			} `json:"params"`
		}
		if json.Unmarshal(f.Frame, &m) == nil && !slices.Contains(ids, m.Params.Update.MessageID) {
			ids = append(ids, m.Params.Update.MessageID)
		}
	}
	return ids
}

func afterLateTurn(t *testing.T, rt *harness.Runtime, id string) {
	t.Helper()
	s, err := rt.Open(t.Context(), id)
	if err != nil {
		t.Errorf("open between replay and open: %v", err)
		return
	}
	turn, err := s.Prompt(t.Context(), protocol.Input{ID: "late-input", Parts: []protocol.Part{{Type: protocol.PartText, Text: "late"}}, Delivery: protocol.DeliveryQueue})
	if err != nil {
		t.Errorf("prompt between replay and open: %v", err)
		return
	}
	if _, err := turn.Wait(t.Context()); err != nil {
		t.Errorf("turn between replay and open: %v", err)
	}
}

var acpLoadScenarios = []acpScenario{
	{
		name:  "load_replays_the_conversation_as_the_turn_sent_it",
		cite:  "The replay sends a user message as `user_message_chunk` updates, and an assistant message, a tool call, and a tool result as the updates that a turn sends, with the same `messageId` and `toolCallId`.",
		model: acpToolCall("edit_file", map[string]any{"path": "a.txt", "old_string": "two", "new_string": "2", "replace_all": true}),
		files: map[string]string{"a.txt": "one\ntwo\nthree\n"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "edit it", stop: "end_turn"},
			acpClose{as: "a"},
			acpLoad{as: "a"},
			acpReplayEquals{},
		},
	},
	{
		name:  "load_of_a_held_session_returns_the_held_session",
		cite:  "A `session/load` or `session/resume` of a session that the connection holds returns the held session.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpLoad{as: "a", noUpdates: true},
			acpLoad{as: "a", resume: true, noUpdates: true},
			acpPrompt{as: "a", text: "two", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) { acpRequests(t, c, 2) }),
		},
	},
	{
		name:  "load_through_another_cwd_fails",
		cite:  "A session whose log names a `work_dir` other than the `cwd` is `-32602`.",
		model: acpRepeatText("ok"),
		dirs:  map[string]map[string]string{"second": nil},
		steps: []acpStep{
			acpNew{as: "a"},
			acpLoad{as: "a", cwd: "second", code: acpInvalidParams},
			acpClose{as: "a"},
			acpLoad{as: "a", cwd: "second", code: acpInvalidParams},
			acpLoad{as: "a", cwd: "second", resume: true, code: acpInvalidParams},
			acpLoad{as: "a"},
		},
	},
	{
		name:  "resume_opens_the_session_without_history",
		cite:  "`session/resume` sends the `available_commands_update` and no conversation.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "first question", stop: "end_turn"},
			acpClose{as: "a"},
			acpLoad{as: "a", resume: true, noHistory: true},
			acpPrompt{as: "a", text: "second question", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				reqs := acpRequests(t, c, 2)
				if got := requestText(reqs[1]); !strings.Contains(got, "first question") {
					t.Errorf("the second request lacks the history of the resumed session: %s", got)
				}
			}),
		},
	},
	{
		name: "load_of_an_unknown_session_is_not_found",
		cite: "A session that the store does not hold is `-32002`.",
		steps: []acpStep{
			acpCall{method: "session/load", params: map[string]any{"sessionId": "nosuchsession", "cwd": "/", "mcpServers": []any{}}, code: acpSessionNotFound},
		},
	},
	{
		name:  "list_returns_the_sessions_of_a_cwd",
		cite:  "With a `cwd`, only the sessions whose `work_dir` equals it are listed.",
		model: acpRepeatText("ok"),
		dirs:  map[string]map[string]string{"second": nil, "empty": nil},
		steps: []acpStep{
			acpNew{as: "a"},
			acpNew{as: "b", cwd: "second"},
			acpList{want: []string{"a", "b"}},
			acpList{cwd: "second", want: []string{"b"}},
			acpList{cwd: "empty"},
			acpList{rawCWD: "relative/dir", code: acpInvalidParams},
			acpList{cursor: "nosuchsession", code: acpInvalidParams},
		},
	},
	{
		name:  "close_cancels_the_prompt_and_releases_the_session",
		cite:  "`session/close` cancels the prompts of the session as `session/cancel` does,",
		model: acpBlockedText("partial"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPromptStart{as: "a", text: "go"},
			acpAwaitText{text: "partial"},
			acpCloseInFlight{as: "a", prompt: "a"},
			acpPrompt{as: "a", text: "again", code: acpSessionNotFound},
			acpClose{as: "a", code: acpSessionNotFound},
		},
	},
	{
		name:  "close_of_an_idle_session_answers_empty",
		cite:  "A `sessionId` that the connection does not hold is `-32002`.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpClose{as: "a"},
			acpClose{as: "a", code: acpSessionNotFound},
		},
	},
	{
		name:      "a_record_appended_between_replay_and_open_arrives_once",
		cite:      "so a record that the log gains between the read and the open reaches the client once, as a live update.",
		model:     []harnesstest.Step{{Name: "first", Reply: harnesstest.Reply{Text: "first reply"}}, {Name: "late", Reply: harnesstest.Reply{Text: "late reply"}}},
		afterRead: afterLateTurn,
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpClose{as: "a"},
			acpLoad{as: "a"},
			acpAwaitText{text: "late reply"},
			acpAwaitUpdate{kind: "session_info_update"},
			acpCheck(func(t *testing.T, c *acpConn) {
				count := map[string]int{}
				for _, f := range c.wire {
					if updateKind(f) == "agent_message_chunk" {
						for _, text := range []string{"first reply", "late reply"} {
							if strings.Contains(string(f.Frame), text) {
								count[text]++
							}
						}
					}
				}
				if count["first reply"] != 2 || count["late reply"] != 1 {
					t.Errorf("chunk counts = %v, want the first reply twice (live, replayed) and the late reply once", count)
				}
			}),
		},
	},
}
