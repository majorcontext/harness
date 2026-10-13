package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// acpSetConfig sends session/set_config_option. It expects the answer to hold
// the complete list with the new value, and a config_option_update before the
// answer. A non-zero code expects that error instead.
type acpSetConfig struct {
	as, id string
	value  any
	code   int
}

func (s acpSetConfig) run(t *testing.T, c *acpConn) {
	t.Helper()
	params := map[string]any{"sessionId": c.session(s.as), "configId": s.id, "value": s.value}
	if _, isBool := s.value.(bool); isBool {
		params["type"] = "boolean"
	}
	id := c.request("session/set_config_option", params)
	updated := false
	for {
		f := c.read()
		if f["method"] == "session/update" {
			params, _ := f["params"].(map[string]any)
			update, _ := params["update"].(map[string]any)
			updated = updated || update["sessionUpdate"] == "config_option_update"
			continue
		}
		if got, ok := f["id"].(float64); !ok || int(got) != id {
			t.Fatalf("want the response to request %d, got %v", id, f)
		}
		if s.code != 0 {
			c.mustBeError(f, s.code)
			return
		}
		res := c.mustBeResult(f, "SetSessionConfigOptionResponse")
		if !updated {
			t.Error("the answer came before a config_option_update")
		}
		if got := acpOptionValue(res["configOptions"], s.id); got != s.value {
			t.Errorf("%s = %v after the change, want %v", s.id, got, s.value)
		}
		if n := len(res["configOptions"].([]any)); n != 2 {
			t.Errorf("the answer lists %d options, want the complete list of 2", n)
		}
		return
	}
}

// expectUpdate reads the next frame and fails unless it is a session/update of
// the kind for the session.
func (c *acpConn) expectUpdate(session, kind string) {
	c.t.Helper()
	f := c.read()
	params, _ := f["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	if f["method"] != "session/update" || params["sessionId"] != session || update["sessionUpdate"] != kind {
		c.t.Fatalf("want a %s update, got %v", kind, f)
	}
}

func acpOptionValue(options any, id string) any {
	list, _ := options.([]any)
	for _, o := range list {
		if m, _ := o.(map[string]any); m["id"] == id {
			return m["currentValue"]
		}
	}
	return nil
}

// acpUpdatesOf returns the updates of the kind that the agent has sent.
func acpUpdatesOf(c *acpConn, kind string) []map[string]any {
	var out []map[string]any
	for _, f := range c.wire {
		var m struct {
			Method string `json:"method"`
			Params struct {
				Update map[string]any `json:"update"`
			} `json:"params"`
		}
		if f.Dir == "agent" && json.Unmarshal(f.Frame, &m) == nil && m.Method == "session/update" && m.Params.Update["sessionUpdate"] == kind {
			out = append(out, m.Params.Update)
		}
	}
	return out
}

// acpNewOptions returns the configOptions of the first session/new answer.
func acpNewOptions(c *acpConn) []any {
	for _, f := range c.wire {
		var m struct {
			Result struct {
				SessionID     string `json:"sessionId"`
				ConfigOptions []any  `json:"configOptions"`
			} `json:"result"`
		}
		if f.Dir == "agent" && json.Unmarshal(f.Frame, &m) == nil && m.Result.SessionID != "" {
			return m.Result.ConfigOptions
		}
	}
	return nil
}

// acpStableFrame replaces what differs between runs and between releases: the
// time of a session_info_update, and the list of models of the model option.
func acpStableFrame(text string) string {
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return text
	}
	acpStable(v)
	b, err := json.Marshal(v)
	if err != nil {
		return text
	}
	return string(b)
}

func acpStable(v any) {
	switch x := v.(type) {
	case map[string]any:
		if x["id"] == "model" && x["options"] != nil {
			x["options"] = []any{map[string]any{"name": "<models>", "value": "<models>"}}
		}
		if _, ok := x["updatedAt"].(string); ok {
			x["updatedAt"] = "<time>"
		}
		for _, e := range x {
			acpStable(e)
		}
	case []any:
		for _, e := range x {
			acpStable(e)
		}
	}
}

