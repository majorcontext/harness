package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveExecutable resolves command[0] to an absolute path using exactly
// the rules a real spawn applies (see instance.dial and os/exec's Cmd.Path
// doc): a bare name (no path separator) is resolved via exec.LookPath —
// PATH lookup, independent of dir or the calling process's own working
// directory; a path containing a separator is resolved relative to dir
// (falling back to the calling process's cwd when dir is empty, matching
// Cmd.Dir's "empty means the calling process's current directory" rule) —
// never relative to the calling process's cwd once dir is set.
//
// Both the manifest-cache hash (see cmd/harness's pluginBinaryHash) and the
// actual process spawn call this exactly once, so the file that gets hashed
// and the file that gets executed can never diverge.
func ResolveExecutable(command []string, dir string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("plugin: empty command")
	}
	name := command[0]

	// filepath.Base(name) == name is exactly the test os/exec's Command
	// uses to decide whether to resolve name via LookPath.
	if filepath.Base(name) == name {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("resolving %q: %w", name, err)
		}
		return resolved, nil
	}

	if filepath.IsAbs(name) {
		return name, nil
	}

	base := dir
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = wd
	} else {
		abs, err := filepath.Abs(base)
		if err != nil {
			return "", err
		}
		base = abs
	}
	return filepath.Join(base, name), nil
}
