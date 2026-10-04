package prompt

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/skill"
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

// readOnly names the read-only file tools of the native and Claude Code
// backends. A spawn keeps the names that the child has.
var readOnly = []string{"read_file", "glob", "grep", "ls", "Read", "Glob", "Grep"}

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
// profile of its name. A file that is not valid, or that repeats the name of
// an earlier file, is skipped with a WARN log line.
func Profiles(dirs []string) map[string]Profile {
	out := map[string]Profile{GeneralPurpose: generalPurpose, explore.Name: explore, plan.Name: plan}
	seen := map[string]bool{}
	read := map[string]bool{}
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
			if err == nil && seen[p.Name] {
				err = errDuplicate
			}
			if err != nil {
				slog.Warn("prompt: agent profile skipped", "path", path, "err", err)
				continue
			}
			seen[p.Name] = true
			out[p.Name] = p
		}
	}
	return out
}

var (
	errDuplicate = errors.New("the name repeats another profile")
	errFields    = errors.New("name and description are required")
)

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
	if f["name"] == "" || f["description"] == "" {
		return Profile{}, errFields
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
