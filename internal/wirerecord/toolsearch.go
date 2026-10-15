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

// recordClaudeCodeUnknownRefused records a run whose harness tools are
// deferred and whose model calls the bare name bash before ToolSearch loads
// mcp__harness__bash. The CLI refuses the unknown name with no "." after it.
func recordClaudeCodeUnknownRefused(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	reg := mcp.NewRegistry("harness", "1")
	reg.RegisterTool(mcp.Tool{Name: "bash", Description: "Run a shell command.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		func(context.Context, json.RawMessage) (mcp.CallToolResult, error) {
			return mcp.CallToolResult{Content: []mcp.Content{{Type: "text", Text: "README.md"}}}, nil
		})
	flags, stop, err := serveRegistry(reg)
	if err != nil {
		return err
	}
	defer stop()
	if err := os.Setenv("ENABLE_TOOL_SEARCH", "true"); err != nil {
		return err
	}
	defer func() { _ = os.Unsetenv("ENABLE_TOOL_SEARCH") }()
	raw, err := runCLI(dir, []string{"Call the tool named exactly `bash`, with no prefix, with the command ls. Do not use ToolSearch first. If that fails, load the tool and run ls, then name the files in one short sentence."},
		cliArgs(claudeModel, "ToolSearch", append([]string{"--effort", "low"}, flags...)...), false)
	if err != nil {
		return fmt.Errorf("unknown-refused run: %w", err)
	}
	if !refusedCall(raw, "bash") {
		return fmt.Errorf("the CLI did not refuse a bash call")
	}
	return r.saveJSONL("claudecode.unknown-refused.jsonl", raw)
}
