package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	mathrand "math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

func newGitChangesHarness(t *testing.T, root string) *harness {
	t.Helper()
	return newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{root})
}

func gitChangesGet(t *testing.T, h *harness, query string) (*http.Response, gitChangesJSON) {
	t.Helper()
	resp, body := h.do(http.MethodGet, "/git/changes"+query, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var got gitChangesJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	return resp, got
}

// gitChangesUncommitted is gitChangesGet's shorthand for the common
// single-request scope=uncommitted case, over a fresh harness.
func gitChangesUncommitted(t *testing.T, dir string) gitChangesJSON {
	t.Helper()
	_, got := gitChangesGet(t, newGitChangesHarness(t, dir), "?scope=uncommitted&dir="+dir)
	return got
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTestFileBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAllTest(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func filesByPath(files []gitChangeFile) map[string]gitChangeFile {
	m := make(map[string]gitChangeFile, len(files))
	for _, f := range files {
		m[f.Path] = f
	}
	return m
}

// TestHandleGitChangesBadRequest covers every 400 case.
func TestHandleGitChangesBadRequest(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (root, query string)
	}{
		{"invalid scope", func(t *testing.T) (string, string) {
			dir := newGitRepo(t)
			return dir, "?scope=commit&dir=" + dir
		}},
		{"dir outside workspace root", func(t *testing.T) (string, string) {
			return newGitRepo(t), "?dir=" + t.TempDir()
		}},
		{"dir does not exist", func(t *testing.T) (string, string) {
			root := newGitRepo(t)
			return root, "?dir=" + filepath.Join(root, "does-not-exist")
		}},
		{"dir escapes root via symlink", func(t *testing.T) (string, string) {
			root := newGitRepo(t)
			link := filepath.Join(root, "escape")
			if err := os.Symlink(t.TempDir(), link); err != nil {
				t.Fatal(err)
			}
			return root, "?dir=" + link
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, query := c.setup(t)
			h := newGitChangesHarness(t, root)
			resp, body := h.do(http.MethodGet, "/git/changes"+query, nil)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
			}
		})
	}
}

// TestHandleGitChangesConflict covers every 409 case.
func TestHandleGitChangesConflict(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T) string
		query      string
		wantSubstr string
	}{
		{"not a git repo", func(t *testing.T) string { return t.TempDir() }, "?dir=", "not_a_git_repo"},
		{"no origin remote", newGitRepo, "?scope=branch&dir=", "no_base"},
		{"no common ancestor", func(t *testing.T) string {
			dir := newGitRepo(t)
			bare := t.TempDir()
			runTestGit(t, bare, "init", "-q", "--bare")
			runTestGit(t, dir, "remote", "add", "origin", bare)
			runTestGit(t, dir, "checkout", "-q", "--orphan", "unrelated")
			runTestGit(t, dir, "commit", "-q", "-m", "unrelated root", "--allow-empty")
			runTestGit(t, dir, "push", "-q", "origin", "HEAD:main")
			runTestGit(t, dir, "remote", "set-head", "origin", "main")
			runTestGit(t, dir, "checkout", "-q", "master")
			return dir
		}, "?scope=branch&dir=", "no_base"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.setup(t)
			h := newGitChangesHarness(t, dir)
			resp, body := h.do(http.MethodGet, "/git/changes"+c.query+dir, nil)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), c.wantSubstr) {
				t.Errorf("body = %s, want %q", body, c.wantSubstr)
			}
		})
	}
}

// TestHandleGitChangesCeilingColonDoesNotLeakOuterRepo: GIT_CEILING_DIRECTORIES
// is itself colon-separated with no escape, so a workspace root under a path
// component containing a colon can't defeat it and reach an enclosing
// repository outside every granted root.
func TestHandleGitChangesCeilingColonDoesNotLeakOuterRepo(t *testing.T) {
	base := t.TempDir()
	outerRepo := filepath.Join(base, "outer")
	runTestGit(t, base, "init", "-q", outerRepo)
	runTestGit(t, outerRepo, "config", "user.email", "test@example.com")
	runTestGit(t, outerRepo, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(outerRepo, "secret.txt"), "outer\n")
	runTestGit(t, outerRepo, "add", "secret.txt")
	runTestGit(t, outerRepo, "commit", "-q", "-m", "outer repo")

	// "har:ness" defeats GIT_CEILING_DIRECTORIES (colon-separated, no
	// escape); root is the only granted workspace root, a plain
	// subdirectory with no .git of its own.
	root := filepath.Join(outerRepo, "har:ness", "root")
	mkdirAllTest(t, root)

	h := newGitChangesHarness(t, root)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+root, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not_a_git_repo") {
		t.Errorf("body = %s, want error code not_a_git_repo, not the outer repository's own changes", body)
	}
}

// TestHandleGitChangesRepoRootTrailingSpace: a repository whose real path
// ends in a space is still found, not misread as not_a_git_repo.
func TestHandleGitChangesRepoRootTrailingSpace(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "repo ") // TrimSpace would eat this trailing space
	mkdirAllTest(t, dir)
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "init")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")

	h := newGitChangesHarness(t, dir)
	_, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+url.QueryEscape(dir))
	if len(got.Files) != 1 || got.Files[0].Path != "new.txt" {
		t.Errorf("Files = %+v, want one added new.txt", got.Files)
	}
}

// TestHandleGitChangesRepoPathColonDoesNotBreakObjectResolution: a
// legitimately granted repository whose own path contains a colon must
// still resolve objects through GIT_ALTERNATE_OBJECT_DIRECTORIES (itself
// colon-separated), not fail with "bad object".
func TestHandleGitChangesRepoPathColonDoesNotBreakObjectResolution(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "my:repo")
	mkdirAllTest(t, dir)
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\n")
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "init")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "new.txt" {
		t.Errorf("Files = %+v, want one added new.txt", got.Files)
	}
}

