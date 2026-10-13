package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	acpSessionNotFound = -32002
	acpInternal        = -32603
	acpRequestCanceled = -32800
)

// acpNew sends session/new for the named directory ("" is the main one) and
// keeps the session ID as. A non-zero code expects that error instead.
type acpNew struct {
	as, cwd string
	// rawCWD replaces the directory. weather adds the stdio weather server.
	rawCWD  string
	mcp     []map[string]any
	weather bool
	code    int
}

// acpPromptStart sends a prompt and keeps its request under id, which is as
// when empty. acpResult then waits for the response.
type acpPromptStart struct {
	as, text, id string
	// sid replaces the session ID. blocks replaces the text block.
	sid    string
	blocks []any
}

// acpResult waits for the response of a started prompt. A non-empty stop
// expects that stop reason. A non-zero code expects that error, and class the
// error_class of its data.
type acpResult struct {
	id, stop, class, dataCode string
	code                      int
}

// acpPrompt is acpPromptStart and acpResult in one step.
type acpPrompt struct {
	as, text, stop, class, dataCode string
	code                            int
}

// acpAwaitText reads frames until the agent has sent a message chunk with text.
type acpAwaitText struct{ text string }

// acpAwaitUpdate reads frames until the agent has sent a session/update of the
// kind, with the status when it is not empty.
type acpAwaitUpdate struct{ kind, status string }

// acpCancel sends the session/cancel notification.
type acpCancel struct{ as string }

// acpCancelRequest sends $/cancel_request for a started prompt.
type acpCancelRequest struct{ id string }

// acpTakeoverStep opens the session in a runtime of the test, which takes it from the agent.
type acpTakeoverStep struct{ as string }

// acpCloseInput closes the input of the agent and keeps reading its output.
type acpCloseInput struct{}

// acpCheck runs a function on the connection, for what the wire does not show.
type acpCheck func(t *testing.T, c *acpConn)

func (s acpNew) run(t *testing.T, c *acpConn) {
	t.Helper()
	servers := []any{}
	for _, m := range s.mcp {
		servers = append(servers, m)
	}
	if s.weather {
		env := []any{}
		for _, kv := range mcpWeather("").StdioEnv() {
			k, v, _ := strings.Cut(kv, "=")
			env = append(env, map[string]any{"name": k, "value": v})
		}
		c.paths[mcpStubBin(t)] = "<mcp-stub>"
		servers = append(servers, map[string]any{"name": "weather", "command": mcpStubBin(t), "args": []any{}, "env": env})
	}
	cwd := s.rawCWD
	if cwd == "" {
		cwd = c.dir(s.cwd)
	}
	f := c.response(c.request("session/new", map[string]any{"cwd": cwd, "mcpServers": servers}))
	if s.code != 0 {
		c.mustBeError(f, s.code)
		return
	}
	res := c.mustBeResult(f, "NewSessionResponse")
	id, _ := res["sessionId"].(string)
	if id == "" {
		t.Fatalf("session/new answered no session ID: %v", res)
	}
	c.sessions[s.as] = id
}

func (s acpPromptStart) run(t *testing.T, c *acpConn) {
	t.Helper()
	if s.id == "" {
		s.id = s.as
	}
	sid := s.sid
	if sid == "" {
		sid = c.session(s.as)
	}
	blocks := s.blocks
	if blocks == nil {
		blocks = []any{map[string]any{"type": "text", "text": s.text}}
	}
	c.pending[s.id] = c.request("session/prompt", map[string]any{"sessionId": sid, "prompt": blocks})
}

func (s acpResult) run(t *testing.T, c *acpConn) {
	t.Helper()
	f := c.response(c.pending[s.id])
	if s.code != 0 {
		c.mustBeError(f, s.code)
		e, _ := f["error"].(map[string]any)
		data, _ := e["data"].(map[string]any)
		if s.class != "" && data["error_class"] != s.class {
			t.Errorf("error data = %v, want error_class %q", data, s.class)
		}
		if s.dataCode != "" && data["code"] != s.dataCode {
			t.Errorf("error data = %v, want code %q", data, s.dataCode)
		}
		return
	}
	res := c.mustBeResult(f, "PromptResponse")
	if res["stopReason"] != s.stop {
		t.Errorf("stopReason = %v, want %s", res["stopReason"], s.stop)
	}
}

func (s acpPrompt) run(t *testing.T, c *acpConn) {
	t.Helper()
	acpPromptStart{as: s.as, text: s.text}.run(t, c)
	acpResult{id: s.as, stop: s.stop, class: s.class, dataCode: s.dataCode, code: s.code}.run(t, c)
}

