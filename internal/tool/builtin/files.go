package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	readDefaultLimit = 2000
	maxLineLen       = 2000
)

// guard records the hash of each file that the session read or wrote, so
// write_file never overwrites a file that the model has not seen.
type guard struct {
	mu     sync.Mutex
	hashes map[string][32]byte
}

func (g *guard) record(path string, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hashes[path] = sha256.Sum256(data)
}

func (g *guard) hash(path string) ([32]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.hashes[path]
	return h, ok
}

const readFileSchema = `{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "Path to the file to read"},
		"offset": {"type": "integer", "description": "1-based line number to start reading from"},
		"limit": {"type": "integer", "description": "Maximum number of lines to return (default 2000)"}
	},
	"required": ["path"]
}`

func (d dir) readFile() tool {
	return newTool("read_file", "Read a file and return its content with line numbers (N→ prefixes). A recognized image file (PNG, JPEG, GIF, WebP) is returned as one line that names its type, size, and pixel dimensions, not its content. Prefer this over shell commands like cat, head, or sed for reading files. Relative paths resolve against the session working directory.",
		readFileSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" {
				return "", errors.New("read_file: missing path argument")
			}
			path := d.resolve(in.Path)
			info, err := os.Stat(path)
			if err != nil {
				return "", fmt.Errorf("read_file: %w", err)
			}
			if info.IsDir() {
				return "", fmt.Errorf("read_file: %s is a directory", path)
			}
			c, err := readContent(path)
			if err != nil {
				return "", fmt.Errorf("read_file: %s: %w", path, err)
			}
			if c.image {
				d.read.record(path, c.data)
				return fmt.Sprintf("image (%s), %d bytes, %dx%d pixels", c.mediaType, len(c.data), c.width, c.height), nil
			}
			return d.lines(path, c.data, in.Offset, in.Limit)
		})
}

// lines numbers the lines of data from offset. A read records the hash of
// the whole file, never only the lines it shows; a read that fails records nothing.
func (d dir) lines(path string, data []byte, offset, limit int) (string, error) {
	lines := strings.Split(string(data), "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	offset = max(offset, 1)
	if limit <= 0 {
		limit = readDefaultLimit
	}
	if total == 0 {
		d.read.record(path, data)
		return "(empty file)", nil
	}
	if offset > total {
		return "", fmt.Errorf("read_file: offset %d is past end of file (%d lines)", offset, total)
	}
	d.read.record(path, data)
	end := min(offset+limit-1, total)
	var b strings.Builder
	for i := offset; i <= end; i++ {
		fmt.Fprintf(&b, "%d→%s\n", i, cutLine(lines[i-1]))
	}
	out := strings.TrimSuffix(b.String(), "\n")
	if end < total {
		out += fmt.Sprintf("\n[truncated: showing lines %d-%d of %d]", offset, end, total)
	}
	return out, nil
}

func cutLine(line string) string {
	if r := []rune(line); len(r) > maxLineLen {
		return string(r[:maxLineLen]) + "…"
	}
	return line
}

const writeFileSchema = `{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "Path to the file to write"},
		"content": {"type": "string", "description": "Full content to write to the file"}
	},
	"required": ["path", "content"]
}`

func (d dir) writeFile() tool {
	return newTool("write_file", "Write content to a file, creating parent directories as needed. Overwriting an existing file requires having read it first with read_file this session, with no changes on disk since — use edit_file for a targeted change, or read_file then write_file to intentionally replace it. Relative paths resolve against the session working directory.",
		writeFileSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.Content == nil {
				return "", errors.New("write_file: missing path or content argument")
			}
			path := d.resolve(in.Path)
			if err := d.mayOverwrite(path); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return "", fmt.Errorf("write_file: %w", err)
			}
			if err := os.WriteFile(path, []byte(*in.Content), 0o644); err != nil {
				return "", fmt.Errorf("write_file: %w", err)
			}
			d.read.record(path, []byte(*in.Content))
			return fmt.Sprintf("wrote %d bytes to %s", len(*in.Content), path), nil
		})
}

// mayOverwrite gates only an existing regular file. A stat that fails for
// another reason refuses: it cannot prove that no file is there.
func (d dir) mayOverwrite(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("write_file: cannot stat %s to check the read-before-overwrite guard: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	seen, ok := d.read.hash(path)
	if !ok {
		return fmt.Errorf("write_file: %s exists and has not been read this session; read it first (or use edit_file)", path)
	}
	data, err := readCapped(path)
	if err != nil && !errors.Is(err, errTooLarge) {
		return fmt.Errorf("write_file: %w", err)
	}
	if err != nil || sha256.Sum256(data) != seen {
		return fmt.Errorf("write_file: %s changed on disk since it was read; read it again before overwriting", path)
	}
	return nil
}

const editFileSchema = `{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "Path to the file to edit"},
		"old_string": {"type": "string", "description": "Exact text to replace"},
		"new_string": {"type": "string", "description": "Replacement text"},
		"replace_all": {"type": "boolean", "description": "Replace every occurrence (default false)"}
	},
	"required": ["path", "old_string", "new_string"]
}`

func (d dir) editFile() tool {
	return newTool("edit_file", "Replace an exact string in a file. Prefer this over sed or shell heredocs for editing files. old_string must match the file content exactly and uniquely; include surrounding context to disambiguate, or set replace_all to replace every occurrence. Relative paths resolve against the session working directory.",
		editFileSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Path == "" || in.OldString == "" {
				return "", errors.New("edit_file: missing path or old_string argument")
			}
			if in.OldString == in.NewString {
				return "", errors.New("edit_file: old_string and new_string are identical")
			}
			path := d.resolve(in.Path)
			data, err := readCapped(path)
			if err != nil {
				return "", fmt.Errorf("edit_file: %w", err)
			}
			content := string(data)
			n := strings.Count(content, in.OldString)
			switch {
			case n == 0:
				return "", fmt.Errorf("edit_file: old_string not found in %s", path)
			case n > 1 && !in.ReplaceAll:
				return "", fmt.Errorf("edit_file: old_string matches %d times in %s; provide more surrounding context to make it unique, or set replace_all to true", n, path)
			}
			if !in.ReplaceAll {
				n = 1
			}
			content = strings.Replace(content, in.OldString, in.NewString, n)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("edit_file: %w", err)
			}
			d.read.record(path, []byte(content))
			return fmt.Sprintf("replaced %d occurrence(s) in %s", n, path), nil
		})
}