// TestHandleGitChangesIgnoresInheritedGitDir: a GIT_DIR in harness's own
// environment (git hooks export one) must not redirect the endpoint to
// another repository's index.
func TestHandleGitChangesIgnoresInheritedGitDir(t *testing.T) {
	dir := newGitRepo(t)
	outer := newGitRepo(t)
	writeTestFile(t, filepath.Join(outer, "outer.txt"), "outer\n")
	runTestGit(t, outer, "add", "outer.txt")
	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(outer, ".git", "index"))

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 0 {
		t.Errorf("Files = %+v, want none: the requested repository is clean", got.Files)
	}
}

// TestHandleGitChangesIgnoresInheritedPathspecMode: an inherited
// GIT_LITERAL_PATHSPECS=1 must not turn add -N's ":(exclude,literal)" magic
// into a literal path that matches nothing.
func TestHandleGitChangesIgnoresInheritedPathspecMode(t *testing.T) {
	dir := newGitRepo(t)
	nested := filepath.Join(dir, "vendor", "dep")
	mkdirAllTest(t, nested)
	runTestGit(t, nested, "init", "-q")
	runTestGit(t, nested, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "i")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "new.txt" {
		t.Errorf("Files = %+v, want one added new.txt", got.Files)
	}
}

// TestHandleGitChangesManyFilterDriversIsTooManyChanges409: 50,000 filter
// drivers fit gitChangesMetadataCap as discovery output (~1 MiB), but their
// -c overrides (~5 MiB) exceed ARG_MAX, so git diff's exec fails with E2BIG.
// That is a scale ceiling (409), not a server fault (500).
func TestHandleGitChangesManyFilterDriversIsTooManyChanges409(t *testing.T) {
	dir := newGitRepo(t)
	var cfg strings.Builder
	for i := range 50_000 {
		fmt.Fprintf(&cfg, "[filter \"d%05d\"]\n\tclean = cat\n", i)
	}
	f, err := os.OpenFile(filepath.Join(dir, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(cfg.String()); err != nil {
		t.Fatal(err)
	}
	f.Close()

	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "too_many_changes") {
		t.Errorf("status = %d, body = %.300s, want 409 too_many_changes", resp.StatusCode, body)
	}
}

// TestHandleGitChangesMissingIndexInCommittedRepo: a committed repository
// whose .git/index is missing must not report every unchanged tracked file
// (deleted, or modified 0/0) just because the private index started empty.
func TestHandleGitChangesMissingIndexInCommittedRepo(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFileBytes(t, filepath.Join(dir, "big.bin"), make([]byte, untrackedLargeCutoff+1))
	runTestGit(t, dir, "add", "big.bin")
	runTestGit(t, dir, "commit", "-q", "-m", "big")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")
	if err := os.Remove(filepath.Join(dir, ".git", "index")); err != nil {
		t.Fatal(err)
	}

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "seed.txt" || got.Files[0].Status != "modified" || got.Files[0].Additions != 1 {
		t.Errorf("Files = %+v, want only seed.txt modified +1 (big.bin is unchanged)", got.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index")); !os.IsNotExist(err) {
		t.Errorf("real .git/index exists after the request (err=%v); only the private copy may be written", err)
	}
}

// TestHandleGitChangesSharedCloneResolvesAlternates: a `clone --shared`
// repository keeps its objects in objects/info/alternates; the private
// GIT_OBJECT_DIRECTORY must still reach them through the real objects dir.
func TestHandleGitChangesSharedCloneResolvesAlternates(t *testing.T) {
	src := newGitRepo(t)
	dir := filepath.Join(t.TempDir(), "shared")
	runTestGit(t, src, "clone", "-q", "--shared", src, dir)
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "seed.txt" || got.Files[0].Additions != 1 {
		t.Errorf("Files = %+v, want seed.txt modified +1", got.Files)
	}
}

// TestHandleGitChangesEmptyDirIsProcessCwd: an omitted dir means the
// process's own cwd, as for POST /session's workdir, which is never checked
// against WorkspaceRoots; an explicit dir outside them is still a 400.
func TestHandleGitChangesEmptyDirIsProcessCwd(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")
	t.Chdir(dir)
	h := newGitChangesHarness(t, t.TempDir()) // cwd is outside every root

	_, got := gitChangesGet(t, h, "?scope=uncommitted")
	if len(got.Files) != 1 || got.Files[0].Path != "new.txt" {
		t.Errorf("Files = %+v, want the cwd repository's new.txt", got.Files)
	}
	if resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("explicit dir outside roots: status = %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestHandleGitChangesRequiresAuth(t *testing.T) {
	dir := newGitRepo(t)
	h := newGitChangesHarness(t, dir)
	req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/git/changes?dir="+dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestHandleGitChangesUncommitted diffs HEAD against the working tree, tracked plus untracked.
func TestHandleGitChangesUncommitted(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "hello\n")
	got := gitChangesUncommitted(t, dir)
	if got.Base != nil {
		t.Errorf("Base = %+v, want nil for scope=uncommitted", got.Base)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want exactly 2 entries (no surplus)", got.Files)
	}
	byPath := filesByPath(got.Files)
	if seed := byPath["seed.txt"]; seed.Status != "modified" || seed.Additions != 1 {
		t.Errorf("seed.txt entry = %+v, want modified/+1", seed)
	}
	if nf := byPath["new.txt"]; nf.Status != "added" || nf.Additions != 1 {
		t.Errorf("new.txt entry = %+v, want added/+1", nf)
	}
	if !strings.Contains(got.Patch, "+more") || !strings.Contains(got.Patch, "+hello") {
		t.Errorf("patch missing expected hunks: %q", got.Patch)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
}

// TestHandleGitChangesBranchScopeMergeBase diffs merge-base(HEAD, default branch) against the working tree.
func TestHandleGitChangesBranchScopeMergeBase(t *testing.T) {
	dir := newGitRepo(t)
	bare := t.TempDir()
	runTestGit(t, bare, "init", "-q", "--bare")
	runTestGit(t, dir, "remote", "add", "origin", bare)
	runTestGit(t, dir, "push", "-q", "origin", "HEAD:master")
	runTestGit(t, dir, "remote", "set-head", "origin", "-a")
	baseSHA := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))

	writeTestFile(t, filepath.Join(dir, "feature.txt"), "feature\n")
	runTestGit(t, dir, "add", "feature.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "add feature")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nuncommitted\n")

	h := newGitChangesHarness(t, dir)
	_, got := gitChangesGet(t, h, "?dir="+dir) // scope omitted: defaults to branch
	if got.Scope != "branch" {
		t.Errorf("Scope = %q, want branch (default)", got.Scope)
	}
	if got.Base == nil || got.Base.Ref != "origin/master" || got.Base.SHA != baseSHA {
		t.Errorf("Base = %+v, want {origin/master %s}", got.Base, baseSHA)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want exactly 2 entries (no surplus)", got.Files)
	}
	byPath := filesByPath(got.Files)
	if f := byPath["feature.txt"]; f.Status != "added" {
		t.Errorf("feature.txt entry = %+v, want added", f)
	}
	if f := byPath["seed.txt"]; f.Status != "modified" {
		t.Errorf("seed.txt entry = %+v, want modified", f)
	}
	if !strings.Contains(got.Patch, "+feature") || !strings.Contains(got.Patch, "+uncommitted") {
		t.Errorf("patch missing expected hunks: %q", got.Patch)
	}
}

func TestHandleGitChangesDetachedHead(t *testing.T) {
	dir := newGitRepo(t)
	head := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	runTestGit(t, dir, "checkout", "-q", head)
	got := gitChangesUncommitted(t, dir)
	if got.Branch != "" {
		t.Errorf("Branch = %q, want empty (detached HEAD)", got.Branch)
	}
	if got.Head != head {
		t.Errorf("Head = %q, want %q", got.Head, head)
	}
}

func TestHandleGitChangesRenamedFile(t *testing.T) {
	dir := newGitRepo(t)
	// A large shared body keeps rename similarity above git's 50% threshold.
	body := strings.Repeat("seed\n", 20)
	writeTestFile(t, filepath.Join(dir, "seed.txt"), body)
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "grow seed")
	runTestGit(t, dir, "mv", "seed.txt", "renamed.txt")
	writeTestFile(t, filepath.Join(dir, "renamed.txt"), body+"extra\n")
	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 {
		t.Fatalf("Files = %+v, want exactly 1 entry", got.Files)
	}
	f := got.Files[0]
	if f.Status != "renamed" || f.Path != "renamed.txt" || f.OldPath != "seed.txt" {
		t.Errorf("entry = %+v, want renamed seed.txt -> renamed.txt", f)
	}
	if !strings.Contains(got.Patch, "rename from seed.txt") {
		t.Errorf("patch missing rename notice: %s", got.Patch)
	}
}

func TestHandleGitChangesDeletedFile(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Status != "deleted" || got.Files[0].Deletions != 1 {
		t.Fatalf("Files = %+v, want one deleted seed.txt with 1 deletion", got.Files)
	}
}

// TestHandleGitChangesBinaryFile proves Binary=true, zero counts, no raw content in patch.
func TestHandleGitChangesBinaryFile(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "bin.dat"), "\x00\x01\x02\x03")
	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || !got.Files[0].Binary || got.Files[0].Additions != 0 {
		t.Fatalf("Files = %+v, want one binary bin.dat with 0 additions", got.Files)
	}
	if !strings.Contains(got.Patch, "Binary files") {
		t.Errorf("patch = %q, want git's binary notice", got.Patch)
	}
	if strings.ContainsRune(got.Patch, 0x02) {
		t.Errorf("patch leaked raw binary content: %q", got.Patch)
	}
}

