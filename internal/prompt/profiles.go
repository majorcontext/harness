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

// profileKeys are the agent frontmatter keys of Claude Code that a profile reads or ignores.
var profileKeys = []string{"name", "description", "tools", "model", "color"}

// Profiles returns the built-in profile and each valid <workDir>/.agents/*.md
// in the agent format of Claude Code, by name. A file replaces a built-in
// profile of its name. A file that is not valid, or that repeats a name, is
// skipped with a WARN log line.
func Profiles(workDir string) map[string]Profile {
	out := map[string]Profile{GeneralPurpose: generalPurpose}
	if workDir == "" {
		return out
	}
	dir := filepath.Join(workDir, ".agents")
	entries, _ := os.ReadDir(dir)
	seen := map[string]bool{}
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
