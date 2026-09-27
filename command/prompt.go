package command

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/majorcontext/harness/skill"
)

// PromptCommand holds validated command metadata. LoadBody reads the body only
// when a caller executes the command.
type PromptCommand struct {
	Name        string
	Description string
	ArgHint     string
	Path        string
	root        string
}

// LoadBody reads the command body without its frontmatter.
func (c *PromptCommand) LoadBody() (string, error) {
	info, err := validatePromptPath(c.root, c.Path, c.Name)
	if err != nil {
		return "", err
	}
	file, err := os.Open(c.Path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return "", fmt.Errorf("command file %q changed while loading", c.Path)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s: command file is not valid UTF-8", c.Path)
	}
	_, body, err := skill.SplitFrontmatter(string(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", c.Path, err)
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("%s: command body is empty", c.Path)
	}
	return body, nil
}

// Discover reads command metadata from Markdown files in dirs. Directories
// have increasing precedence: a later directory shadows an earlier one.
// Results are sorted by command name. It does not read command bodies.
func Discover(dirs []string) ([]*PromptCommand, error) {
	commands := make(map[string]*PromptCommand)
	for _, dir := range dirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("command directory %q is not a directory", root)
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, err
		}
		if canonical != root {
			return nil, fmt.Errorf("command directory %q must not contain symlinks", root)
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("command path %q must not contain symlinks", path)
			}
			if entry.IsDir() {
				return nil
			}
			if filepath.Ext(entry.Name()) != ".md" {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("command file %q is not a regular file", path)
			}
			name, err := promptName(root, path)
			if err != nil {
				return err
			}
			if err := checkPromptName(name); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if builtinName(name) {
				return fmt.Errorf("%s: prompt command name %q conflicts with a builtin", path, name)
			}
			prompt, err := loadPromptMetadata(path, name)
			if err != nil {
				return err
			}
			prompt.root = root
			if previous := commands[name]; previous != nil {
				slog.Warn("prompt command shadowed", "name", name, "previous", previous.Path, "winner", path)
			}
			commands[name] = prompt
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]*PromptCommand, 0, len(commands))
	for _, prompt := range commands {
		out = append(out, prompt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LookupPrompt reads one named command from dirs. A later directory shadows
// an earlier one. It returns nil without an error for invalid names or misses.
func LookupPrompt(dirs []string, name string) (*PromptCommand, error) {
	if err := checkPromptName(name); err != nil {
		return nil, nil
	}
	if builtinName(name) {
		return nil, fmt.Errorf("prompt command name %q conflicts with a builtin", name)
	}
	file := filepath.FromSlash(strings.ReplaceAll(name, ":", "/")) + ".md"
	var found *PromptCommand
	for _, dir := range dirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if canonical != root {
			return nil, fmt.Errorf("command directory %q must not contain symlinks", root)
		}
		path := filepath.Join(root, file)
		if _, err := validatePromptPath(root, path, name); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		found, err = loadPromptMetadata(path, name)
		if err != nil {
			return nil, err
		}
		found.root = root
	}
	return found, nil
}

// Expand substitutes $ARGUMENTS with args and $1 through $9 with positional
// arguments split on whitespace. Other dollar expressions remain unchanged.
func Expand(body string, args string) string {
	fields := strings.Fields(args)
	var out strings.Builder
	for i := 0; i < len(body); {
		if body[i] != '$' {
			out.WriteByte(body[i])
			i++
			continue
		}
		name, index, ok := argumentPlaceholder(body[i:])
		if !ok {
			out.WriteByte(body[i])
			i++
			continue
		}
		end := i + len(name)
		if end < len(body) {
			r, _ := utf8.DecodeRuneInString(body[end:])
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				out.WriteString(name)
				i = end
				continue
			}
		}
		if index == 0 {
			out.WriteString(args)
		} else if index <= len(fields) {
			out.WriteString(fields[index-1])
		}
		i = end
	}
	return out.String()
}

func argumentPlaceholder(text string) (name string, index int, ok bool) {
	if strings.HasPrefix(text, "$ARGUMENTS") {
		return "$ARGUMENTS", 0, true
	}
	if len(text) >= 2 && text[0] == '$' && text[1] >= '1' && text[1] <= '9' {
		return text[:2], int(text[1] - '0'), true
	}
	return "", 0, false
}

func validatePromptPath(root, path, name string) (os.FileInfo, error) {
	if err := checkPromptName(name); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	expected := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(name, ":", "/"))+".md")
	if filepath.Clean(path) != expected {
		return nil, fmt.Errorf("command path %q does not match name %q", path, name)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, fmt.Errorf("command directory %q is not a regular directory", root)
	}
	rel, err := filepath.Rel(root, expected)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	current := root
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("command path %q must not contain symlinks", current)
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("command file %q is not a regular file", current)
			}
			return info, nil
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("command path %q is not a directory", current)
		}
	}
	return nil, fmt.Errorf("command path %q is invalid", path)
}

func loadPromptMetadata(path, name string) (*PromptCommand, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	first, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, err
	}
	if strings.TrimRight(first, "\r\n \t") != "---" {
		return nil, fmt.Errorf("%s: missing frontmatter: file must begin with a '---' delimiter line", path)
	}
	var frontmatter strings.Builder
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if strings.TrimRight(line, "\r\n \t") == "---" {
			break
		}
		if readErr == io.EOF {
			return nil, fmt.Errorf("%s: unterminated frontmatter: no closing '---' delimiter found", path)
		}
		frontmatter.WriteString(line)
	}
	if !utf8.ValidString(frontmatter.String()) {
		return nil, fmt.Errorf("%s: frontmatter is not valid UTF-8", path)
	}
	fields, err := skill.ParseFrontmatterFields(frontmatter.String(), "description", "argument-hint")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	description := fields["description"]
	if description == "" {
		return nil, fmt.Errorf("%s: missing or empty required field: description", path)
	}
	if length := utf8.RuneCountInString(description); length > 1024 {
		return nil, fmt.Errorf("%s: description is %d chars, exceeds maximum of 1024", path, length)
	}
	return &PromptCommand{
		Name:        name,
		Description: description,
		ArgHint:     fields["argument-hint"],
		Path:        path,
	}, nil
}

func promptName(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(filepath.ToSlash(rel), filepath.Ext(rel))
	return strings.ReplaceAll(name, "/", ":"), nil
}

func checkPromptName(name string) error {
	if name == "" {
		return errors.New("prompt command name is empty")
	}
	for _, segment := range strings.Split(name, ":") {
		if segment == "" || strings.HasPrefix(segment, "-") || strings.HasSuffix(segment, "-") || strings.Contains(segment, "--") {
			return fmt.Errorf("invalid prompt command name %q", name)
		}
		for _, r := range segment {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
				return fmt.Errorf("invalid prompt command name %q", name)
			}
		}
	}
	return nil
}

func builtinName(name string) bool {
	_, ok := NewRegistry().Lookup(name)
	return ok
}
