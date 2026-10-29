package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/majorcontext/harness/protocol"
)

// untrackedLargeCutoff matches the snapshot of opencode: an untracked file
// this large gets no hunk, so a request never diffs a large blob.
const untrackedLargeCutoff = 2 * 1024 * 1024

// untrackedEntries splits the output of ls-files --others into add -N
// excludes and large files. An entry that ends in "/" is a nested
// repository. Nested repositories and large files are never staged.
func untrackedEntries(repoRoot, lsFilesOut string) (excludeArgs, bigPaths []string) {
	for _, p := range splitNulZ(lsFilesOut) {
		if strings.HasSuffix(p, "/") {
			excludeArgs = append(excludeArgs, ":(exclude,literal)"+p)
			continue
		}
		info, err := os.Lstat(filepath.Join(repoRoot, p))
		if err != nil {
			continue
		}
		if info.Size() > untrackedLargeCutoff {
			bigPaths = append(bigPaths, p)
			excludeArgs = append(excludeArgs, ":(exclude,literal)"+p)
		}
	}
	return excludeArgs, bigPaths
}

// addUntrackedIntentToAdd stages every untracked path but the excludes as
// intent-to-add in one subprocess. core.splitIndex=false writes no shared
// index into the real repository. The pathspecs go over stdin, because
// many excludes would pass the argument-size limit of the OS.
func addUntrackedIntentToAdd(ctx context.Context, repoRoot string, env []string, excludeArgs []string) error {
	var stdin bytes.Buffer
	stdin.WriteString(".\x00")
	for _, a := range excludeArgs {
		stdin.WriteString(a)
		stdin.WriteByte(0)
	}
	cmd := gitCmdMagicPathspecs(ctx, repoRoot, env,
		"-c", "core.splitIndex=false", "add", "-N", "--pathspec-from-file=-", "--pathspec-file-nul", "--")
	cmd.Stdin = &stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git add -N: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// diffFilterDriverEnv neutralizes every clean and process filter driver of
// the repository. It returns GIT_CONFIG_COUNT/KEY/VALUE pairs, not -c
// arguments: git splits -c k=v at the first "=", so a driver named "x=y"
// would keep running.
func diffFilterDriverEnv(ctx context.Context, dir string, env []string) ([]string, error) {
	out, err := gitOutCapped(ctx, dir, env, filterDiscoveryCap, "config", "--null", "--name-only", "--get-regexp", `^filter\..*\.(clean|process)$`)
	if err != nil {
		if isGitWorkTreeErr(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, key := range splitNulZ(out) {
		rest, ok := strings.CutPrefix(key, "filter.")
		last := strings.LastIndex(rest, ".")
		if !ok || last < 0 {
			continue
		}
		name := rest[:last]
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var driverEnv []string
	n := 0
	set := func(key, value string) {
		driverEnv = append(driverEnv, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, key), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, value))
		n++
	}
	for _, name := range names {
		set("filter."+name+".clean", "")
		set("filter."+name+".process", "")
		set("filter."+name+".required", "false")
	}
	if n == 0 {
		return nil, nil
	}
	return append(driverEnv, "GIT_CONFIG_COUNT="+strconv.Itoa(n)), nil
}

// gitRealPath resolves gitPath, such as "index", with rev-parse --git-path,
// which also finds the files of a linked worktree.
func gitRealPath(ctx context.Context, repoRoot, gitPath string) (string, error) {
	out, err := gitOut(ctx, repoRoot, nil, "rev-parse", "--git-path", gitPath)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, p)
	}
	return p, nil
}

// gitQuotePathListEntry quotes path for GIT_ALTERNATE_OBJECT_DIRECTORIES,
// a colon-separated list, so a colon in path does not split it.
func gitQuotePathListEntry(path string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path)
	return `"` + escaped + `"`
}

// diffSource says where changeSet reads the repository. The zero gitDir
// means the repository that holds root. Otherwise gitDir is a bare git
// directory that root is the work tree of: git runs in a private git
// directory that reads the objects of gitDir and nothing else of it, and
// seed returns the tree-ish that the private index starts from and the diff
// runs against. tmp is the private directory.
type diffSource struct {
	root   string
	gitDir string
	// skip is a top-level name that never counts as untracked.
	skip string
	seed func(ctx context.Context, env []string, tmp string) (string, error)
}

