// Package guard is a plugin that blocks a few dangerous tool calls: a force
// push, rm -rf on a broad target, and a read_file call on a .env file.
//
// The same Plugin value runs in the host process through
// harness.Options.Plugins and as a plugin process through plugin.Serve.
//
// The guard reads the text of a command. It is not a sandbox: a model can
// write the same action in a form that the guard does not see, such as a
// script file or a command that a variable builds.
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/majorcontext/harness/plugin"
)

// Plugin is the guardrail plugin. DenyOnFailure makes the host deny a call
// when the guard itself fails, so a broken guard never lets a call through.
var Plugin = &plugin.Plugin{
	Name:              "guardrail",
	ToolExecuteBefore: &plugin.ToolBefore{DenyOnFailure: true, Run: check},
}

func check(_ context.Context, _ *plugin.Client, req *plugin.ToolExecuteBeforeRequest) (*plugin.ToolExecuteBeforeResponse, error) {
	reason, err := verdict(req.Tool, req.Args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Tool, err)
	}
	if reason == "" {
		return nil, nil
	}
	return &plugin.ToolExecuteBeforeResponse{Deny: reason}, nil
}

// verdict returns the deny text for a call, or "" when the call may run.
func verdict(tool string, args json.RawMessage) (string, error) {
	switch tool {
	case "bash":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return checkCommand(in.Command), nil
	case "read_file":
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		if isEnvFile(in.Path) {
			return ".env files are blocked", nil
		}
	}
	return "", nil
}

var separators = regexp.MustCompile(`[;&|\n]+`)

func checkCommand(command string) string {
	for _, segment := range separators.Split(command, -1) {
		words := strings.Fields(strings.NewReplacer(`"`, "", `'`, "").Replace(segment))
		if len(words) > 0 && words[0] == "sudo" {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		switch {
		case words[0] == "git" && isForcePush(words[1:]):
			return "force push is blocked"
		case words[0] == "rm" && isBroadRemove(words[1:]):
			return "rm -rf on a broad target is blocked"
		}
	}
	return ""
}

// isForcePush reports whether words, the words after git, are a push that
// forces: push must be the subcommand, the first word that is not an option
// or the value of -C or -c.
func isForcePush(words []string) bool {
	i := 0
	for ; i < len(words); i++ {
		switch w := words[i]; {
		case w == "-C" || w == "-c":
			i++
		case !strings.HasPrefix(w, "-"):
			return words[i] == "push" && forces(words[i+1:])
		}
	}
	return false
}

func forces(words []string) bool {
	for _, w := range words {
		switch {
		case w == "--force", w == "--force-with-lease", strings.HasPrefix(w, "--force-with-lease="):
			return true
		case hasShortFlag(w, 'f'), len(w) > 1 && w[0] == '+':
			return true
		}
	}
	return false
}

func isBroadRemove(words []string) bool {
	var recursive, force bool
	var targets []string
	for _, w := range words {
		switch {
		case w == "--recursive":
			recursive = true
		case w == "--force":
			force = true
		case hasShortFlag(w, 'r') || hasShortFlag(w, 'R'):
			recursive = true
			force = force || hasShortFlag(w, 'f')
		case hasShortFlag(w, 'f'):
			force = true
		case !strings.HasPrefix(w, "-"):
			targets = append(targets, w)
		}
	}
	if !recursive || !force {
		return false
	}
	for _, t := range targets {
		if isBroadTarget(t) {
			return true
		}
	}
	return false
}

// hasShortFlag reports whether w is a cluster of short flags, such as -fr,
// that holds flag.
func hasShortFlag(w string, flag byte) bool {
	return len(w) > 1 && w[0] == '-' && w[1] != '-' && strings.IndexByte(w, flag) > 0
}

func isBroadTarget(t string) bool {
	t = path.Clean(t)
	if t == "*" {
		return true
	}
	if path.Base(t) == "*" {
		t = path.Dir(t)
	}
	switch t {
	case "/", "~", "$HOME", "${HOME}", ".", "..":
		return true
	}
	return false
}

func isEnvFile(p string) bool {
	base := path.Base(strings.ReplaceAll(p, `\`, "/"))
	return base == ".env" || strings.HasPrefix(base, ".env.")
}