// acpWithoutTurnUpdates drops the updates that exist only when a turn ran.
func acpWithoutTurnUpdates(wire []acpFrame) []acpFrame {
	var out []acpFrame
	for _, f := range wire {
		text := string(f.Frame)
		if strings.Contains(text, `"sessionUpdate":"session_info_update"`) || strings.Contains(text, `"sessionUpdate":"usage_update"`) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func acpMessageText(c *acpConn) string {
	var b strings.Builder
	for _, u := range acpUpdatesOf(c, "agent_message_chunk") {
		content, _ := u["content"].(map[string]any)
		text, _ := content["text"].(string)
		b.WriteString(text)
	}
	return b.String()
}

var acpConfigScenarios = []acpScenario{
	{
		name: "session_new_offers_the_model_and_effort_options",
		cite: "`session/new` answers `configOptions` with two `select` options.",
		steps: []acpStep{
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				opts := acpNewOptions(c)
				if len(opts) != 2 {
					t.Fatalf("configOptions = %v, want model and effort", opts)
				}
				model, _ := opts[0].(map[string]any)
				effort, _ := opts[1].(map[string]any)
				if model["id"] != "model" || model["category"] != "model" || model["currentValue"] != "anthropic/claude-fable-5" {
					t.Errorf("model option = %v", model)
				}
				if effort["id"] != "effort" || effort["category"] != "thought_level" || effort["currentValue"] != "default" {
					t.Errorf("effort option = %v", effort)
				}
			}),
		},
	},
	{
		name: "session_new_is_followed_by_the_available_commands",
		cite: "After the answer to `session/new`, the agent sends an `available_commands_update`.",
		steps: []acpStep{
			acpNew{as: "a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				got := acpUpdatesOf(c, "available_commands_update")
				if len(got) != 1 {
					t.Fatalf("available_commands_update count = %d, want 1", len(got))
				}
				names := map[string]bool{}
				for _, cmd := range got[0]["availableCommands"].([]any) {
					m := cmd.(map[string]any)
					names[m["name"].(string)] = true
					if m["description"] == "" {
						t.Errorf("command %v has no description", m["name"])
					}
				}
				for _, want := range []string{"compact", "model", "thinking", "status"} {
					if !names[want] {
						t.Errorf("commands = %v, want %s", names, want)
					}
				}
			}),
		},
	},
	{
		name: "set_config_option_changes_the_model",
		cite: "The update of a `session/set_config_option` reaches the client before the answer.",
		config: map[string]any{"providers": map[string]any{
			"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
			"codex":     map[string]any{"type": "openai", "api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://127.0.0.1:1"},
		}},
		steps: []acpStep{
			acpNew{as: "a"},
			acpSetConfig{as: "a", id: "model", value: "codex/gpt-6-luna"},
			acpCheck(func(t *testing.T, c *acpConn) {
				var codex, native int
				for _, o := range acpNewOptions(c)[0].(map[string]any)["options"].([]any) {
					switch v := o.(map[string]any)["value"].(string); {
					case strings.HasPrefix(v, "codex/"):
						codex++
					case strings.HasPrefix(v, "anthropic/"):
						native++
					}
				}
				if codex != 3 || native == 0 {
					t.Errorf("the model option lists %d codex and %d anthropic values, want the 3 codex models and the anthropic models", codex, native)
				}
			}),
		},
	},
	{
		name:  "set_config_option_changes_the_effort",
		cite:  "`session/set_config_option` takes a `configId` and a string `value`.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpSetConfig{as: "a", id: "effort", value: "high"},
			acpSetConfig{as: "a", id: "effort", value: "default"},
		},
	},
	{
		name: "set_config_option_rejects_an_unknown_option_and_value",
		cite: "A `configId` other than `model` and `effort`, a `value` that the option does not list, and a boolean `value` are `-32602`.",
		steps: []acpStep{
			acpNew{as: "a"},
			acpSetConfig{as: "a", id: "mode", value: "x", code: acpInvalidParams},
			acpSetConfig{as: "a", id: "effort", value: "extreme", code: acpInvalidParams},
			acpSetConfig{as: "a", id: "model", value: "nobody/nothing", code: acpInvalidParams},
			acpSetConfig{as: "a", id: "effort", value: true, code: acpInvalidParams},
		},
	},
	{
		name:  "slash_model_changes_the_model_and_reports_it",
		cite:  "This includes a change by `/model`, by `/thinking`, and by the `model` tool.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "/model anthropic/claude-haiku-4-5", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpOptionValue(acpUpdatesOf(c, "config_option_update")[0]["configOptions"], "model"); got != "anthropic/claude-haiku-4-5" {
					t.Errorf("config_option_update model = %v", got)
				}
				if got := acpMessageText(c); !strings.Contains(got, "/model succeeded") {
					t.Errorf("command status text = %q", got)
				}
			}),
			acpPrompt{as: "a", text: "hi", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpRequests(t, c, 1)[0].Model; got != "claude-haiku-4-5" {
					t.Errorf("the request used the model %q, want claude-haiku-4-5", got)
				}
			}),
		},
	},
	{
		name:   "slash_compact_folds_the_older_turns",
		cite:   "The status text of the command is an `agent_message_chunk`, and the prompt answers `end_turn`.",
		model:  acpRepeatText("ok"),
		config: map[string]any{"compaction_keep_turns": 1},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpPrompt{as: "a", text: "two", stop: "end_turn"},
			acpPrompt{as: "a", text: "/compact", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpMessageText(c); !strings.Contains(got, "/compact succeeded") {
					t.Errorf("command status text = %q", got)
				}
				acpRequests(t, c, 3)
			}),
		},
	},
	{
		name:  "a_line_that_names_no_command_goes_to_the_model",
		cite:  "A line that names no command goes to the model as text.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "/nosuchcommand now", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpRequests(t, c, 1)[0].LastUserText(); !strings.HasPrefix(got, "/nosuchcommand now") {
					t.Errorf("the model saw %q", got)
				}
			}),
		},
	},
	{
		name:  "a_model_call_sends_a_usage_update",
		cite:  "A `usage_update` has `used` and `size` from the context of the session.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "hi", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				got := acpUpdatesOf(c, "usage_update")
				if len(got) != 1 {
					t.Fatalf("usage_update count = %d, want 1", len(got))
				}
				used, _ := got[0]["used"].(float64)
				size, _ := got[0]["size"].(float64)
				if used <= 0 || size < used {
					t.Errorf("usage_update = %v, want 0 < used <= size", got[0])
				}
			}),
		},
	},
	{
		name:  "turn_end_sends_a_session_info_update",
		cite:  "A `session_info_update` with `updatedAt` follows each `turn.ended` event.",
		model: acpRepeatText("ok"),
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "one", stop: "end_turn"},
			acpPrompt{as: "a", text: "two", stop: "end_turn"},
			acpCheck(func(t *testing.T, c *acpConn) {
				got := acpUpdatesOf(c, "session_info_update")
				if len(got) != 2 || got[0]["updatedAt"] == nil || got[0]["title"] != nil {
					t.Errorf("session_info_update = %v, want two with updatedAt and no title", got)
				}
			}),
		},
	},
	{
		name:  "lost_ownership_sends_a_notice_when_the_client_has_the_capability",
		cite:  "When another owner takes a session from the connection and the client has the capability `session.notices`, the agent sends one `notice` update with the severity `warning` for that session.",
		model: acpRepeatText("x"),
		steps: []acpStep{
			acpInit{version: 1, want: 1, notices: true},
			acpNew{as: "a"},
			acpTakeoverStep{as: "a"},
			acpPrompt{as: "a", text: "go", code: acpInternal, dataCode: "session_not_owned"},
			acpCheck(func(t *testing.T, c *acpConn) {
				got := acpUpdatesOf(c, "notice")
				if len(got) != 1 || got[0]["severity"] != "warning" {
					t.Errorf("notices = %v, want one warning", got)
				}
			}),
		},
	},
	{
		name:  "lost_ownership_sends_no_notice_without_the_capability",
		cite:  "Without the capability, the agent sends no notice.",
		model: acpRepeatText("x"),
		steps: []acpStep{
			acpInit{version: 1, want: 1},
			acpNew{as: "a"},
			acpTakeoverStep{as: "a"},
			acpPrompt{as: "a", text: "go", code: acpInternal, dataCode: "session_not_owned"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpUpdatesOf(c, "notice"); len(got) != 0 {
					t.Errorf("notices = %v, want none", got)
				}
			}),
		},
	},
}
