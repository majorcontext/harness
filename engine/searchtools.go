package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

// glob, grep, and ls are minimal, read-only search tools. The
// subagent-sessions design doc (docs/plans/2026-08-23-subagent-sessions-design.md)
// names tools with these exact names in its read-only agent presets
// (explore, plan — see agentdef.go), as if mapping onto an existing
// registry entry. None existed before this file: the only prior search
// capability was the unrestricted bash tool, which cannot back a preset
// that must actually be read-only. These three fill that gap.
//
// They follow the conventions read_file/write_file/edit_file already
// established (filetools.go): resolvePath for path arguments (relative to
// Config.WorkDir; an absolute path passes through unchanged — the same
// trust model as every other built-in tool, no separate sandbox), and a
// truncation marker rather than a silent cut-off when a result is capped.
// They are deliberately minimal — no full shell-glob/regex feature parity
// with any particular shell or grep implementation is intended.

const (
	// maxSearchResults bounds how many matches glob/grep return before
	// truncating, mirroring read_file's readFileDefaultLimit-style cap: a
	// giant match set otherwise floods the session with a single tool
	// result.
	maxSearchResults = 500
	// maxWalkedFiles bounds how many filesystem entries glob/grep will
	// examine in one call, independent of how many actually match — the
	// backstop against an accidental repo-root sweep of a huge tree taking
	// unbounded wall-clock time.
	maxWalkedFiles = 200000
	// maxGrepFileBytes bounds how large a single file grep will read
	// before searching it. A default (no path) search can walk into an
	// unexpectedly huge file (a build artifact, a core dump, a database
	// file) and exhaust process memory reading it whole — the same risk
	// read_file guards with readFileMaxImageBytes/io.LimitReader; grep's
	// searchFile enforces this bound the identical way, via
	// io.LimitReader(f, maxGrepFileBytes+1) over one open handle, never
	// against a separately captured os.Stat size a concurrently growing
	// file (a live log, a build artifact still being written) could
	// outrun between the stat and a later read — the exact TOCTOU shape
	// AGENTS.md's read_file guidance forbids, and an earlier revision of
	// this file used. A file over the cap is skipped entirely — grep's
	// job is finding SMALL, textual matches, not partially searching one
	// giant file, so skipping is the right trade, not a truncated read.
	maxGrepFileBytes = 20 * 1024 * 1024
)

// skippedSearchDirs names directories glob/grep never descend into,
// regardless of pattern — version-control internals are never a match a
// caller wants and are often enormous.
var skippedSearchDirs = map[string]bool{
	".git": true,
}

// globToRegexp translates a shell-glob-ish pattern into an anchored regexp
// matched against a slash-separated relative path. Supported: "*" (any
// run of non-slash characters), "?" (one non-slash character), "**/" (zero
// or more whole path segments — so "**/*.go" matches both "main.go" and
// "pkg/sub/util.go", the conventional globstar meaning), and a bare "**"
// anywhere else (any run of characters, including slashes). Every other
// regexp metacharacter in the pattern is escaped literally.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch c := runes[i]; c {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				if i+2 < len(runes) && runes[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
					continue
				}
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func globTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "glob",
			Description: "Find files by name pattern. Supports * (any characters except /), ? (one character), and ** (any characters, including /, for recursive matching). Returns matching paths relative to the working directory, most-recently-modified first. Relative base paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Glob pattern, e.g. \"**/*.go\" or \"src/*.ts\""},
					"path": {"type": "string", "description": "Directory to search from (default: the working directory)"}
				},
				"required": ["pattern"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Pattern == "" {
				return nil, fmt.Errorf("glob: missing pattern argument")
			}
			base := s.cfg.WorkDir
			if in.Path != "" {
				base = s.resolvePath(in.Path)
			}
			re, err := globToRegexp(in.Pattern)
			if err != nil {
				return nil, fmt.Errorf("glob: invalid pattern %q: %w", in.Pattern, err)
			}

			type match struct {
				rel     string
				modTime int64
			}
			var matches []match
			walked := 0
			err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil // unreadable entry: skip it, don't fail the whole search
				}
				if p != base && d.IsDir() && skippedSearchDirs[d.Name()] {
					return filepath.SkipDir
				}
				if d.IsDir() {
					return nil
				}
				walked++
				if walked > maxWalkedFiles {
					return fmt.Errorf("glob: exceeded %d files walked under %s", maxWalkedFiles, base)
				}
				rel, err := filepath.Rel(base, p)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				if !re.MatchString(rel) {
					return nil
				}
				info, err := d.Info()
				var mt int64
				if err == nil {
					mt = info.ModTime().UnixNano()
				}
				matches = append(matches, match{rel: rel, modTime: mt})
				return nil
			})
			if err != nil {
				return nil, err
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].modTime > matches[j].modTime })

			if len(matches) == 0 {
				return message.Parts{&message.Text{Text: "(no matches)"}}, nil
			}
			truncated := len(matches) > maxSearchResults
			if truncated {
				matches = matches[:maxSearchResults]
			}
			lines := make([]string, len(matches))
			for i, m := range matches {
				lines[i] = m.rel
			}
			out := strings.Join(lines, "\n")
			if truncated {
				out += fmt.Sprintf("\n[truncated: showing %d matches]", maxSearchResults)
			}
			return message.Parts{&message.Text{Text: out}}, nil
		},
	}
}

func grepTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "grep",
			Description: "Search file contents for a regular expression (RE2 syntax). Returns matching lines as path:line:text. Relative base paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Regular expression to search for (RE2 syntax)"},
					"path": {"type": "string", "description": "File or directory to search (default: the working directory)"},
					"glob": {"type": "string", "description": "Only search files whose relative path matches this glob pattern, e.g. \"**/*.go\""},
					"case_insensitive": {"type": "boolean", "description": "Match case-insensitively (default false)"}
				},
				"required": ["pattern"]
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Pattern         string `json:"pattern"`
				Path            string `json:"path"`
				Glob            string `json:"glob"`
				CaseInsensitive bool   `json:"case_insensitive"`
			}
			if err := json.Unmarshal(args, &in); err != nil || in.Pattern == "" {
				return nil, fmt.Errorf("grep: missing pattern argument")
			}
			pat := in.Pattern
			if in.CaseInsensitive {
				pat = "(?i)" + pat
			}
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("grep: invalid pattern %q: %w", in.Pattern, err)
			}
			var includeRe *regexp.Regexp
			if in.Glob != "" {
				includeRe, err = globToRegexp(in.Glob)
				if err != nil {
					return nil, fmt.Errorf("grep: invalid glob %q: %w", in.Glob, err)
				}
			}

			base := s.cfg.WorkDir
			if in.Path != "" {
				base = s.resolvePath(in.Path)
			}
			info, err := os.Stat(base)
			if err != nil {
				return nil, fmt.Errorf("grep: %w", err)
			}

			var results []string
			truncated := false
			walked := 0
			searchFile := func(path, rel string) error {
				if includeRe != nil && !includeRe.MatchString(rel) {
					return nil
				}
				f, err := os.Open(path)
				if err != nil {
					return nil // unreadable file: skip it
				}
				data, err := io.ReadAll(io.LimitReader(f, maxGrepFileBytes+1))
				f.Close()
				if err != nil {
					return nil // read error mid-file: skip it
				}
				if len(data) > maxGrepFileBytes {
					return nil // too large to safely read whole — skip it
				}
				if looksBinary(data) {
					return nil
				}
				for i, line := range strings.Split(string(data), "\n") {
					if len(results) >= maxSearchResults {
						truncated = true
						return errStopWalk
					}
					if re.MatchString(line) {
						if r := []rune(line); len(r) > readFileMaxLineLen {
							line = string(r[:readFileMaxLineLen]) + "…"
						}
						results = append(results, fmt.Sprintf("%s:%d:%s", rel, i+1, line))
					}
				}
				return nil
			}

			if !info.IsDir() {
				rel := filepath.ToSlash(filepath.Base(base))
				if err := searchFile(base, rel); err != nil && err != errStopWalk {
					return nil, err
				}
			} else {
				err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if p != base && d.IsDir() && skippedSearchDirs[d.Name()] {
						return filepath.SkipDir
					}
					if d.IsDir() {
						return nil
					}
					walked++
					if walked > maxWalkedFiles {
						return fmt.Errorf("grep: exceeded %d files walked under %s", maxWalkedFiles, base)
					}
					rel, err := filepath.Rel(base, p)
					if err != nil {
						return nil
					}
					return searchFile(p, filepath.ToSlash(rel))
				})
				if err != nil && err != errStopWalk {
					return nil, err
				}
			}

			if len(results) == 0 {
				return message.Parts{&message.Text{Text: "(no matches)"}}, nil
			}
			out := strings.Join(results, "\n")
			if truncated {
				out += fmt.Sprintf("\n[truncated: showing %d matches]", maxSearchResults)
			}
			return message.Parts{&message.Text{Text: out}}, nil
		},
	}
}

// errStopWalk unwinds filepath.WalkDir early once a search has already
// collected maxSearchResults matches, without treating the early stop as a
// tool error.
var errStopWalk = fmt.Errorf("grep: result cap reached")

// looksBinary reports whether data appears to be non-text content, using
// the same "contains a NUL byte in the first sniffed chunk" heuristic Git
// itself uses to classify a file as binary — cheap, and right often enough
// to keep grep from dumping garbage into a tool result.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > imageSniffLen {
		n = imageSniffLen
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func lsTool() Tool {
	return Tool{
		Def: provider.ToolDef{
			Name:        "ls",
			Description: "List a directory's immediate entries (not recursive), directories first, then files, both alphabetical. Relative paths resolve against the session working directory.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Directory to list (default: the working directory)"}
				}
			}`),
		},
		Run: func(ctx context.Context, s *Session, args json.RawMessage) (message.Parts, error) {
			var in struct {
				Path string `json:"path"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, fmt.Errorf("ls: invalid arguments: %w", err)
				}
			}
			path := s.cfg.WorkDir
			if in.Path != "" {
				path = s.resolvePath(in.Path)
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("ls: %w", err)
			}
			var dirs, files []string
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					dirs = append(dirs, name+"/")
				} else {
					files = append(files, name)
				}
			}
			sort.Strings(dirs)
			sort.Strings(files)
			all := append(dirs, files...)
			if len(all) == 0 {
				return message.Parts{&message.Text{Text: "(empty directory)"}}, nil
			}
			return message.Parts{&message.Text{Text: strings.Join(all, "\n")}}, nil
		},
	}
}
