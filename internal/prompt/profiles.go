package prompt

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/internal/skill"
)

// GeneralPurpose names the built-in profile, which allows every tool.
const GeneralPurpose = "general-purpose"

// Profile is an agent profile: a kind of child session that the task tool starts.
type Profile struct {
	Name        string
	Description string
	// Tools names the tools of the child. nil allows every tool of the parent.
	Tools []string
	// Model is the model of the child. Empty: the model of the parent.
	Model string
	// Prompt follows the system prompt of the child.
	Prompt string
}

var generalPurpose = Profile{Name: GeneralPurpose,
	Description: "A general agent for complex, multi-step tasks, with every tool of its parent.",
	Prompt: "You are a child agent. A parent agent gave you the task in your first message with its task tool. " +
		"Do the whole task with your tools. Your final message is the only thing the parent reads, " +
		"so make it a complete, concise report of what you found or changed."}

// readOnly names the read-only tools of the native and Claude Code backends:
// the file tools and session_info. A spawn keeps the names that the child has.
var readOnly = []string{"read_file", "glob", "grep", "ls", "session_info", "Read", "Glob", "Grep"}

var explore = Profile{Name: "explore", Tools: readOnly,
	Description: "A fast, read-only agent that finds code and answers where-is questions. It cannot edit files or run commands.",
	Prompt:      generalPurpose.Prompt}

var plan = Profile{Name: "plan", Tools: readOnly,
	Description: "A read-only agent that investigates the code and returns an implementation plan. It makes no edits.",
	Prompt: generalPurpose.Prompt + "\n\nInvestigate with your read-only tools, then give a clear, concrete implementation plan as your final message. " +
		"You have no tool that edits a file or runs a command, so make no change."}

// profileKeys are the agent frontmatter keys of Claude Code that a profile reads or ignores.
var profileKeys = []string{"name", "description", "tools", "model", "color"}

// Profiles returns the built-in profiles and each valid *.md file of dirs in
// the agent format of Claude Code, by name. A file replaces a built-in
// profile of its name. A file with a key that the format does not know is
// skipped with a WARN log line. Any other file that is not valid, and a file
// that repeats the name of an earlier file, in one directory or across dirs,
// is left out, and the load returns the profiles of the other files with the
// error of the first such file; a repeat names both files.
func Profiles(dirs []string) (map[string]Profile, error) {
	out := map[string]Profile{GeneralPurpose: generalPurpose, explore.Name: explore, plan.Name: plan}
	source := map[string]string{}
	read := map[string]bool{}
	var failed error
	for _, dir := range dirs {
		if dir = filepath.Clean(dir); read[dir] {
			continue
		}
		read[dir] = true
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
				continue
			}
			path := filepath.Join(dir, e.Name())
			p, err := profile(path)
			switch {
			case errors.Is(err, skill.ErrUnknownKey):
				slog.Warn("prompt: agent profile skipped", "path", path, "err", err)
				continue
			case err != nil:
				if failed == nil {
					failed = fmt.Errorf("agent definition %s: %w", path, err)
				}
				continue
			}
			if first, ok := source[p.Name]; ok {
				if failed == nil {
					failed = fmt.Errorf("agent definition %s: name %q already defined in %s", path, p.Name, first)
				}
				continue
			}
			source[p.Name] = path
			out[p.Name] = p
		}
	}
	return out, failed
}

func profile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	fm, body, err := skill.SplitFrontmatter(string(data))
	if err != nil {
		return Profile{}, err
	}
	f, err := skill.ParseFrontmatterFields(fm, profileKeys...)
	if err != nil {
		return Profile{}, err
	}
	for _, key := range []string{"name", "description"} {
		if f[key] == "" {
			return Profile{}, fmt.Errorf("frontmatter missing required '%s'", key)
		}
	}
	p := Profile{Name: f["name"], Description: f["description"], Prompt: strings.TrimSpace(body)}
	if f["model"] != "inherit" {
		p.Model = f["model"]
	}
	for t := range strings.SplitSeq(f["tools"], ",") {
		if t = strings.TrimSpace(t); t != "" {
			p.Tools = append(p.Tools, t)
		}
	}
	return p, nil
}
