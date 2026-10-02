package gates

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"testing/fstest"
)

// Base is the tree at the merge base and the paths that differ from it.
type Base struct {
	Report  Report
	Changed map[string]bool
}

func git(root string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func nulPaths(out []byte) []string {
	var ps []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ps = append(ps, p)
		}
	}
	return ps
}

// LoadBase measures the merge base of HEAD and ref in the repository at root.
// Changed holds every path that differs from the merge base in the working
// tree, including untracked files.
func LoadBase(root, ref string) (Base, error) {
	out, err := git(root, "merge-base", "HEAD", ref)
	if err != nil {
		return Base{}, fmt.Errorf("find the merge base of HEAD and %s (fetch full history or set GATES_BASE_REF): %w", ref, err)
	}
	sha := strings.TrimSpace(string(out))
	tree, err := archiveFS(root, sha)
	if err != nil {
		return Base{}, err
	}
	report, err := Collect(tree)
	if err != nil {
		return Base{}, err
	}
	changed := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "--no-renames", sha},
		{"ls-files", "-z", "--others", "--exclude-standard"},
	} {
		if out, err = git(root, args...); err != nil {
			return Base{}, err
		}
		for _, p := range nulPaths(out) {
			changed[p] = true
		}
	}
	return Base{report, changed}, nil
}

func wanted(p string) bool {
	return strings.HasSuffix(p, ".go") || path.Base(p) == "AGENTS.md" || path.Base(p) == "go.mod"
}

func archiveFS(root, rev string) (fs.FS, error) {
	out, err := git(root, "archive", "--format=tar", rev)
	if err != nil {
		return nil, err
	}
	tree := fstest.MapFS{}
	tr := tar.NewReader(bytes.NewReader(out))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return tree, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || !wanted(h.Name) {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		tree[h.Name] = &fstest.MapFile{Data: data}
	}
}