// privateGitDir makes a git directory in dir that reads the objects of
// realObjects through an alternate and writes its own. No config of the
// user, the system, or the repository applies to it.
func privateGitDir(dir, realObjects string) (env []string, err error) {
	for _, d := range []string{"objects/info", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	files := map[string]string{
		"HEAD":                    "ref: refs/heads/main\n",
		"config":                  "[core]\n\trepositoryformatversion = 0\n\tattributesFile = " + os.DevNull + "\n",
		"objects/info/alternates": realObjects + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return nil, err
		}
	}
	return []string{"GIT_DIR=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, nil
}

// changeSet computes files and patch against base in a fixed number of
// subprocesses. Untracked files join as added through a private copy of the
// index, so the real index and object store are never written. files is
// always complete; patch stops at the last whole file within limit.
func changeSet(ctx context.Context, src diffSource, head, base string, limit int) (files []protocol.ChangedFile, patch string, truncated bool, err error) {
	tmp, err := os.MkdirTemp("", "harness-git-changes-")
	if err != nil {
		return nil, "", false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env, diffEnv, treeish, err := privateIndex(ctx, src, head, tmp)
	if err != nil {
		return nil, "", false, err
	}
	if src.seed != nil {
		base = treeish
	}
	repoRoot := src.root
	lsArgs := []string{"ls-files", "--others", "--exclude-standard", "-z"}
	if src.skip != "" {
		lsArgs = append(lsArgs, "--exclude=/"+src.skip)
	}
	lsFilesOut, err := gitOutCapped(ctx, repoRoot, env, metadataCap, lsArgs...)
	if err != nil {
		return nil, "", false, err
	}
	excludeArgs, bigPaths := untrackedEntries(repoRoot, lsFilesOut)
	if src.skip != "" {
		excludeArgs = append(excludeArgs, ":(exclude,literal)"+src.skip)
	}
	if err := addUntrackedIntentToAdd(ctx, repoRoot, env, excludeArgs); err != nil {
		return nil, "", false, err
	}
	if files, err = changedFiles(ctx, repoRoot, diffEnv, base, bigPaths); err != nil {
		return nil, "", false, err
	}
	patch, truncated, err = runPatchCapped(ctx, repoRoot, diffEnv, diffArgs("--no-color", "-M", base), limit)
	if err != nil {
		return nil, "", false, err
	}
	return files, patch, truncated, nil
}

// privateIndex builds the index in dir and returns the environment that
// points git at it: env for staging, and diffEnv, which also neutralizes
// every filter driver, for each command that hashes or diffs content.
// GIT_OBJECT_DIRECTORY keeps each object that add -N writes in dir, and
// GIT_ALTERNATE_OBJECT_DIRECTORIES still reads every real object. A git
// work tree copies its real index. A source with a seed runs in a private
// git directory (see privateGitDir), starts from an empty index that holds
// the seed tree, and returns that tree.
func privateIndex(ctx context.Context, src diffSource, head, dir string) (env, diffEnv []string, treeish string, err error) {
	repoRoot := src.root
	index, objects := filepath.Join(dir, "index"), filepath.Join(dir, "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		return nil, nil, "", err
	}
	missing := false
	if src.gitDir != "" {
		gitEnv, err := privateGitDir(filepath.Join(dir, "git"), filepath.Join(src.gitDir, "objects"))
		if err != nil {
			return nil, nil, "", err
		}
		env = append([]string{"GIT_INDEX_FILE=" + index, "GIT_WORK_TREE=" + repoRoot}, gitEnv...)
		diffEnv = env
	} else {
		realIndex, err := gitRealPath(ctx, repoRoot, "index")
		if err != nil {
			return nil, nil, "", err
		}
		if data, err := os.ReadFile(realIndex); err == nil {
			if err := os.WriteFile(index, data, 0o600); err != nil {
				return nil, nil, "", err
			}
		} else if os.IsNotExist(err) {
			missing = true
		} else {
			return nil, nil, "", err
		}
		realObjects, err := gitRealPath(ctx, repoRoot, "objects")
		if err != nil {
			return nil, nil, "", err
		}
		env = []string{"GIT_INDEX_FILE=" + index, "GIT_OBJECT_DIRECTORY=" + objects,
			"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + gitQuotePathListEntry(realObjects)}
		filterEnv, err := diffFilterDriverEnv(ctx, repoRoot, nil)
		if err != nil {
			return nil, nil, "", err
		}
		diffEnv = append(slices.Clone(env), filterEnv...)
	}
	switch {
	case src.seed != nil:
		if treeish, err = src.seed(ctx, diffEnv, dir); err != nil {
			return nil, nil, "", err
		}
		if _, err := gitOut(ctx, repoRoot, env, "-c", "core.splitIndex=false", "read-tree", treeish); err != nil {
			return nil, nil, "", err
		}
	case missing && head != "":
		// An empty index makes every tracked file of a commit look deleted, so
		// a missing index is seeded from HEAD, as git reset --mixed does.
		if _, err := gitOut(ctx, repoRoot, env, "-c", "core.splitIndex=false", "read-tree", "HEAD"); err != nil {
			return nil, nil, "", err
		}
	}
	_, err = gitOut(ctx, repoRoot, diffEnv, "-c", "core.splitIndex=false", "update-index", "-q", "--unmerged", "--refresh")
	return env, diffEnv, treeish, err
}

// diffArgs puts the global -c override before the diff subcommand and
// every diff option after it.
func diffArgs(rest ...string) []string {
	args := []string{"-c", "diff.autoRefreshIndex=false", "diff", "--no-ext-diff", "--no-textconv", "--submodule=short", "--ignore-submodules=dirty"}
	return append(args, rest...)
}

// changedFiles lists each file of the diff against base, then each large
// untracked file. A large path that the diff already holds, such as a
// large tracked file removed from the index, is not listed twice.
func changedFiles(ctx context.Context, repoRoot string, diffEnv []string, base string, bigPaths []string) ([]protocol.ChangedFile, error) {
	numstatOut, err := gitOutCapped(ctx, repoRoot, diffEnv, metadataCap, diffArgs("--numstat", "-z", "-M", base)...)
	if err != nil {
		return nil, err
	}
	nameStatusOut, err := gitOutCapped(ctx, repoRoot, diffEnv, metadataCap, diffArgs("--name-status", "-z", "-M", base)...)
	if err != nil {
		return nil, err
	}
	numstat := parseNumstatZ(numstatOut)
	files := []protocol.ChangedFile{}
	seen := make(map[string]bool, len(bigPaths))
	for _, e := range parseNameStatusZ(nameStatusOut) {
		ns := numstat[e.newPath]
		files = append(files, protocol.ChangedFile{Path: e.newPath, OldPath: e.oldPath, Status: statusWord(e.status),
			Additions: ns.additions, Deletions: ns.deletions, Binary: ns.binary})
		seen[e.newPath], seen[e.oldPath] = true, e.oldPath != ""
	}
	for _, p := range bigPaths {
		if _, err := os.Lstat(filepath.Join(repoRoot, p)); err != nil || seen[p] {
			continue
		}
		files = append(files, protocol.ChangedFile{Path: p, Status: "added", Large: true})
	}
	return files, nil
}

// diffGitMarker starts each file of a patch after the first.
var diffGitMarker = []byte("\ndiff --git ")

// runPatchCapped reads at most patchCap+len(diffGitMarker) bytes, so a file
// that ends exactly at patchCap still counts as whole. A longer patch kills
// git and cuts back to the last whole file within patchCap.
func runPatchCapped(ctx context.Context, dir string, extraEnv, args []string, patchCap int) (patch string, truncated bool, err error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", false, err
	}

	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(patchCap+len(diffGitMarker))))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), readErr)
	}

	if len(data) > patchCap {
		cut := lastWholeFileBoundary(data, patchCap)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return string(data[:cut]), true, nil
	}
	if err := cmd.Wait(); err != nil {
		return "", false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(data), false, nil
}

// lastWholeFileBoundary returns the length of the longest prefix of data
// within limit that ends at a diffGitMarker.
func lastWholeFileBoundary(data []byte, limit int) int {
	best := 0
	for i := 0; ; {
		idx := bytes.Index(data[i:], diffGitMarker)
		if idx < 0 {
			break
		}
		idx += i
		if cut := idx + 1; cut <= limit {
			best = cut
			i = idx + 1
		} else {
			break
		}
	}
	return best
}