func (s acpAwaitText) run(t *testing.T, c *acpConn) {
	t.Helper()
	var got strings.Builder
	for !strings.Contains(got.String(), s.text) {
		f := c.read()
		if f["method"] != "session/update" {
			t.Fatalf("waiting for text %q, got %v", s.text, f)
		}
		params, _ := f["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		content, _ := update["content"].(map[string]any)
		if update["sessionUpdate"] == "agent_message_chunk" {
			text, _ := content["text"].(string)
			got.WriteString(text)
		}
	}
}

func (s acpAwaitUpdate) run(t *testing.T, c *acpConn) {
	t.Helper()
	for {
		f := c.read()
		if f["method"] != "session/update" {
			t.Fatalf("waiting for a %s update, got %v", s.kind, f)
		}
		params, _ := f["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if update["sessionUpdate"] == s.kind && (s.status == "" || update["status"] == s.status) {
			return
		}
	}
}

func (s acpCancel) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": c.session(s.as)}})
}

func (s acpCancelRequest) run(t *testing.T, c *acpConn) {
	t.Helper()
	c.send(map[string]any{"jsonrpc": "2.0", "method": "$/cancel_request", "params": map[string]any{"requestId": float64(c.pending[s.id])}})
}

func (s acpTakeoverStep) run(t *testing.T, c *acpConn) {
	t.Helper()
	acpTakeover(t, c.env, c.dir(""), c.session(s.as))
}

func (acpCloseInput) run(t *testing.T, c *acpConn) {
	t.Helper()
	if closer, ok := c.in.(io.Closer); ok {
		_ = closer.Close()
	}
}

func (s acpCheck) run(t *testing.T, c *acpConn) {
	t.Helper()
	s(t, c)
}

func (c *acpConn) session(as string) string {
	c.t.Helper()
	id, ok := c.sessions[as]
	if !ok {
		c.t.Fatalf("no session %q", as)
	}
	return id
}

func (c *acpConn) dir(name string) string {
	c.t.Helper()
	if name == "" {
		name = "main"
	}
	d, ok := c.dirs[name]
	if !ok {
		c.t.Fatalf("no directory %q", name)
	}
	return d
}

var acpMessageID = regexp.MustCompile(`"messageId":"([^"]+)"`)

// goldenWire returns the frames with the session IDs, the directories, and the
// message IDs replaced by placeholders, and with the consecutive chunks of one
// message joined: how the model stream splits into chunks is not the contract.
func (c *acpConn) goldenWire() []acpFrame {
	type swap struct{ from, to string }
	var swaps []swap
	for as, id := range c.sessions {
		swaps = append(swaps, swap{id, "<session:" + as + ">"})
	}
	for name, dir := range c.dirs {
		swaps = append(swaps, swap{dir, "<dir:" + name + ">"})
	}
	for path, to := range c.paths {
		swaps = append(swaps, swap{path, to})
	}
	sort.Slice(swaps, func(i, j int) bool { return len(swaps[i].from) > len(swaps[j].from) })
	ids := map[string]string{}
	var out []acpFrame
	for _, f := range c.wire {
		text := string(f.Frame)
		for _, s := range swaps {
			text = strings.ReplaceAll(text, s.from, s.to)
		}
		text = acpMessageID.ReplaceAllStringFunc(text, func(m string) string {
			id := acpMessageID.FindStringSubmatch(m)[1]
			if _, ok := ids[id]; !ok {
				ids[id] = fmt.Sprintf("<message:%d>", len(ids)+1)
			}
			return `"messageId":"` + ids[id] + `"`
		})
		f.Frame = json.RawMessage(text)
		if n := len(out); n > 0 && joinACPChunk(&out[n-1], f) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// joinACPChunk appends the text of next to prev when both are agent chunks of
// one message, and reports whether it did.
func joinACPChunk(prev *acpFrame, next acpFrame) bool {
	a, ok := chunkOf(*prev)
	b, ok2 := chunkOf(next)
	if !ok || !ok2 || !slices.Equal(a.key, b.key) {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(prev.Frame, &m); err != nil {
		return false
	}
	params, _ := m["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	content, _ := update["content"].(map[string]any)
	content["text"] = a.text + b.text
	joined, err := json.Marshal(m)
	if err != nil {
		return false
	}
	prev.Frame = joined
	return true
}

type acpChunk struct {
	key  []string
	text string
}

func chunkOf(f acpFrame) (acpChunk, bool) {
	var m struct {
		Method string `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind      string `json:"sessionUpdate"`
				MessageID string `json:"messageId"`
				Content   struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
	}
	if f.Dir != "agent" || json.Unmarshal(f.Frame, &m) != nil || m.Method != "session/update" || m.Params.Update.Content.Type != "text" {
		return acpChunk{}, false
	}
	u := m.Params.Update
	if u.Kind != "agent_message_chunk" && u.Kind != "agent_thought_chunk" {
		return acpChunk{}, false
	}
	return acpChunk{key: []string{m.Params.SessionID, u.Kind, u.MessageID}, text: u.Content.Text}, true
}