// TestHandleGitChangesPatchTruncatesAtFileBoundary proves the cap stops at a whole file, never mid-hunk.
func TestHandleGitChangesPatchTruncatesAtFileBoundary(t *testing.T) {
	dir := newGitRepo(t)
	small := strings.Repeat("a\n", 10)
	big := strings.Repeat("b\n", gitChangesPatchCap) // its patch alone exceeds the cap
	writeTestFile(t, filepath.Join(dir, "a_small.txt"), small)
	writeTestFile(t, filepath.Join(dir, "z_big.txt"), big)
	got := gitChangesUncommitted(t, dir)
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want both entries present regardless of truncation", got.Files)
	}
	if !strings.Contains(got.Patch, "a_small.txt") || strings.Contains(got.Patch, "z_big.txt") {
		t.Errorf("patch = %q, want only the in-budget file", got.Patch)
	}
	if len(got.Patch) > gitChangesPatchCap {
		t.Errorf("patch length %d exceeds cap %d", len(got.Patch), gitChangesPatchCap)
	}
}

// gitCmdCount counts git subprocesses spawned, via gitCmdHook.
func gitCmdCount(t *testing.T) *int {
	t.Helper()
	n := 0
	old := gitCmdHook
	gitCmdHook = func(args []string) { n++ }
	t.Cleanup(func() { gitCmdHook = old })
	return &n
}

