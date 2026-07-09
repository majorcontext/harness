package plugin

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/majorcontext/harness/message"
)

// TestTypeScriptSDKConformance exercises the TypeScript/JS plugin SDK
// (sdk/typescript/harness-plugin.mjs) against the real plugin.Host and
// plugin.Probe, via the reference redactor.mjs plugin
// (examples/plugins/redactor.mjs). It proves that a plugin written against
// the TS SDK speaks the exact same wire protocol the Go SDK does: manifest
// shape, hook mutation round-trips, and custom tool execution.
//
// Skips (rather than fails) when node isn't on PATH, since this repo has no
// Go dependency on Node — the TS SDK is zero-Go-deps by design.
func TestTypeScriptSDKConformance(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found on PATH; skipping TypeScript SDK conformance test")
	}

	pluginPath, err := filepath.Abs(filepath.Join("..", "examples", "plugins", "redactor.mjs"))
	if err != nil {
		t.Fatal(err)
	}

	command := []string{nodePath, pluginPath}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	manifest, err := Probe(ctx, command)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if manifest.Name != "redactor" {
		t.Errorf("manifest.Name = %q, want redactor", manifest.Name)
	}
	if manifest.ProtocolVersion != ProtocolVersion {
		t.Errorf("manifest.ProtocolVersion = %d, want %d", manifest.ProtocolVersion, ProtocolVersion)
	}

	gotHooks := make([]string, len(manifest.Hooks))
	for i, h := range manifest.Hooks {
		gotHooks[i] = string(h)
	}
	sort.Strings(gotHooks)
	wantHooks := []string{"system.transform", "tool.execute.after"}
	if len(gotHooks) != len(wantHooks) {
		t.Fatalf("manifest.Hooks = %v, want %v", gotHooks, wantHooks)
	}
	for i := range wantHooks {
		if gotHooks[i] != wantHooks[i] {
			t.Errorf("manifest.Hooks = %v, want %v", gotHooks, wantHooks)
		}
	}

	if len(manifest.Tools) != 1 || manifest.Tools[0].Name != "echo_reverse" {
		t.Fatalf("manifest.Tools = %+v, want [echo_reverse]", manifest.Tools)
	}

	h, err := NewHost(Options{HookTimeout: 5 * time.Second}, Spec{
		Command:  command,
		Manifest: manifest,
	})
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer h.Close()

	// Mutation round-trip: tool.execute.after must redact the secret.
	out := h.ToolExecuteAfter(context.Background(), &ToolExecuteAfterRequest{
		SessionID: "s1",
		CallID:    "c1",
		Tool:      "bash",
		Output:    message.Parts{&message.Text{Text: "here is a key: sk-abcdefghijklmnopqrstuvwx"}},
	})
	if got := out.Text(); got != "here is a key: [REDACTED]" {
		t.Errorf("redacted output = %q", got)
	}

	// Non-secret output passes through unchanged (no mutation).
	out2 := h.ToolExecuteAfter(context.Background(), &ToolExecuteAfterRequest{
		SessionID: "s1",
		CallID:    "c2",
		Tool:      "bash",
		Output:    message.Parts{&message.Text{Text: "nothing secret here"}},
	})
	if got := out2.Text(); got != "nothing secret here" {
		t.Errorf("unredacted output = %q, want unchanged", got)
	}

	// Custom tool execution.
	resp, err := h.ExecuteTool(context.Background(), &ToolExecuteRequest{
		SessionID: "s1",
		CallID:    "c3",
		Tool:      "echo_reverse",
		Args:      json.RawMessage(`{"text":"harness"}`),
	})
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if resp.Output.Text() != "ssenrah" {
		t.Errorf("echo_reverse output = %q, want ssenrah", resp.Output.Text())
	}
}
