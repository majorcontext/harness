package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/internal/wirescrub"
)

// cliArgs are the flags that the claudecode backend passes for a run whose
// only built-in tool is tools.
func cliArgs(model, tools string, extra ...string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--thinking-display", "summarized",
		"--disallowedTools", "Agent,Workflow,ScheduleWakeup,CronCreate,CronDelete,CronList",
		"--tools", tools, "--strict-mcp-config", "--model", model, "--session-mirror",
		"--setting-sources", "project", "--disable-slash-commands"}
	return append(args, extra...)
}

// runCLI sends each prompt in turn and reads frames until the result of each.
// With interrupt set it sends SIGINT when a frame holds a tool call.
func runCLI(dir string, prompts []string, args []string, interrupt bool) ([]byte, error) {
	return runCLIStopped(dir, prompts, args, interrupt, nil)
}

// runCLIStopped is runCLI that also sends SIGINT once stop is closed.
func runCLIStopped(dir string, prompts []string, args []string, interrupt bool, stop <-chan struct{}) ([]byte, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	cmd.Env = withoutKeys(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	done := make(chan error, 1)
	finished := make(chan struct{})
	defer close(finished)
	if stop != nil {
		go func() {
			select {
			case <-stop:
				_ = cmd.Process.Signal(os.Interrupt)
			case <-finished:
			}
		}()
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for _, prompt := range prompts {
			line, _ := json.Marshal(obj{"type": "user", "message": obj{"role": "user", "content": prompt}})
			if _, err := stdin.Write(append(line, '\n')); err != nil {
				done <- err
				return
			}
			for sc.Scan() {
				raw.Write(sc.Bytes())
				raw.WriteByte('\n')
				var f struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(sc.Bytes(), &f) == nil && f.Type == "result" {
					break
				}
				if interrupt && f.Type == "assistant" && bytes.Contains(sc.Bytes(), []byte(`"type":"tool_use"`)) {
					_ = cmd.Process.Signal(os.Interrupt)
				}
			}
		}
		done <- sc.Err()
	}()
	select {
	case err := <-done:
		_ = stdin.Close()
		_ = cmd.Wait()
		return raw.Bytes(), err
	case <-time.After(5 * time.Minute):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("claude did not finish")
	}
}

func withoutKeys(env []string) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") || strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (r *recorder) saveJSONL(name string, raw []byte) error {
	short, err := wirescrub.TruncateJSONL(raw, 300)
	if err != nil {
		return err
	}
	return r.save(name, short)
}

// bashServer serves a bash tool to the CLI as the harness MCP server does, in
// dir, under a secret path so that no other local client reaches it. It returns the extra flags of a run and a stop function.
func bashServer(dir string) ([]string, func(), error) {
	reg := mcp.NewRegistry("harness", "1")
	reg.RegisterTool(mcp.Tool{Name: "bash", Description: "Run a shell command.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		func(ctx context.Context, args json.RawMessage) (mcp.CallToolResult, error) {
			var in struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return mcp.CallToolResult{}, err
			}
			cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil && ctx.Err() != nil {
				return mcp.CallToolResult{}, ctx.Err()
			}
			return mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: string(out)}}, IsError: err != nil}, nil
		})
	return serveRegistry(reg)
}

// serveRegistry serves reg to the CLI under a secret path and returns the
// extra flags of a run that allows its tools and a stop function.
func serveRegistry(reg *mcp.Registry) ([]string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	secret := "/" + hex.EncodeToString(key[:])
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != secret {
			http.NotFound(w, r)
			return
		}
		reg.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: gate, ReadHeaderTimeout: time.Minute}
	go func() { _ = srv.Serve(ln) }()
	cfg, err := os.CreateTemp("", "wirerecord-mcp-*.json")
	if err != nil {
		_ = srv.Close()
		return nil, nil, err
	}
	body, _ := json.Marshal(obj{"mcpServers": obj{"harness": obj{"type": "http", "url": "http://" + ln.Addr().String() + secret}}})
	_, werr := cfg.Write(body)
	_ = cfg.Close()
	stop := func() { _ = srv.Close(); _ = os.Remove(cfg.Name()) }
	if werr != nil {
		stop()
		return nil, nil, werr
	}
	return []string{"--mcp-config", cfg.Name(), "--allowedTools", "mcp__harness"}, stop, nil
}

