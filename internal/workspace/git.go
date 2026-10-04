package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// waitDelay bounds exec.Cmd.Wait, so a grandchild process that holds
// stdout or stderr open cannot block it.
const waitDelay = 2 * time.Second

// metadataCap bounds the stdout of ls-files, --numstat, and --name-status.
// Files must stay complete, so a larger change set is too_many_changes.
const metadataCap = 32 << 20

// filterDiscoveryCap bounds filter-driver discovery: each driver becomes
// three GIT_CONFIG_KEY/VALUE pairs in the environment of every diff, which
// shares ARG_MAX with argv.
const filterDiscoveryCap = 64 << 10

// gitStaticSafetyArgs turn off the fsmonitor hook and implicit bare
// repositories, and point core.hooksPath at no hooks: add -N runs the
// post-index-change hook of the repository otherwise.
var gitStaticSafetyArgs = []string{
	"-c", "core.fsmonitor=false",
	"-c", "safe.bareRepository=explicit",
	"-c", "core.hooksPath=" + os.DevNull,
}

// gitStrippedEnv is never inherited. The first group, from rev-parse
// --local-env-vars, selects another repository, index, or config; a git
// hook exports GIT_DIR. The second group changes pathspec parsing.
var gitStrippedEnv = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_CONFIG": true,
	"GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_DIR": true, "GIT_WORK_TREE": true,
	"GIT_IMPLICIT_WORK_TREE": true, "GIT_GRAFT_FILE": true,
	"GIT_INDEX_FILE": true, "GIT_NO_REPLACE_OBJECTS": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_PREFIX": true,
	"GIT_INTERNAL_SUPER_PREFIX": true, "GIT_SHALLOW_FILE": true,
	"GIT_COMMON_DIR": true,

	"GIT_LITERAL_PATHSPECS": true, "GIT_GLOB_PATHSPECS": true,
	"GIT_NOGLOB_PATHSPECS": true, "GIT_ICASE_PATHSPECS": true,
}

// gitBaseEnv is os.Environ() without gitStrippedEnv.
func gitBaseEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !gitStrippedEnv[name] {
			out = append(out, kv)
		}
	}
	return out
}

// gitCmd builds a git subprocess bounded by ctx. GIT_LITERAL_PATHSPECS=1
// keeps a file name such as "b*.txt" from becoming a glob.
func gitCmd(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	args = append(append([]string{}, gitStaticSafetyArgs...), args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(gitBaseEnv(),
		"GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_NO_LAZY_FETCH=1"), extraEnv...)
	cmd.WaitDelay = waitDelay
	return cmd
}

// gitCmdMagicPathspecs is gitCmd without GIT_LITERAL_PATHSPECS, for the
// add -N call, whose ":(exclude,literal)" magic keeps each path exact.
func gitCmdMagicPathspecs(ctx context.Context, dir string, extraEnv []string, args ...string) *exec.Cmd {
	args = append(append([]string{}, gitStaticSafetyArgs...), args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(gitBaseEnv(), "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1"), extraEnv...)
	cmd.WaitDelay = waitDelay
	return cmd
}

func gitOut(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// errMetadataTooLarge reports a gitOutCapped overflow. Metadata must stay
// complete, so an overflow is too_many_changes, never a cut.
var errMetadataTooLarge = errors.New("git metadata exceeded its size bound")

// gitOutCapped is gitOut that reads at most maxBytes of stdout.
func gitOutCapped(ctx context.Context, dir string, extraEnv []string, maxBytes int, args ...string) (string, error) {
	cmd := gitCmd(ctx, dir, extraEnv, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	if readErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), readErr)
	}
	if len(data) > maxBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return "", errMetadataTooLarge
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(data), nil
}

// isGitWorkTreeErr reports a non-zero git exit, as opposed to a failed start.
func isGitWorkTreeErr(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

// repoRootAt returns the work tree root of dir below ceiling, or ok false
// when dir is in no work tree.
func repoRootAt(ctx context.Context, dir, ceiling string) (root string, ok bool, err error) {
	var env []string
	if ceiling != "" {
		env = []string{"GIT_CEILING_DIRECTORIES=" + ceiling}
	}
	out, err := gitOut(ctx, dir, env, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() == nil && isGitWorkTreeErr(err) {
			return "", false, nil
		}
		return "", false, err
	}
	// A directory name can end in a space, so only the newline goes.
	return strings.TrimSuffix(out, "\n"), true, nil
}

// emptyTree returns the empty tree of the hash algorithm of the repository.
func emptyTree(ctx context.Context, dir string) (string, error) {
	out, err := gitOut(ctx, dir, nil, "hash-object", "-t", "tree", os.DevNull)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// defaultBranchRef returns the first of origin/HEAD, origin/main, and
// origin/master that resolves, so a stale symref falls through.
func defaultBranchRef(ctx context.Context, dir string) (display, revision string, found bool) {
	if out, err := gitOut(ctx, dir, nil, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		full := strings.TrimSpace(out)
		if _, verr := gitOut(ctx, dir, nil, "rev-parse", "--verify", "-q", full); verr == nil {
			return strings.TrimPrefix(full, "refs/remotes/"), full, true
		}
	}
	for _, name := range []string{"main", "master"} {
		ref := "refs/remotes/origin/" + name
		if _, err := gitOut(ctx, dir, nil, "rev-parse", "--verify", "-q", ref); err == nil {
			return "origin/" + name, ref, true
		}
	}
	return "", "", false
}

type diffNumstat struct {
	additions int
	deletions int
	binary    bool
}

func splitNulZ(s string) []string {
	s = strings.TrimSuffix(s, "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// parseNumstatZ parses `git diff --numstat -z`, keyed by each file's new
// path. A rename or copy prints an empty path field, then old and new.
func parseNumstatZ(out string) map[string]diffNumstat {
	fields := splitNulZ(out)
	result := make(map[string]diffNumstat, len(fields))
	i := 0
	for i < len(fields) {
		head := fields[i]
		i++
		parts := strings.SplitN(head, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		addStr, delStr, path := parts[0], parts[1], parts[2]
		if path == "" {
			if i+1 >= len(fields) {
				break
			}
			i++
			path = fields[i]
			i++
		}
		binary := addStr == "-" || delStr == "-"
		add, _ := strconv.Atoi(addStr)
		del, _ := strconv.Atoi(delStr)
		result[path] = diffNumstat{additions: add, deletions: del, binary: binary}
	}
	return result
}

type nameStatusEntry struct {
	status  string
	oldPath string
	newPath string
}

func parseNameStatusZ(out string) []nameStatusEntry {
	fields := splitNulZ(out)
	var entries []nameStatusEntry
	i := 0
	for i < len(fields) {
		status := fields[i]
		i++
		if i >= len(fields) || status == "" {
			break
		}
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 >= len(fields) {
				break
			}
			old := fields[i]
			i++
			newPath := fields[i]
			i++
			entries = append(entries, nameStatusEntry{status: status[:1], oldPath: old, newPath: newPath})
			continue
		}
		path := fields[i]
		i++
		entries = append(entries, nameStatusEntry{status: status[:1], newPath: path})
	}
	return entries
}

// statusWord maps a name-status letter to the contract's status word; an
// undocumented letter (C, T) falls back to "modified".
func statusWord(letter string) string {
	switch letter {
	case "A":
		return "added"
	case "D":
		return "deleted"
	case "R":
		return "renamed"
	default:
		return "modified"
	}
}