// TestHandleGitChangesSubprocessCountIsConstant: 50k untracked root files cost the same subprocess count as 3.
func TestHandleGitChangesSubprocessCountIsConstant(t *testing.T) {
	populate := func(dir string, n int) {
		for i := 0; i < n; i++ {
			writeTestFile(t, filepath.Join(dir, fmt.Sprintf("f%05d.txt", i)), "x\n")
		}
	}

	small := newGitRepo(t)
	populate(small, 3)
	count := gitCmdCount(t)
	got := gitChangesUncommitted(t, small)
	if len(got.Files) != 3 {
		t.Fatalf("Files = %+v", got.Files)
	}
	smallCount := *count

	const n = 50000
	big := newGitRepo(t)
	populate(big, n)
	count = gitCmdCount(t)
	got = gitChangesUncommitted(t, big)
	if len(got.Files) != n {
		t.Fatalf("Files count = %d, want %d", len(got.Files), n)
	}
	if *count != smallCount {
		t.Errorf("git subprocess count = %d for 3 files, %d for %d files; want equal", smallCount, *count, n)
	}
}

// TestHandleGitChangesSingleHugeFileCapped: one tracked file's huge modification is still capped correctly.
func TestHandleGitChangesSingleHugeFileCapped(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "huge.txt"), "x\n")
	runTestGit(t, dir, "add", "huge.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "add huge.txt")
	huge := strings.Repeat("b\n", 2*gitChangesPatchCap)
	writeTestFile(t, filepath.Join(dir, "huge.txt"), huge)
	got := gitChangesUncommitted(t, dir)
	if !got.Truncated || got.Patch != "" {
		t.Errorf("Truncated = %v, Patch = %q, want true/empty (not even one whole file fits)", got.Truncated, got.Patch)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "huge.txt" {
		t.Errorf("Files = %+v, want one huge.txt entry regardless of patch truncation", got.Files)
	}
}

// TestLastWholeFileBoundaryKeepsFileEndingExactlyAtLimit: a file ending exactly at limit is kept, not dropped.
func TestLastWholeFileBoundaryKeepsFileEndingExactlyAtLimit(t *testing.T) {
	const limit = 20
	file1 := strings.Repeat("a", limit-1) + "\n" // ends with '\n' at index limit-1
	data := append([]byte(file1), []byte("diff --git a/file2 b/file2\n...")...)
	if got := lastWholeFileBoundary(data, limit); got != limit {
		t.Errorf("lastWholeFileBoundary = %d, want %d (file1 kept whole)", got, limit)
	}
}

// TestHandleGitChangesFilesNeverNull proves "files" serializes as [], not null.
func TestHandleGitChangesFilesNeverNull(t *testing.T) {
	dir := newGitRepo(t) // freshly committed, nothing changed
	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), `"files":null`) {
		t.Fatalf("body = %s, want files to serialize as [] not null", body)
	}
	var got gitChangesJSON
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Files == nil {
		t.Error("Files = nil, want a non-nil empty slice")
	}
}

func TestHandleGitChangesNeverRunsExternalDiffOrTextconv(t *testing.T) {
	dir := newGitRepo(t)
	sentinel := filepath.Join(t.TempDir(), "external-diff-ran")
	runTestGit(t, dir, "config", "diff.external", "touch "+sentinel+" #")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")
	gitChangesUncommitted(t, dir)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("diff.external ran: a hostile repo config executed an arbitrary command")
	}
}

// TestHandleGitChangesGlobLikeFilenameTreatedLiterally: "b*.txt" is not expanded as a glob.
func TestHandleGitChangesGlobLikeFilenameTreatedLiterally(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "b*.txt"), "b\n")
	writeTestFile(t, filepath.Join(dir, "bx.txt"), "x\n")
	runTestGit(t, dir, "add", "b*.txt", "bx.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "add glob-like files")
	writeTestFile(t, filepath.Join(dir, "b*.txt"), "b\nmore\n")
	writeTestFile(t, filepath.Join(dir, "bx.txt"), "x\nmore\n")

	got := gitChangesUncommitted(t, dir)
	if n := strings.Count(got.Patch, "diff --git a/bx.txt b/bx.txt"); n != 1 {
		t.Errorf("patch contains %d bx.txt diff headers, want exactly 1 (got: %s)", n, got.Patch)
	}
}

// TestHandleGitChangesUntrackedEntries: missing dropped; nested repo and
// every over-cutoff file excluded from intent-to-add and reported as a big
// path candidate, regardless of whether it turns out to exist in the base
// tree (gitChangeSet, not this function, decides that from the diff).
func TestHandleGitChangesUntrackedEntries(t *testing.T) {
	dir := newGitRepo(t)
	bigContent := make([]byte, untrackedLargeCutoff+1)
	writeTestFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), bigContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked_big.bin"), bigContent, 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "tracked_big.bin")
	runTestGit(t, dir, "commit", "-q", "-m", "add tracked_big.bin")
	runTestGit(t, dir, "rm", "-q", "--cached", "tracked_big.bin")

	lsFilesOut := "keep.txt\x00big.bin\x00tracked_big.bin\x00vendor/dep/\x00gone.txt\x00"
	excludeArgs, bigPaths := untrackedEntries(dir, lsFilesOut)

	wantExclude := ":(exclude,literal)big.bin,:(exclude,literal)tracked_big.bin,:(exclude,literal)vendor/dep/"
	if strings.Join(excludeArgs, ",") != wantExclude {
		t.Errorf("excludeArgs = %v, want %s", excludeArgs, wantExclude)
	}
	wantBig := []string{"big.bin", "tracked_big.bin"}
	if !slices.Equal(bigPaths, wantBig) {
		t.Errorf("bigPaths = %v, want %v", bigPaths, wantBig)
	}
}