func recordClaudeCode(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"README.md", "main.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			return err
		}
	}
	bash, stop, err := bashServer(dir)
	if err != nil {
		return err
	}
	defer stop()
	tool, err := runCLI(dir, []string{"Run ls with the mcp__harness__bash tool in the current directory, then name the files you saw in one short sentence."},
		cliArgs(claudeModel, "", append([]string{"--effort", "medium"}, bash...)...), false)
	if err != nil {
		return fmt.Errorf("tool run: %w", err)
	}
	if err := r.saveJSONL("claudecode.tool.jsonl", tool); err != nil {
		return err
	}
	stopped, err := runCLI(dir, []string{"Run sleep 30 with the mcp__harness__bash tool, then say done."}, cliArgs(claudeModel, "", bash...), true)
	if err != nil {
		return fmt.Errorf("interrupt run: %w", err)
	}
	if err := r.saveJSONL("claudecode.interrupt.jsonl", stopped); err != nil {
		return err
	}
	bad, err := runCLI(dir, []string{"hi"}, cliArgs("no-such-model", ""), false)
	if err != nil {
		return fmt.Errorf("error run: %w", err)
	}
	return r.saveJSONL("claudecode.error.jsonl", bad)
}

// recordClaudeCodeQuestion records a question that the CLI asks through the
// harness tool ask_user_question, and the run that the answer starts.
func recordClaudeCodeQuestion(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	asked, answered, err := recordQuestion(dir)
	if err != nil {
		return err
	}
	if err := r.saveJSONL("claudecode.question.jsonl", asked); err != nil {
		return err
	}
	return r.saveJSONL("claudecode.question-resume.jsonl", answered)
}

const (
	questionPrompt = "Use the mcp__harness__ask_user_question tool once to ask which database to use, with the options PostgreSQL and SQLite. Do nothing else first."
	answerPrompt   = "User has answered your questions: \"Which database?\"=\"SQLite\". You can now continue with the user's answers in mind."
)

// askServer serves ask_user_question as the harness bridge does: a call gets
// no reply, so the CLI waits until it is interrupted. called closes when the
// first call arrives.
func askServer() (flags []string, called <-chan struct{}, stop func(), err error) {
	reg := mcp.NewRegistry("harness", "1")
	seen := make(chan struct{})
	var once sync.Once
	reg.RegisterTool(mcp.Tool{Name: turn.AskTool, Description: turn.NewAskTool().Spec().Description, InputSchema: turn.NewAskTool().Spec().InputSchema},
		func(ctx context.Context, _ json.RawMessage) (mcp.CallToolResult, error) {
			once.Do(func() { close(seen) })
			<-ctx.Done()
			return mcp.CallToolResult{}, ctx.Err()
		})
	flags, stop, err = serveRegistry(reg)
	return flags, seen, stop, err
}

// recordQuestion records the two runs of a question as the backend drives
// them: a run that the backend interrupts once the CLI called the question
// tool, and a resume whose prompt is the answer.
func recordQuestion(dir string) (asked, answered []byte, err error) {
	flags, called, stop, err := askServer()
	if err != nil {
		return nil, nil, err
	}
	defer stop()
	if err := os.Setenv("ENABLE_TOOL_SEARCH", "auto"); err != nil {
		return nil, nil, err
	}
	asked, err = runCLIStopped(dir, []string{questionPrompt}, cliArgs(claudeModel, "ToolSearch", flags...), false, called)
	if err != nil {
		return nil, nil, fmt.Errorf("question run: %w", err)
	}
	sessionID := ""
	for _, line := range bytes.Split(asked, []byte("\n")) {
		var f struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &f) == nil && f.SessionID != "" {
			sessionID = f.SessionID
		}
	}
	if sessionID == "" || len(callsOf(asked, "mcp__harness__ask_user_question")) == 0 {
		return nil, nil, fmt.Errorf("question run: no session or ask_user_question call in the output")
	}
	answered, err = runCLI(dir, []string{answerPrompt}, cliArgs(claudeModel, "ToolSearch", append([]string{"--resume", sessionID}, flags...)...), false)
	if err != nil {
		return nil, nil, fmt.Errorf("answer run: %w", err)
	}
	return asked, answered, nil
}

