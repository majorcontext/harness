// Command acptck runs the ACP Test Compatibility Kit against `harness acp`
// with a scripted model, then judges the report against
// testdata/acp-tck-allow.txt. Run it from the repository root.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const (
	dummyKey     = "acptck-dummy-key"
	cancelPrompt = "acptck-hold-this-turn-open"
)

func main() {
	harness := flag.String("harness", "./harness", "harness binary under test")
	tck := flag.String("tck", "", "directory of the acp-tck checkout")
	allow := flag.String("allow", "testdata/acp-tck-allow.txt", "allow-list of MANDATORY failures")
	out := flag.String("report", "acp-tck-report.json", "path of the JSON report that the TCK writes")
	flag.Parse()
	if *tck == "" {
		fmt.Fprintln(os.Stderr, "acptck: -tck is required")
		os.Exit(2)
	}
	if err := run(*harness, *tck, *allow, *out); err != nil {
		fmt.Fprintln(os.Stderr, "acptck:", err)
		os.Exit(1)
	}
}

func run(harness, tck, allowFile, reportPath string) error {
	allowed, err := readAllow(allowFile)
	if err != nil {
		return err
	}
	bin, err := filepath.Abs(harness)
	if err != nil {
		return err
	}
	reportPath, err = filepath.Abs(reportPath)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "acptck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	var t scriptTB
	defer t.close()
	model := harnesstest.NewAnthropic(&t,
		harnesstest.Step{Name: "hold", Match: harnesstest.LastUserText(cancelPrompt), Reply: harnesstest.Reply{Text: "working", Block: true}, Repeat: true},
		harnesstest.Step{Name: "answer", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
	)

	cfgPath, err := writeConfig(work, model.URL())
	if err != nil {
		return err
	}
	agentCwd := filepath.Join(work, "cwd")
	if err := os.Mkdir(agentCwd, 0o755); err != nil {
		return err
	}

	cmd := exec.Command("uv", "--project", tck, "run", "--frozen", "acp-tck",
		"--agent-cwd", agentCwd,
		"--agent-env", "HARNESS_CONFIG="+cfgPath,
		"--agent-env", "ANTHROPIC_API_KEY="+dummyKey,
		"--cancel-prompt", cancelPrompt,
		"--timeout", "60", "--test-timeout", "180",
		"--report-json", reportPath,
		"--", bin, "acp")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// The TCK exits 1 on any non-conformance; the allow-list decides the result.
	runErr := cmd.Run()

	data, err := os.ReadFile(reportPath)
	if err != nil {
		return fmt.Errorf("the TCK wrote no report (%v): %w", runErr, err)
	}
	var rep report
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("parse report: %w", err)
	}
	var buf bytes.Buffer
	ok := judge(&buf, rep, allowed)
	_, _ = os.Stdout.Write(buf.Bytes())
	if !ok {
		return fmt.Errorf("the MANDATORY tier does not match %s", allowFile)
	}
	return nil
}

func writeConfig(dir, baseURL string) (string, error) {
	cfg := map[string]any{
		"model":       "anthropic/claude-fable-5",
		"session_dir": filepath.Join(dir, "sessions"),
		"providers": map[string]any{
			"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": baseURL},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "config.json")
	return path, os.WriteFile(path, b, 0o600)
}

// scriptTB lets the scripted model server run outside a test. It implements
// only the methods that harnesstest calls on its start-up path.
type scriptTB struct {
	testing.TB
	cleanups []func()
}

func (t *scriptTB) Helper()          {}
func (t *scriptTB) Failed() bool     { return false }
func (t *scriptTB) Cleanup(f func()) { t.cleanups = append(t.cleanups, f) }
func (t *scriptTB) Errorf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "acptck: "+f+"\n", a...)
}

func (t *scriptTB) close() {
	for i := len(t.cleanups) - 1; i >= 0; i-- {
		t.cleanups[i]()
	}
}