// TestHandleGitChangesLargeFileAlreadyInBaseIsNotDuplicated: `git rm
// --cached` of a tracked file over the large-file cutoff reports it once,
// as deleted — not also as a synthetic large:true "added" entry. A
// newline in one large file's own path must not affect any sibling's
// status, since the base-tree check is now a plain path lookup against
// the already-parsed diff, not a line-oriented subprocess protocol.
func TestHandleGitChangesLargeFileAlreadyInBaseIsNotDuplicated(t *testing.T) {
	big := strings.Repeat("x\n", 3*1024*1024/2)
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		check func(t *testing.T, got gitChangesJSON)
	}{
		{"rm --cached", func(t *testing.T, dir string) {
			writeTestFile(t, filepath.Join(dir, "big.bin"), big)
			runTestGit(t, dir, "add", "big.bin")
			runTestGit(t, dir, "commit", "-q", "-m", "add big.bin")
			runTestGit(t, dir, "rm", "-q", "--cached", "big.bin")
		}, func(t *testing.T, got gitChangesJSON) {
			if len(got.Files) != 1 || got.Files[0].Path != "big.bin" || got.Files[0].Status != "deleted" || got.Files[0].Large {
				t.Errorf("Files = %+v, want exactly one deleted big.bin (not also large:true added)", got.Files)
			}
		}},
		{"newline in one path doesn't shift the rest", func(t *testing.T, dir string) {
			writeTestFile(t, filepath.Join(dir, "a\nb.bin"), big)
			writeTestFile(t, filepath.Join(dir, "c.bin"), big)
			runTestGit(t, dir, "add", "c.bin")
			runTestGit(t, dir, "commit", "-q", "-m", "add c.bin")
			runTestGit(t, dir, "rm", "-q", "--cached", "c.bin")
			writeTestFile(t, filepath.Join(dir, "d.bin"), big)
		}, func(t *testing.T, got gitChangesJSON) {
			if len(got.Files) != 3 {
				t.Fatalf("Files = %+v, want exactly 3 entries", got.Files)
			}
			byPath := filesByPath(got.Files)
			if f := byPath["a\nb.bin"]; !f.Large {
				t.Errorf("a\\nb.bin entry = %+v, want large=true", f)
			}
			if f := byPath["c.bin"]; f.Status != "deleted" || f.Large {
				t.Errorf("c.bin entry = %+v, want deleted, not large", f)
			}
			if f := byPath["d.bin"]; !f.Large {
				t.Errorf("d.bin entry = %+v, want large=true", f)
			}
		}},
		{"rename pairs the large old path via oldPath, not just newPath", func(t *testing.T, dir string) {
			full := make([]byte, untrackedLargeCutoff+100_000)
			mathrand.New(mathrand.NewSource(1)).Read(full)
			writeTestFileBytes(t, filepath.Join(dir, "p.bin"), full)
			runTestGit(t, dir, "add", "p.bin")
			runTestGit(t, dir, "commit", "-q", "-m", "add p.bin")
			runTestGit(t, dir, "rm", "-q", "--cached", "p.bin")
			// q.bin is under the cutoff (staged normally) but shares most of
			// p.bin's own content, so -M's default 50% threshold pairs them.
			writeTestFileBytes(t, filepath.Join(dir, "q.bin"), full[:untrackedLargeCutoff-50_000])
		}, func(t *testing.T, got gitChangesJSON) {
			if len(got.Files) != 1 {
				t.Fatalf("Files = %+v, want exactly 1 entry (a rename, not also a duplicate large:true added)", got.Files)
			}
			f := got.Files[0]
			if f.Status != "renamed" || f.OldPath != "p.bin" || f.Path != "q.bin" || f.Large {
				t.Errorf("Files[0] = %+v, want a renamed p.bin -> q.bin, not large", f)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newGitRepo(t)
			c.setup(t, dir)
			c.check(t, gitChangesUncommitted(t, dir))
		})
	}
}

// TestHandleGitChangesVanishedUntrackedFileNotFatal: a file (large or not) deleted right as add -N runs is silently absent, not fatal.
func TestHandleGitChangesVanishedUntrackedFileNotFatal(t *testing.T) {
	dir := newGitRepo(t)
	vanish := filepath.Join(dir, "vanish.txt")
	vanishBig := filepath.Join(dir, "vanish.bin")
	writeTestFile(t, vanish, "v\n")
	writeTestFileBytes(t, vanishBig, make([]byte, untrackedLargeCutoff+1))
	writeTestFile(t, filepath.Join(dir, "keep.txt"), "k\n")

	deleted := false
	old := gitCmdHook
	gitCmdHook = func(args []string) {
		if deleted || !slices.Contains(args, "-N") {
			return
		}
		os.Remove(vanish)
		os.Remove(vanishBig)
		deleted = true
	}
	t.Cleanup(func() { gitCmdHook = old })

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "keep.txt" {
		t.Errorf("Files = %+v, want only keep.txt (vanish.txt and large vanish.bin dropped, not fatal)", got.Files)
	}
}

// TestHandleGitChangesTooManyChanges409: an already-expired internal deadline is 409, not 500.
func TestHandleGitChangesTooManyChanges409(t *testing.T) {
	dir := newGitRepo(t)

	oldTimeout := gitChangesTimeout
	gitChangesTimeout = gitChangesResponseMargin // internal budget: 0
	t.Cleanup(func() { gitChangesTimeout = oldTimeout })

	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "too_many_changes") {
		t.Errorf("body = %s, want error code too_many_changes", body)
	}
}

