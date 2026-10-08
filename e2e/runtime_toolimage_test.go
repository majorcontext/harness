package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// A Claude Code turn with an allowed list runs the MCP tools of the runtime
// through the loopback server, and the CLI gets the image of a result as
// image content, as it did when it connected the server itself.
func TestClaudeCodeGetsTheImageOfAnMCPTool(t *testing.T) {
	skipShort(t)
	srv := harnesstest.NewMCPServer(t, harnesstest.MCPSpec{Name: "weather", Tools: []harnesstest.MCPTool{{
		Name: "shot", Description: "Returns an image",
		Result: harnesstest.MCPResult{Content: []harnesstest.MCPContent{
			{Type: harnesstest.MCPContentImage, MimeType: "image/png", Data: mcpPNG},
		}},
	}}})
	srv.RequireAuthorization(mcpToken)
	dir := t.TempDir()
	toolLog := filepath.Join(dir, "tool.jsonl")
	for k, v := range map[string]string{
		"FAKE_CLAUDE_MODE": "normal", "FAKE_CLAUDE_STATE": filepath.Join(dir, "state"),
		"FAKE_CLAUDE_CALL_TOOL": "mcp__weather__shot", "FAKE_CLAUDE_TOOL_LOG": toolLog,
		"FAKE_CLAUDE_INIT_TOOLS": `["mcp__harness__mcp__weather__shot"]`,
	} {
		t.Setenv(k, v)
	}
	raw, err := json.Marshal(map[string]any{
		"model": "claude-code/sonnet",
		"providers": map[string]any{"claude-code": map[string]any{
			"type": "claude-code-cli", "binary_path": fakeClaudePath()}},
		"mcp_servers": map[string]any{"weather": map[string]any{"url": srv.URL(), "headers": map[string]string{"Authorization": mcpToken}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), WorkDir: dir, Config: *cfg})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "claude-code/sonnet", AllowedTools: []string{"mcp__weather__shot"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, s)
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(toolLog)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Result struct {
			Content []struct {
				Type, Data, MimeType string
			}
			IsError bool
		}
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &reply); err != nil {
		t.Fatalf("tool reply %q: %v", data, err)
	}
	var got string
	for _, c := range reply.Result.Content {
		if c.Type == "image" {
			got = c.MimeType + " " + c.Data
		}
	}
	if want := "image/png " + mcpPNG; got != want || reply.Result.IsError {
		t.Errorf("CLI tool reply = %s, want an image/png content of the screenshot", data)
	}
}
