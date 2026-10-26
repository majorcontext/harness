package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/majorcontext/harness/harnesstest"
)

// workerdEnv is set by the workerd job of CI. Without it the test skips, because it builds the module and needs the
// workerd package; with it, a missing node or workerd fails the test.
const workerdEnv = "HARNESS_E2E_WORKERD"

// workerdKey is the credential that the Worker adds to the model request. The module never sees it.
const workerdKey = "workerd-test-key"

// TestWorkerdRunsATurnInProcess pins the sentence of the WebAssembly section on the in-process mode: the module runs
// in a Cloudflare Worker with no Worker of its own, and a turn on a stand-in openai-compat model ends with the text of
// the model, and the model request carries the credential that the Worker added.
func TestWorkerdRunsATurnInProcess(t *testing.T) {
	skipShort(t)
	if os.Getenv(workerdEnv) != "1" {
		t.Skip(workerdEnv + "=1 enables the workerd job")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("the workerd job needs node: %v", err)
	}
	embed := filepath.Join(filepath.Dir(mustGetwd(t)), "sdk", "embed")
	if _, err := os.Stat(filepath.Join(embed, "node_modules", "workerd")); err != nil {
		t.Fatalf("workerd is not installed: run npm ci in sdk/embed")
	}
	wasm, execJS, err := buildNodeModule()
	if err != nil {
		t.Fatal(err)
	}
	fake := harnesstest.NewChat(t, replyText("hello from the worker"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join(embed, "test", "workerd", "run.mjs"), wasm, execJS, fake.URL(), workerdKey)
	cmd.Dir = embed
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run.mjs: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	var got struct {
		StopReason string `json:"stopReason"`
		Text       string `json:"text"`
		Failure    string `json:"failure"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("answer %q: %v", stdout.String(), err)
	}
	if got.Failure != "" || got.StopReason != "end_turn" || got.Text != "hello from the worker" {
		t.Errorf("answer = %+v, want stop reason end_turn and the text of the model\n%s", got, stderr.String())
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model requests = %d, want 1", len(reqs))
	}
	if auth := reqs[0].Header.Get("Authorization"); auth != "Bearer "+workerdKey {
		t.Errorf("Authorization = %q, want the credential that the Worker added", auth)
	}
}