// TestHandleGitChangesMetadataCapIsTooManyChanges409: ls-files/--numstat/
// --name-status output over gitChangesMetadataCap is 409, not an unbounded
// allocation.
func TestHandleGitChangesMetadataCapIsTooManyChanges409(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "untracked.txt"), "n\n")

	oldCap := gitChangesMetadataCap
	gitChangesMetadataCap = 4 // smaller than even one NUL-terminated path
	t.Cleanup(func() { gitChangesMetadataCap = oldCap })

	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "too_many_changes") {
		t.Errorf("body = %s, want error code too_many_changes", body)
	}
}

// gitChangesDo calls srv directly with ctx, so a hook-driven cancellation
// lands deterministically at a chosen subprocess call, with no wall-clock
// wait.
func gitChangesDo(t *testing.T, h *harness, ctx context.Context, query string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/git/changes"+query, nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	resp := rec.Result()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// argsEndWith reports whether args, after gitCmd's static safety flags, is
// exactly the given git subcommand and its own arguments.
func argsEndWith(args []string, suffix ...string) bool {
	if len(args) < len(suffix) {
		return false
	}
	return slices.Equal(args[len(args)-len(suffix):], suffix)
}

// TestHandleGitChangesHeadDeadlineIsTooManyChanges409: a deadline exactly at
// `rev-parse HEAD` must not read as an unborn HEAD; branch scope would
// otherwise answer 409 no_base instead of 409 too_many_changes.
func TestHandleGitChangesHeadDeadlineIsTooManyChanges409(t *testing.T) {
	dir := newGitRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	old := gitCmdHook
	gitCmdHook = func(args []string) {
		if argsEndWith(args, "rev-parse", "HEAD") {
			cancel()
		}
	}
	t.Cleanup(func() { gitCmdHook = old })

	h := newGitChangesHarness(t, dir)
	resp, body := gitChangesDo(t, h, ctx, "?scope=branch&dir="+dir)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "too_many_changes") {
		t.Errorf("body = %s, want error code too_many_changes, not no_base", body)
	}
}

// TestHandleGitChangesDefaultBranchLookupDeadlineIsTooManyChanges409: a
// deadline during defaultBranchRef's own git calls must not read as no
// default branch found; branch scope would otherwise answer 409 no_base
// instead of 409 too_many_changes.
func TestHandleGitChangesDefaultBranchLookupDeadlineIsTooManyChanges409(t *testing.T) {
	dir := newGitRepo(t)

	ctx, cancel := context.WithCancel(context.Background())
	old := gitCmdHook
	gitCmdHook = func(args []string) {
		if argsEndWith(args, "symbolic-ref", "-q", "refs/remotes/origin/HEAD") {
			cancel()
		}
	}
	t.Cleanup(func() { gitCmdHook = old })

	h := newGitChangesHarness(t, dir)
	resp, body := gitChangesDo(t, h, ctx, "?scope=branch&dir="+dir)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "too_many_changes") {
		t.Errorf("body = %s, want error code too_many_changes, not no_base", body)
	}
}

// TestHandleGitChangesLargeUntrackedFile: over the cutoff is large:true with no hunk; under it keeps its hunk.
func TestHandleGitChangesLargeUntrackedFile(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "big.txt"), strings.Repeat("x\n", 3*1024*1024/2))
	writeTestFile(t, filepath.Join(dir, "small.txt"), strings.Repeat("y\n", 100))

	got := gitChangesUncommitted(t, dir)
	byPath := filesByPath(got.Files)
	if big := byPath["big.txt"]; !big.Large || big.Additions != 0 || big.Deletions != 0 {
		t.Errorf("big.txt entry = %+v, want large=true, 0/0", big)
	}
	if strings.Contains(got.Patch, "big.txt") {
		t.Errorf("patch contains a hunk for the large file: %q", got.Patch)
	}
	if small := byPath["small.txt"]; small.Large {
		t.Errorf("small.txt entry = %+v, want large=false", small)
	}
	if !strings.Contains(got.Patch, "diff --git a/small.txt b/small.txt") {
		t.Errorf("patch missing the 1 MiB file's hunk: %q", got.Patch)
	}
}

// listObjectFiles lists loose object files under dir/.git/objects.
func listObjectFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(filepath.Join(dir, ".git", "objects"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

func TestHandleGitChangesUncommittedIncludesUnmergedFiles(t *testing.T) {
	dir := newGitRepo(t)
	branch := strings.TrimSpace(runTestGit(t, dir, "branch", "--show-current"))
	runTestGit(t, dir, "checkout", "-q", "-b", "conflict")
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "side\n")
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "side")
	runTestGit(t, dir, "checkout", "-q", branch)
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "main\n")
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "main")
	cmd := exec.Command("git", "merge", "conflict")
	cmd.Dir = dir
	_, _ = cmd.CombinedOutput()
	if output := runTestGit(t, dir, "ls-files", "-u"); output == "" {
		t.Fatal("merge did not leave unmerged index entries")
	}

	got := gitChangesUncommitted(t, dir)
	if _, ok := filesByPath(got.Files)["seed.txt"]; !ok {
		t.Fatalf("Files = %+v, want seed.txt", got.Files)
	}
}

