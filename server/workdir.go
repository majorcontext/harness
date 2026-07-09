package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveWorkDir validates POST /session's optional workdir. An empty input
// defaults to the server process's own current working directory — always
// trusted, never checked against roots. A non-empty input must clean-resolve
// (made absolute, then filepath.Clean'd, so ".." segments cannot escape by
// string trickery) to a path equal to, or nested under, one of roots; an
// empty roots list falls back to the process cwd as the sole implicit root.
// Each configured root is absolutized (relative to the server process's cwd)
// the same way, so a relative -workspace-root value works as documented
// instead of silently rejecting every workdir under it.
func resolveWorkDir(roots []string, input string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if input == "" {
		return cwd, nil
	}
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	effective := roots
	if len(effective) == 0 {
		effective = []string{cwd}
	}
	for _, r := range effective {
		rabs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		rc := filepath.Clean(rabs)
		if abs == rc || strings.HasPrefix(abs, rc+string(os.PathSeparator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("workdir %q is not under an allowed workspace root", input)
}
