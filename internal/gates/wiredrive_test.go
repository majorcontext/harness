package gates

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

func post(t *testing.T, url string, header map[string]string, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

// fakeRun is what one scripted request to a fake returns.
type fakeRun struct {
	status int
	body   []byte
}

func observeRun(t *testing.T, w Wire, s Shape, run fakeRun) {
	t.Helper()
	if run.status != http.StatusOK {
		var body map[string]any
		if err := json.Unmarshal(run.body, &body); err != nil {
			t.Fatalf("%s error body: %v", w.Name, err)
		}
		w.ObserveError(s, body)
		return
	}
	events, err := DecodeSSE(run.body)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		w.Observe(s, ev)
	}
}

var toolCall = harnesstest.ToolCall{ID: "toolu_1", Name: "list_files", Input: map[string]any{"dir": "."}}

const anthropicRequest = `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

func fakeAnthropicShape(t *testing.T) Shape {
	t.Helper()
	replies := []harnesstest.Reply{
		{Text: "ok"},
		{Text: "calling", ToolCalls: []harnesstest.ToolCall{toolCall}},
		{ToolCalls: []harnesstest.ToolCall{toolCall}},
		{Text: "cut", StopReason: "max_tokens", Usage: harnesstest.Usage{Input: 9, Output: 4}},
		{HTTPStatus: http.StatusNotFound, ErrorMessage: "model: m"},
		{HTTPStatus: http.StatusTooManyRequests},
	}
	s := Shape{}
	for _, rep := range replies {
		srv := harnesstest.New(t, harnesstest.Step{Name: "only", Reply: rep})
		status, body := post(t, srv.URL()+"/v1/messages", nil, anthropicRequest)
		observeRun(t, AnthropicWire, s, fakeRun{status, body})
	}
	return s
}

const chatRequest = `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`

func fakeChatShape(t *testing.T) Shape {
	t.Helper()
	replies := []harnesstest.Reply{
		{Text: "ok"},
		{Text: "calling", ToolCalls: []harnesstest.ToolCall{toolCall}},
		{ToolCalls: []harnesstest.ToolCall{toolCall}},
		{Text: "cut", StopReason: "max_tokens", Usage: harnesstest.Usage{Input: 9, Output: 4}},
		{Reasoning: "think", Text: "ok"},
		{HTTPStatus: http.StatusNotFound, ErrorMessage: "model m", ErrorCode: "model_not_found"},
	}
	s := Shape{}
	for _, rep := range replies {
		srv := harnesstest.NewChat(t, harnesstest.Step{Name: "only", Reply: rep})
		status, body := post(t, srv.URL()+"/v1/chat/completions", nil, chatRequest)
		observeRun(t, ChatWire, s, fakeRun{status, body})
	}
	return s
}

const responsesRequest = `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`

func fakeResponsesShape(t *testing.T) Shape {
	t.Helper()
	cases := []struct {
		reply harnesstest.Reply
		extra harnesstest.CodexReply
	}{
		{reply: harnesstest.Reply{Text: "ok"}},
		{reply: harnesstest.Reply{Text: "calling", ToolCalls: []harnesstest.ToolCall{toolCall}}},
		{reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{toolCall}}},
		{reply: harnesstest.Reply{Text: "ok"}, extra: harnesstest.CodexReply{Reasoning: []string{"think"}}},
	}
	s := Shape{}
	for _, c := range cases {
		srv := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Replies: map[string]harnesstest.CodexReply{"only": c.extra}}, harnesstest.Step{Name: "only", Reply: c.reply})
		status, body := post(t, srv.URL()+"/backend-api/codex/responses", map[string]string{"Authorization": "Bearer k"}, responsesRequest)
		observeRun(t, ResponsesWire, s, fakeRun{status, body})
	}
	return s
}

var (
	fakeClaudeOnce sync.Once
	fakeClaudePath string
	fakeClaudeErr  error
)

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	fakeClaudeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeclaude-")
		if err != nil {
			fakeClaudeErr = err
			return
		}
		fakeClaudePath = filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", fakeClaudePath, "./harnesstest/fakeclaude")
		cmd.Dir = "../.."
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeClaudeErr = &exec.ExitError{Stderr: out}
		}
	})
	if fakeClaudeErr != nil {
		t.Fatalf("build fakeclaude: %v", fakeClaudeErr)
	}
	return fakeClaudePath
}

func fakeClaudeModes(t *testing.T, bin string) []string {
	t.Helper()
	out, err := exec.Command(bin, "--list-modes").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

// fakeClaudeFrames runs the fake in one mode with one user line on stdin and
// returns the frames it printed before it ended or the deadline passed.
func fakeClaudeFrames(t *testing.T, bin, mode string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	state := filepath.Join(t.TempDir(), "state")
	cmd.Env = append(os.Environ(), "FAKE_CLAUDE_MODE="+mode, "FAKE_CLAUDE_STATE="+state)
	cmd.Stdin = strings.NewReader(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	return out.Bytes()
}

func fakeClaudeShape(t *testing.T) Shape {
	t.Helper()
	bin := buildFakeClaude(t)
	modes := append([]string{""}, fakeClaudeModes(t, bin)...)
	frames := make([][]byte, len(modes))
	var wg sync.WaitGroup
	for i, mode := range modes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames[i] = fakeClaudeFrames(t, bin, mode)
		}()
	}
	wg.Wait()
	s := Shape{}
	for _, data := range frames {
		events, err := DecodeJSONL(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events {
			ClaudeCodeWire.Observe(s, ev)
		}
	}
	return s
}