func TestHandleGitChangesUncommittedIgnoresStaleIndexStat(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.Chtimes(filepath.Join(dir, "seed.txt"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := gitChangesUncommitted(t, dir); len(got.Files) != 0 {
		t.Fatalf("Files = %+v, want none", got.Files)
	}
}

// TestHandleGitChangesIndexNeverWritten: .git/index and .git/objects are both unchanged.
func TestHandleGitChangesIndexNeverWritten(t *testing.T) {
	dir := newGitRepo(t)
	seed := filepath.Join(dir, "seed.txt")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(seed, future, future); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")

	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := listObjectFiles(t, dir)

	gitChangesUncommitted(t, dir)

	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Errorf(".git/index changed: before mtime=%s, after mtime=%s", before.ModTime(), after.ModTime())
	}
	if objectsAfter := listObjectFiles(t, dir); strings.Join(objectsAfter, ",") != strings.Join(objectsBefore, ",") {
		t.Errorf(".git/objects changed: before %v, after %v", objectsBefore, objectsAfter)
	}
}

// TestHandleGitChangesNoSplitIndexFileWritten: with core.splitIndex=true, no sharedindex.* file lands in the real .git.
func TestHandleGitChangesNoSplitIndexFileWritten(t *testing.T) {
	dir := newGitRepo(t)
	runTestGit(t, dir, "config", "core.splitIndex", "true")
	h := newGitChangesHarness(t, dir)
	for i := 0; i < 3; i++ {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), "x\n")
		gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".git", "sharedindex.*"))
	if len(matches) != 0 {
		t.Errorf("sharedindex files in .git = %v, want none", matches)
	}
}

// TestHandleGitChangesExistingSplitIndex: a repository already on a split
// index (its .git/index links to .git/sharedindex.<hash>) still reports the
// right changes from the private copy, and gains no new sharedindex file.
func TestHandleGitChangesExistingSplitIndex(t *testing.T) {
	dir := newGitRepo(t)
	runTestGit(t, dir, "config", "core.splitIndex", "true")
	runTestGit(t, dir, "update-index", "--split-index")
	before, _ := filepath.Glob(filepath.Join(dir, ".git", "sharedindex.*"))
	if len(before) == 0 {
		t.Fatal("setup: no sharedindex file; the index is not split")
	}
	writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "n\n")

	got := filesByPath(gitChangesUncommitted(t, dir).Files)
	if len(got) != 2 || got["seed.txt"].Status != "modified" || got["new.txt"].Status != "added" {
		t.Errorf("Files = %+v, want seed.txt modified and new.txt added", got)
	}
	after, _ := filepath.Glob(filepath.Join(dir, ".git", "sharedindex.*"))
	if !slices.Equal(after, before) {
		t.Errorf("sharedindex files = %v, want unchanged %v", after, before)
	}
}

// TestHandleGitChangesSubdirectoryDirStillCoversWholeRepo: a subdirectory dir still reports the whole repo.
func TestHandleGitChangesSubdirectoryDirStillCoversWholeRepo(t *testing.T) {
	dir := newGitRepo(t)
	sub := filepath.Join(dir, "sub")
	mkdirAllTest(t, sub)
	writeTestFile(t, filepath.Join(sub, "f.txt"), "f\n")
	runTestGit(t, dir, "add", "sub/f.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "add sub/f.txt")
	writeTestFile(t, filepath.Join(sub, "f.txt"), "f\nmore\n")
	writeTestFile(t, filepath.Join(dir, "root_untracked.txt"), "root\n")

	h := newGitChangesHarness(t, dir)
	_, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+sub)
	if got.Dir != sub {
		t.Errorf("Dir = %q, want the requested subdirectory %q", got.Dir, sub)
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want exactly 2 entries (no surplus)", got.Files)
	}
	byPath := filesByPath(got.Files)
	if _, ok := byPath["sub/f.txt"]; !ok {
		t.Errorf("Files = %+v, want both sub/f.txt and root_untracked.txt", got.Files)
	}
	if _, ok := byPath["root_untracked.txt"]; !ok {
		t.Errorf("Files = %+v, want both sub/f.txt and root_untracked.txt", got.Files)
	}
	if !strings.Contains(got.Patch, "+more") || !strings.Contains(got.Patch, "+root") {
		t.Errorf("patch missing expected hunks (both tracked and untracked): %q", got.Patch)
	}
}

// TestHandleGitChangesUnbornHEAD: a commit-less repo answers rather than 500ing.
func TestHandleGitChangesUnbornHEAD(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "hi\n")

	got := gitChangesUncommitted(t, dir)
	if got.Head != "" {
		t.Errorf("Head = %q, want empty (unborn)", got.Head)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "new.txt" || got.Files[0].Status != "added" {
		t.Errorf("Files = %+v, want one new.txt added", got.Files)
	}

	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=branch&dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("branch status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "no_base") {
		t.Errorf("body = %s, want error code no_base", body)
	}
}

// TestHandleGitChangesStaleOriginHEADFallsThrough: a stale origin/HEAD falls through to origin/main.
func TestHandleGitChangesStaleOriginHEADFallsThrough(t *testing.T) {
	dir := newGitRepo(t)
	bare := t.TempDir()
	runTestGit(t, bare, "init", "-q", "--bare")
	runTestGit(t, dir, "remote", "add", "origin", bare)
	runTestGit(t, dir, "push", "-q", "origin", "HEAD:master")
	runTestGit(t, dir, "remote", "set-head", "origin", "-a") // symref -> refs/remotes/origin/master
	runTestGit(t, dir, "push", "-q", "origin", "HEAD:main")
	runTestGit(t, dir, "update-ref", "-d", "refs/remotes/origin/master") // simulate fetch --prune

	h := newGitChangesHarness(t, dir)
	_, got := gitChangesGet(t, h, "?scope=branch&dir="+dir)
	if got.Base == nil || got.Base.Ref != "origin/main" {
		t.Errorf("Base = %+v, want origin/main (fallen through from the stale origin/HEAD)", got.Base)
	}
}

// TestHandleGitChangesPatchNotHTMLEscaped proves the patch is not HTML-escaped.
func TestHandleGitChangesPatchNotHTMLEscaped(t *testing.T) {
	dir := newGitRepo(t)
	writeTestFile(t, filepath.Join(dir, "page.html"), "<div>&amp;</div>\n")
	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=uncommitted&dir="+dir, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "<div>") {
		t.Errorf("body = %s, want literal <div>, not an HTML-escaped form", body)
	}
}

