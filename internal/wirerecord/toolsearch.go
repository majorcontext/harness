package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/internal/mcp"
)

// recordClaudeCodeToolSearch records a CLI turn whose MCP server holds many
// tools, so that the CLI defers their schemas and loads one through the
// built-in ToolSearch tool, as the claudecode backend runs it.
func recordClaudeCodeToolSearch(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	reg := mcp.NewRegistry("harness", "1")
	for i := range 40 {
		reg.RegisterTool(mcp.Tool{Name: fmt.Sprintf("tool_%d", i), Description: fmt.Sprintf("Report the number %d. ", i) + strings.Repeat("It takes no input. ", 20),
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)},
			func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
				return mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: "ok"}}}, nil
			})
	}
	flags, stop, err := serveRegistry(reg)
	if err != nil {
		return err
	}
	defer stop()
	if err := os.Setenv("ENABLE_TOOL_SEARCH", "true"); err != nil {
		return err
	}
	defer func() { _ = os.Unsetenv("ENABLE_TOOL_SEARCH") }()
	raw, err := runCLI(dir, []string{"Call the mcp__harness__tool_7 tool once, then say what it returned in one short sentence."},
		cliArgs(claudeModel, "ToolSearch", append([]string{"--effort", "medium"}, flags...)...), false)
	if err != nil {
		return fmt.Errorf("toolsearch run: %w", err)
	}
	if len(callsOf(raw, "ToolSearch")) == 0 {
		return fmt.Errorf("the CLI did not call ToolSearch")
	}
	return r.saveJSONL("claudecode.toolsearch.jsonl", raw)
}