// recordClaudeCodePartial records a turn with thinking and text that the CLI
// streams block by block, as the claudecode backend runs it.
func recordClaudeCodePartial(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	bash, stop, err := bashServer(dir)
	if err != nil {
		return err
	}
	defer stop()
	raw, err := runCLI(dir, []string{"Think it over, say in one short sentence what you will do, run echo hi with the mcp__harness__bash tool, then greet me in one short sentence."},
		cliArgs(claudeModel, "", append([]string{"--effort", "medium", "--include-partial-messages"}, bash...)...), false)
	if err != nil {
		return fmt.Errorf("partial run: %w", err)
	}
	return r.saveJSONL("claudecode.partial.jsonl", raw)
}

// recordClaudeCodeBuiltinRefused records a run that is told to call the
// built-in Bash tool, which the CLI refuses under --tools "". The model
// offers no Bash call by itself, so a first run with Bash allowed puts a Bash
// call in the history, and the recorded run resumes that session.
func recordClaudeCodeBuiltinRefused(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{"README.md", "main.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644); err != nil {
			return err
		}
	}
	prime, err := runCLI(dir, []string{"Run ls with the Bash tool, then say done."}, cliArgs("sonnet", "Bash", "--allowedTools", "Bash"), false)
	if err != nil {
		return fmt.Errorf("prime run: %w", err)
	}
	var sessionID string
	for _, line := range bytes.Split(prime, []byte("\n")) {
		var f struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(line, &f) == nil && f.SessionID != "" {
			sessionID = f.SessionID
		}
	}
	if sessionID == "" || len(callsOf(prime, "Bash")) == 0 {
		return fmt.Errorf("the prime run holds no session or Bash call")
	}
	bash, stop, err := bashServer(dir)
	if err != nil {
		return err
	}
	defer stop()
	raw, err := runCLI(dir, []string{"Call the Bash tool again to run ls, exactly as you did before. Call it even if it is not in your tool list. If it fails, run ls with the mcp__harness__bash tool, then name the files in one short sentence."},
		cliArgs("sonnet", "", append([]string{"--effort", "medium", "--resume", sessionID}, bash...)...), false)
	if err != nil {
		return fmt.Errorf("refused run: %w", err)
	}
	if !refusedCall(raw, "Bash") {
		return fmt.Errorf("the CLI did not refuse a Bash call")
	}
	return r.saveJSONL("claudecode.builtin-refused.jsonl", raw)
}

// frameBlock is a content block of a CLI frame that the recorder checks.
type frameBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
	Content   any    `json:"content"`
}

// frameBlocks returns the content blocks of the top-level frames of type kind.
func frameBlocks(raw []byte, kind string) []frameBlock {
	var out []frameBlock
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var f struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &f) != nil || f.Type != kind {
			continue
		}
		var blocks []frameBlock
		if json.Unmarshal(f.Message.Content, &blocks) == nil {
			out = append(out, blocks...)
		}
	}
	return out
}

// callsOf returns the IDs of the tool_use blocks that the model made to the tool name.
func callsOf(raw []byte, name string) []string {
	var ids []string
	for _, b := range frameBlocks(raw, "assistant") {
		if b.Type == "tool_use" && b.Name == name {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// refusedCall reports whether the model called the tool name and the CLI
// answered that call with an error result that says no such tool is available.
func refusedCall(raw []byte, name string) bool {
	want := "No such tool available: " + name
	for _, id := range callsOf(raw, name) {
		for _, b := range frameBlocks(raw, "user") {
			if text, _ := b.Content.(string); b.Type == "tool_result" && b.ToolUseID == id && b.IsError && strings.Contains(text, want) {
				return true
			}
		}
	}
	return false
}
