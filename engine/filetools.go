package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

const (
	// readFileDefaultLimit is the default maximum number of lines returned
	// by read_file.
	readFileDefaultLimit = 2000
	// readFileMaxLineLen is the maximum length of a single returned line;
	// longer lines are truncated with a trailing ellipsis.
	readFileMaxLineLen = 2000
)

// resolvePath resolves a tool path argument against the session working
// directory. Absolute paths pass through unchanged.
func (s *Session) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(s.cfg.WorkDir, path)
}

func readFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "read_file",
			Description: "Read a file and return its content with line numbers (N→ prefixes). Prefer this over shell commands like cat, head, or sed for reading files. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to read"},
					"offset": {"type": "integer", "description": "1-based line number to start reading from"},
					"limit": {"type": "integer", "description": "Maximum number of lines to return (default 2000)"}
				},
				"required": ["path"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
				return nil, fmt.Errorf("read_file: missing path argument")
			}
			path := s.resolvePath(in.Path)
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("read_file: %w", err)
			}
			if info.IsDir() {
				return nil, fmt.Errorf("read_file: %s is a directory", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read_file: %w", err)
			}

			lines := strings.Split(string(data), "\n")
			// A trailing newline produces one empty trailing element; drop it.
			if n := len(lines); n > 0 && lines[n-1] == "" {
				lines = lines[:n-1]
			}
			total := len(lines)

			offset := in.Offset
			if offset < 1 {
				offset = 1
			}
			limit := in.Limit
			if limit <= 0 {
				limit = readFileDefaultLimit
			}
			if total == 0 {
				return message.Parts{&message.Text{Text: "(empty file)"}}, nil
			}
			if offset > total {
				return nil, fmt.Errorf("read_file: offset %d is past end of file (%d lines)", offset, total)
			}
			end := offset + limit - 1
			if end > total {
				end = total
			}

			var b strings.Builder
			for i := offset; i <= end; i++ {
				line := lines[i-1]
				if r := []rune(line); len(r) > readFileMaxLineLen {
					line = string(r[:readFileMaxLineLen]) + "…"
				}
				fmt.Fprintf(&b, "%d→%s\n", i, line)
			}
			out := strings.TrimSuffix(b.String(), "\n")
			if end < total {
				out += fmt.Sprintf("\n[truncated: showing lines %d-%d of %d]", offset, end, total)
			}
			return message.Parts{&message.Text{Text: out}}, nil
		},
	}
}

func writeFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "write_file",
			Description: "Write content to a file, creating parent directories as needed and overwriting any existing file. Prefer this over shell redirection or heredocs for creating and rewriting files. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to write"},
					"content": {"type": "string", "description": "Full content to write to the file"}
				},
				"required": ["path", "content"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.Content == nil {
				return nil, fmt.Errorf("write_file: missing path or content argument")
			}
			path := s.resolvePath(in.Path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
			if err := os.WriteFile(path, []byte(*in.Content), 0o644); err != nil {
				return nil, fmt.Errorf("write_file: %w", err)
			}
			return message.Parts{&message.Text{Text: fmt.Sprintf("wrote %d bytes to %s", len(*in.Content), path)}}, nil
		},
	}
}

func editFileTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "edit_file",
			Description: "Replace an exact string in a file. Prefer this over sed or shell heredocs for editing files. old_string must match the file content exactly and uniquely; include surrounding context to disambiguate, or set replace_all to replace every occurrence. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Path to the file to edit"},
					"old_string": {"type": "string", "description": "Exact text to replace"},
					"new_string": {"type": "string", "description": "Replacement text"},
					"replace_all": {"type": "boolean", "description": "Replace every occurrence (default false)"}
				},
				"required": ["path", "old_string", "new_string"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.OldString == "" {
				return nil, fmt.Errorf("edit_file: missing path or old_string argument")
			}
			if in.OldString == in.NewString {
				return nil, fmt.Errorf("edit_file: old_string and new_string are identical")
			}
			path := s.resolvePath(in.Path)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			content := string(data)

			count := strings.Count(content, in.OldString)
			switch {
			case count == 0:
				return nil, fmt.Errorf("edit_file: old_string not found in %s", path)
			case count > 1 && !in.ReplaceAll:
				return nil, fmt.Errorf("edit_file: old_string matches %d times in %s; provide more surrounding context to make it unique, or set replace_all to true", count, path)
			}

			replaced := count
			if !in.ReplaceAll {
				replaced = 1
			}
			content = strings.Replace(content, in.OldString, in.NewString, replaced)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return nil, fmt.Errorf("edit_file: %w", err)
			}
			return message.Parts{&message.Text{Text: fmt.Sprintf("replaced %d occurrence(s) in %s", replaced, path)}}, nil
		},
	}
}
