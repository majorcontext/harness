package builtin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	maxResults = 500
	// maxWalked bounds the entries that one glob or grep examines.
	maxWalked = 200000
	// maxGrepBytes skips a larger file. The cap binds on the bytes read.
	maxGrepBytes = 20 * 1024 * 1024
	// binarySniff is the prefix in which a NUL marks a file as binary.
	binarySniff = 64 * 1024
)

// skipDirs are never searched: version control and the harness state.
var skipDirs = map[string]bool{".git": true, ".harness": true}

var errStop = errors.New("grep: result cap reached")

// globRegexp matches a slash-separated relative path: * is a run of
// non-slash characters, ? is one, **/ is zero or more whole segments, and
// any other ** is any run of characters.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; {
		case c == '*' && i+2 < len(runes) && runes[i+1] == '*' && runes[i+2] == '/':
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && i+1 < len(runes) && runes[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// walk calls visit for each file under base. An unreadable entry is
// skipped, but an unreadable base fails the walk.
func walk(tool, base string, visit func(path, rel string, d fs.DirEntry) error) error {
	walked := 0
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil && p == base:
			return err
		case err != nil:
			return nil
		case p != base && d.IsDir() && skipDirs[d.Name()]:
			return filepath.SkipDir
		case d.IsDir():
			return nil
		}
		if walked++; walked > maxWalked {
			return fmt.Errorf("%s: exceeded %d files walked under %s", tool, maxWalked, base)
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return nil
		}
		return visit(p, filepath.ToSlash(rel), d)
	})
}

const globSchema = `{
	"type": "object",
	"properties": {
		"pattern": {"type": "string", "description": "Glob pattern, e.g. \"**/*.go\" or \"src/*.ts\""},
		"path": {"type": "string", "description": "Directory to search from (default: the working directory)"}
	},
	"required": ["pattern"]
}`

func (d dir) glob() tool {
	return newTool("glob", "Find files by name pattern. Supports * (any characters except /), ? (one character), and ** (any characters, including /, for recursive matching). Returns matching paths relative to the working directory, most-recently-modified first. Relative base paths resolve against the session working directory.",
		globSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Pattern == "" {
				return "", errors.New("glob: missing pattern argument")
			}
			base := d.base(in.Path)
			re, err := globRegexp(in.Pattern)
			if err != nil {
				return "", fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
			}
			if _, err := os.Stat(base); err != nil {
				return "", fmt.Errorf("glob: %w", err)
			}
			type match struct {
				rel string
				mod int64
			}
			var matches []match
			err = walk("glob", base, func(_, rel string, e fs.DirEntry) error {
				if re.MatchString(rel) {
					m := match{rel: rel}
					if info, err := e.Info(); err == nil {
						m.mod = info.ModTime().UnixNano()
					}
					matches = append(matches, m)
				}
				return nil
			})
			if err != nil {
				return "", err
			}
			slices.SortFunc(matches, func(a, b match) int {
				return cmp.Or(cmp.Compare(b.mod, a.mod), strings.Compare(a.rel, b.rel))
			})
			rels := make([]string, len(matches))
			for i, m := range matches {
				rels[i] = m.rel
			}
			return listed(rels, "(no matches)", "matches"), nil
		})
}

// listed joins at most maxResults lines and says when it dropped some.
func listed(lines []string, empty, noun string) string {
	if len(lines) == 0 {
		return empty
	}
	if len(lines) <= maxResults {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:maxResults], "\n") + fmt.Sprintf("\n[truncated: showing %d %s]", maxResults, noun)
}

const grepSchema = `{
	"type": "object",
	"properties": {
		"pattern": {"type": "string", "description": "Regular expression to search for (RE2 syntax)"},
		"path": {"type": "string", "description": "File or directory to search (default: the working directory)"},
		"glob": {"type": "string", "description": "Only search files whose relative path matches this glob pattern, e.g. \"**/*.go\""},
		"case_insensitive": {"type": "boolean", "description": "Match case-insensitively (default false)"}
	},
	"required": ["pattern"]
}`

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
}

func (d dir) grep() tool {
	return newTool("grep", "Search file contents for a regular expression (RE2 syntax). Returns matching lines as path:line:text. Relative base paths resolve against the session working directory.",
		grepSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in grepArgs
			if err := json.Unmarshal(args, &in); err != nil || in.Pattern == "" {
				return "", errors.New("grep: missing pattern argument")
			}
			pat := in.Pattern
			if in.CaseInsensitive {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return "", fmt.Errorf("grep: invalid pattern %q: %w", in.Pattern, err)
			}
			var include *regexp.Regexp
			if in.Glob != "" {
				if include, err = globRegexp(in.Glob); err != nil {
					return "", fmt.Errorf("grep: invalid glob %q: %w", in.Glob, err)
				}
			}
			base := d.base(in.Path)
			info, err := os.Stat(base)
			if err != nil {
				return "", fmt.Errorf("grep: %w", err)
			}
			g := &grepper{re: re, include: include}
			if !info.IsDir() {
				err = g.file(base, filepath.ToSlash(filepath.Base(base)))
			} else {
				err = walk("grep", base, func(p, rel string, _ fs.DirEntry) error { return g.file(p, rel) })
			}
			if err != nil && err != errStop {
				return "", err
			}
			if len(g.results) == 0 {
				return "(no matches)", nil
			}
			out := strings.Join(g.results, "\n")
			if g.truncated {
				out += fmt.Sprintf("\n[truncated: showing %d matches]", maxResults)
			}
			return out, nil
		})
}

type grepper struct {
	re, include *regexp.Regexp
	results     []string
	truncated   bool
}

// file skips a file that it cannot read, a file over maxGrepBytes, and a binary file.
func (g *grepper) file(path, rel string) error {
	if g.include != nil && !g.include.MatchString(rel) {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGrepBytes+1))
	_ = f.Close()
	if err != nil || len(data) > maxGrepBytes || slices.Contains(data[:min(len(data), binarySniff)], 0) {
		return nil
	}
	for i, line := range strings.Split(string(data), "\n") {
		if !g.re.MatchString(line) {
			continue
		}
		if len(g.results) >= maxResults {
			g.truncated = true
			return errStop
		}
		g.results = append(g.results, fmt.Sprintf("%s:%d:%s", rel, i+1, cutLine(line)))
	}
	return nil
}

const lsSchema = `{
	"type": "object",
	"properties": {
		"path": {"type": "string", "description": "Directory to list (default: the working directory)"}
	}
}`

func (d dir) ls() tool {
	return newTool("ls", "List a directory's immediate entries (not recursive), directories first, then files, both alphabetical. Relative paths resolve against the session working directory.",
		lsSchema, func(_ context.Context, args json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return "", fmt.Errorf("ls: invalid arguments: %w", err)
				}
			}
			entries, err := os.ReadDir(d.base(in.Path))
			if err != nil {
				return "", fmt.Errorf("ls: %w", err)
			}
			var dirs, files []string
			for _, e := range entries {
				if e.IsDir() {
					dirs = append(dirs, e.Name()+"/")
				} else {
					files = append(files, e.Name())
				}
			}
			slices.Sort(dirs)
			slices.Sort(files)
			return listed(append(dirs, files...), "(empty directory)", "entries"), nil
		})
}
