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
	"time"

	"github.com/majorcontext/harness/internal/mcp"
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
	if err := r.saveJSONL("claudecode.error.jsonl", bad); err != nil {
		return err
	}
	parked, resumed, err := runQuestion(dir)
	if err != nil {
		return err
	}
	if err := r.saveJSONL("claudecode.question.jsonl", parked); err != nil {
		return err
	}
	return r.saveJSONL("claudecode.question-resume.jsonl", resumed)
}

const (
	deferHook      = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"defer"}}`
	questionPrompt = "Use the AskUserQuestion tool once to ask which database to use, with the options PostgreSQL and SQLite. Do nothing else first."
)

func questionHook(command string) string {
	hook := obj{"matcher": "AskUserQuestion", "hooks": []obj{{"type": "command", "command": command}}}
	data, _ := json.Marshal(obj{"hooks": obj{"PreToolUse": []obj{hook}}})
	return string(data)
}

func questionArgs(settings string, extra ...string) []string {
	return cliArgs(claudeModel, "AskUserQuestion", append([]string{"--permission-prompt-tool", "stdio", "--settings", settings}, extra...)...)
}

// runQuestion records the two runs of an AskUserQuestion flow as the backend
// drives it: a run that the defer hook parks, and a resume that answers the
// parked call over the control channel.
func runQuestion(dir string) (parked, resumed []byte, err error) {
	parked, err = runCLI(dir, []string{questionPrompt}, questionArgs(questionHook("cat >/dev/null; printf '%s' '"+deferHook+"'")), false)
	if err != nil {
		return nil, nil, fmt.Errorf("park run: %w", err)
	}
	var sessionID, callID string
	var input json.RawMessage
	for _, line := range bytes.Split(parked, []byte("\n")) {
		var f struct {
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type  string          `json:"type"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &f) != nil {
			continue
		}
		if f.SessionID != "" {
			sessionID = f.SessionID
		}
		for _, c := range f.Message.Content {
			if c.Type == "tool_use" && c.Name == "AskUserQuestion" {
				callID, input = c.ID, c.Input
			}
		}
	}
	if sessionID == "" || callID == "" {
		return nil, nil, fmt.Errorf("park run: no session or AskUserQuestion call in the output")
	}
	var updated map[string]any
	if err := json.Unmarshal(input, &updated); err != nil {
		return nil, nil, err
	}
	var asked struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(input, &asked); err != nil || len(asked.Questions) == 0 || len(asked.Questions[0].Options) == 0 {
		return nil, nil, fmt.Errorf("park run: the AskUserQuestion call holds no question with options")
	}
	updated["answers"] = obj{asked.Questions[0].Question: asked.Questions[0].Options[0].Label}
	pass := `input=$(cat); case "$input" in *'"` + callID + `"'*) ;; *) printf '%s' '` + deferHook + `';; esac`
	resumed, err = runResume(dir, questionArgs(questionHook(pass), "--resume", sessionID), obj{"behavior": "allow", "updatedInput": updated})
	if err != nil {
		return nil, nil, fmt.Errorf("resume run: %w", err)
	}
	return parked, resumed, nil
}

// runResume answers the first can_use_tool request with decision and reads
// frames until the result.
func runResume(dir string, args []string, decision obj) ([]byte, error) {
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
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			raw.Write(sc.Bytes())
			raw.WriteByte('\n')
			var f struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			if f.Type == "control_request" {
				reply, _ := json.Marshal(obj{"type": "control_response", "response": obj{"subtype": "success", "request_id": f.RequestID, "response": decision}})
				if _, err := stdin.Write(append(reply, '\n')); err != nil {
					done <- err
					return
				}
			}
			if f.Type == "result" {
				break
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
	if sessionID == "" || !bytes.Contains(prime, []byte(`"name":"Bash"`)) {
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
	if !bytes.Contains(raw, []byte(`"name":"Bash"`)) {
		return fmt.Errorf("the model did not call Bash")
	}
	return r.saveJSONL("claudecode.builtin-refused.jsonl", raw)
}
