package gates

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			fakeClaudeErr = fmt.Errorf("%w: %s", err, out)
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

const (
	claudeUserLine    = `{"type":"user","message":{"role":"user","content":"hi"}}`
	claudeAnswerLine  = `{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"behavior":"allow"}}}`
	claudeDismissLine = `{"type":"control_response","response":{"subtype":"success","request_id":"req-1","response":{"behavior":"deny","message":"dismissed","interrupt":true}}}`
	claudeMirrorDir   = "../../harnesstest/fakeclaude/testdata/"
	claudeRunBound    = 30 * time.Second
)

// fakeClaudeHangs holds the modes that print and then wait for a signal. The
// driver sends SIGINT after their first frame, so the frames of the
// interrupted turn reach the gate.
var fakeClaudeHangs = map[string]bool{
	"hang_after_text": true, "hang_after_listing": true, "hang_in_tool": true,
	"tool_on_interrupt": true, "tool_result_on_interrupt": true, "success_on_interrupt": true,
	"placeholder_on_interrupt": true, "exit_on_interrupt": true,
}

// fakeClaudeExitsByDesign holds the modes that end with an error or print no
// frame on purpose, or that need an MCP server this driver does not run.
var fakeClaudeExitsByDesign = map[string]bool{"crash": true, "crash_before_init": true, "mcp": true}

// claudeSpawn is one process of a scenario: the lines it reads on stdin, and
// whether the driver interrupts it after its first frame.
type claudeSpawn struct {
	stdin   []string
	signal  bool
	nonzero bool
}

// claudeRun is a sequence of spawns of one mode that share one state file, as
// the harness resumes a session with a new process.
type claudeRun struct {
	mode   string
	env    []string
	spawns []claudeSpawn
}

// claudeRuns lists the runs that show every frame a mode can print: one
// spawn for most modes, a park and a resume for each question mode, a
// dismissed question, two spawns for the mirror, a queued line for the modes
// that read one, and an interrupt for the modes that hang.
func claudeRuns(modes []string) []claudeRun {
	queued := []string{claudeUserLine, claudeUserLine}
	var runs []claudeRun
	for _, mode := range append([]string{""}, modes...) {
		one := claudeRun{mode: mode, spawns: []claudeSpawn{{stdin: []string{claudeUserLine}, nonzero: fakeClaudeExitsByDesign[mode]}}}
		switch {
		case fakeClaudeHangs[mode]:
			one.spawns = []claudeSpawn{{stdin: []string{claudeUserLine}, signal: true, nonzero: true}}
		case strings.HasPrefix(mode, "question"):
			resume := claudeSpawn{stdin: []string{claudeAnswerLine}}
			if mode == "question_continues" {
				resume.stdin = []string{claudeAnswerLine, claudeUserLine}
			}
			one.spawns = []claudeSpawn{{stdin: []string{claudeUserLine}}, resume}
			if mode == "question" {
				runs = append(runs, claudeRun{mode: mode, spawns: []claudeSpawn{{stdin: []string{claudeUserLine}}, {stdin: []string{claudeDismissLine}, nonzero: true}}})
			}
		case mode == "mirror":
			one.env = []string{"FAKE_CLAUDE_MIRROR_FIXTURE=" + claudeMirrorDir + "run1.stdout.jsonl," + claudeMirrorDir + "run2.stdout.jsonl"}
			one.spawns = []claudeSpawn{{stdin: []string{claudeUserLine}}, {stdin: []string{claudeUserLine}}}
		case mode == "queue_injection" || mode == "steer":
			one.spawns = []claudeSpawn{{stdin: queued}}
		}
		runs = append(runs, one)
	}
	return runs
}

func runClaudeSpawn(ctx context.Context, bin string, env []string, sp claudeSpawn) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "--include-partial-messages")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(strings.Join(sp.stdin, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	r := bufio.NewReader(stdout)
	var out bytes.Buffer
	if sp.signal {
		first, err := r.ReadBytes('\n')
		out.Write(first)
		if err != nil {
			_ = cmd.Wait()
			return out.Bytes(), errors.New("printed no frame before the signal")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Wait()
			return out.Bytes(), errors.New("ended before the signal; remove it from fakeClaudeHangs")
		}
	}
	rest, _ := io.ReadAll(r)
	out.Write(rest)
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return out.Bytes(), errors.New("did not end; add it to fakeClaudeHangs")
	}
	if _, exit := errors.AsType[*exec.ExitError](werr); werr != nil && (!exit || !sp.nonzero) {
		return out.Bytes(), werr
	}
	return out.Bytes(), nil
}

func runClaude(t *testing.T, bin string, run claudeRun) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), claudeRunBound)
	defer cancel()
	dir := t.TempDir()
	env := append(os.Environ(), "FAKE_CLAUDE_MODE="+run.mode, "FAKE_CLAUDE_STATE="+filepath.Join(dir, "state"),
		"FAKE_CLAUDE_SIGNAL_LOG="+filepath.Join(dir, "signal"), "CLAUDE_CONFIG_DIR="+filepath.Join(dir, "cfg"),
		"FAKE_CLAUDE_MIRROR_FIXTURE="+claudeMirrorDir+"run1.stdout.jsonl")
	env = append(env, run.env...)
	var outs [][]byte
	for i, sp := range run.spawns {
		out, err := runClaudeSpawn(ctx, bin, env, sp)
		if err != nil {
			t.Errorf("fakeclaude mode %q spawn %d: %v", run.mode, i+1, err)
		}
		if len(out) == 0 && !fakeClaudeExitsByDesign[run.mode] {
			t.Errorf("fakeclaude mode %q spawn %d printed no frames", run.mode, i+1)
		}
		outs = append(outs, out)
	}
	return outs
}

func fakeClaudeShape(t *testing.T) Shape {
	t.Helper()
	bin := buildFakeClaude(t)
	runs := claudeRuns(fakeClaudeModes(t, bin))
	frames := make([][][]byte, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			frames[i] = runClaude(t, bin, run)
		}()
	}
	wg.Wait()
	s := Shape{}
	for _, spawns := range frames {
		for _, data := range spawns {
			events, err := DecodeJSONL(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range events {
				ClaudeCodeWire.Observe(s, ev)
			}
		}
	}
	return s
}