// TestHandleGitChangesNeutralizesFilterDrivers: a filter driver never runs,
// including one whose name holds a dot ("a.b") or an "=" ("x=y"), which a
// `-c filter.<name>.clean=` override would split at the wrong "=".
func TestHandleGitChangesNeutralizesFilterDrivers(t *testing.T) {
	cases := []struct {
		driver       string
		missingIndex bool
	}{
		{"testdrv", false}, {"a.b", false}, {"x=y", false}, {"p=q.r", false},
		{"x=y", true},
	}
	for _, c := range cases {
		driver := c.driver
		t.Run(fmt.Sprintf("%s/missingIndex=%v", driver, c.missingIndex), func(t *testing.T) {
			dir := newGitRepo(t)
			sentinel := filepath.Join(t.TempDir(), "filter-ran")
			// Attach and commit before configuring the driver, or add/commit invokes it early.
			writeTestFile(t, filepath.Join(dir, ".gitattributes"), "seed.txt filter="+driver+"\n")
			runTestGit(t, dir, "add", ".gitattributes")
			runTestGit(t, dir, "commit", "-q", "-m", "attach filter")
			runTestGit(t, dir, "config", "filter."+driver+".clean", "touch "+sentinel+" #")
			runTestGit(t, dir, "config", "filter."+driver+".process", "touch "+sentinel+" #")
			runTestGit(t, dir, "config", "filter."+driver+".required", "true")
			writeTestFile(t, filepath.Join(dir, "seed.txt"), "seed\nmore\n")
			if c.missingIndex {
				if err := os.Remove(filepath.Join(dir, ".git", "index")); err != nil {
					t.Fatal(err)
				}
			}

			got := gitChangesUncommitted(t, dir)
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatal("filter driver ran: a repo-configured filter driver executed an arbitrary command")
			}
			if !strings.Contains(got.Patch, "+more") {
				t.Errorf("patch missing the real diff content: %q", got.Patch)
			}
		})
	}
}

// TestHandleGitChangesNeutralizesHooksPath: a repo-configured post-index-change hook never runs.
func TestHandleGitChangesNeutralizesHooksPath(t *testing.T) {
	dir := newGitRepo(t)
	sentinel := filepath.Join(t.TempDir(), "hook-ran")
	hooksDir := filepath.Join(dir, ".githooks")
	mkdirAllTest(t, hooksDir)
	hook := filepath.Join(hooksDir, "post-index-change")
	writeTestFile(t, hook, "#!/bin/sh\ntouch "+sentinel+"\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "config", "core.hooksPath", ".githooks")
	writeTestFile(t, filepath.Join(dir, "untracked.txt"), "new\n")

	gitChangesUncommitted(t, dir)
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("post-index-change hook ran: a repo-configured hook executed an arbitrary command")
	}
}

// TestHandleGitChangesFileCounts: gitignored, nested-repo, and dirty-submodule content are all excluded.
func TestHandleGitChangesFileCounts(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T) string
		wantCount int
		wantPath  string
	}{
		{"gitignore honored", func(t *testing.T) string {
			dir := newGitRepo(t)
			writeTestFile(t, filepath.Join(dir, ".gitignore"), "*.log\n")
			runTestGit(t, dir, "add", ".gitignore")
			runTestGit(t, dir, "commit", "-q", "-m", "add gitignore")
			writeTestFile(t, filepath.Join(dir, "debug.log"), "noisy\n")
			return dir
		}, 0, ""},
		{"nested repo dropped, not faked", func(t *testing.T) string {
			dir := newGitRepo(t)
			mkdirAllTest(t, filepath.Join(dir, "vendor", "dep"))
			runTestGit(t, filepath.Join(dir, "vendor", "dep"), "init", "-q")
			writeTestFile(t, filepath.Join(dir, "real.txt"), "hi\n")
			return dir
		}, 1, "real.txt"},
		{"dirty submodule not walked", func(t *testing.T) string {
			dir := newGitRepo(t)
			subRepo := newGitRepo(t)
			runTestGit(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", subRepo, "sub")
			runTestGit(t, dir, "commit", "-q", "-m", "add submodule")
			writeTestFile(t, filepath.Join(dir, "sub", "seed.txt"), "seed\ndirty\n")
			return dir
		}, 0, ""},
		{"ignored files beside a nested repo", func(t *testing.T) string {
			dir := newGitRepo(t)
			writeTestFile(t, filepath.Join(dir, ".gitignore"), "*.log\n__pycache__/\n")
			runTestGit(t, dir, "add", ".gitignore")
			runTestGit(t, dir, "commit", "-q", "-m", "add gitignore")
			mkdirAllTest(t, filepath.Join(dir, "experiments", "upstream"))
			runTestGit(t, filepath.Join(dir, "experiments", "upstream"), "init", "-q")
			writeTestFile(t, filepath.Join(dir, "experiments", "run.py"), "x\n")
			writeTestFile(t, filepath.Join(dir, "experiments", "out.log"), "noisy\n")
			mkdirAllTest(t, filepath.Join(dir, "experiments", "__pycache__"))
			writeTestFile(t, filepath.Join(dir, "experiments", "__pycache__", "run.pyc"), "bin\n")
			return dir
		}, 1, "experiments/run.py"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.setup(t)
			got := gitChangesUncommitted(t, dir)
			if len(got.Files) != c.wantCount {
				t.Fatalf("Files = %+v, want %d entries", got.Files, c.wantCount)
			}
			if c.wantPath != "" && got.Files[0].Path != c.wantPath {
				t.Errorf("Files[0].Path = %q, want %q", got.Files[0].Path, c.wantPath)
			}
		})
	}
}
