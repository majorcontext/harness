package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/wirescrub"
)

// cliArgs are the flags that the claudecode backend passes for a run with one
// built-in tool.
func cliArgs(model, tools string, extra ...string) []string {
	args := []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--forward-subagent-text", "--thinking-display", "summarized",
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
	tool, err := runCLI(dir, []string{"Run ls with the Bash tool in the current directory, then name the files you saw in one short sentence."},
		cliArgs(claudeModel, "Bash", "--effort", "medium"), false)
	if err != nil {
		return fmt.Errorf("tool run: %w", err)
	}
	if err := r.saveJSONL("claudecode.tool.jsonl", tool); err != nil {
		return err
	}
	stopped, err := runCLI(dir, []string{"Run sleep 30 with the Bash tool, then say done."}, cliArgs(claudeModel, "Bash"), true)
	if err != nil {
		return fmt.Errorf("interrupt run: %w", err)
	}
	if err := r.saveJSONL("claudecode.interrupt.jsonl", stopped); err != nil {
		return err
	}
	bad, err := runCLI(dir, []string{"hi"}, cliArgs("no-such-model", "Bash"), false)
	if err != nil {
		return fmt.Errorf("error run: %w", err)
	}
	return r.saveJSONL("claudecode.error.jsonl", bad)
}
