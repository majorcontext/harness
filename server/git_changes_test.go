package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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

// TestHandleGitChangesSubprocessCountIsConstant proves subprocess count doesn't grow with file count.
// TestHandleGitChangesSubprocessCountIsConstant also doubles as the scale
// guard: 50k untracked files in one directory would take ~24s in add -N
// alone if pathspec matching regressed to quadratic (per the review's own
// repro), so a generous but discriminating time bound catches that too.
func TestHandleGitChangesSubprocessCountIsConstant(t *testing.T) {
	populate := func(dir string, n int) {
		sub := filepath.Join(dir, "bigdir")
		mkdirAllTest(t, sub)
		for i := 0; i < n; i++ {
			writeTestFile(t, filepath.Join(sub, fmt.Sprintf("f%05d.txt", i)), "x\n")
		}
	}

	small := newGitRepo(t)
	populate(small, 3)
	count := gitCmdCount(t)
	h := newGitChangesHarness(t, small)
	_, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+small)
	if len(got.Files) != 3 {
		t.Fatalf("Files = %+v", got.Files)
	}
	smallCount := *count

	const n = 50000
	big := newGitRepo(t)
	populate(big, n)
	count = gitCmdCount(t)
	h = newGitChangesHarness(t, big)
	start := time.Now()
	_, got = gitChangesGet(t, h, "?scope=uncommitted&dir="+big)
	elapsed := time.Since(start)
	if len(got.Files) != n {
		t.Fatalf("Files count = %d, want %d", len(got.Files), n)
	}
	if elapsed > 20*time.Second {
		t.Errorf("request took %s for %d untracked files, want well under quadratic (~24s)", elapsed, n)
	}
	if *count != smallCount {
		t.Errorf("git subprocess count = %d for 3 files, %d for %d files; want equal", smallCount, *count, n)
	}
}

// TestHandleGitChangesSingleHugeFileCapped: one file alone exceeding the cap is still capped correctly.
func TestHandleGitChangesSingleHugeFileCapped(t *testing.T) {
	dir := newGitRepo(t)
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

// TestHandleGitChangesUntrackedIndexInputSkipsMissingAndDirectories: a
// missing path is dropped; a clean directory is one pathspec; a directory
// containing a nested repo keeps its sibling files but drops the repo.
func TestHandleGitChangesUntrackedIndexInputSkipsMissingAndDirectories(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "keep.txt"), "keep\n")
	mkdirAllTest(t, filepath.Join(dir, "clean_dir"))
	mkdirAllTest(t, filepath.Join(dir, "mixed", "vendor", "dep"))
	writeTestFile(t, filepath.Join(dir, "mixed", "normal.txt"), "n\n")
	runTestGit(t, filepath.Join(dir, "mixed", "vendor", "dep"), "init", "-q")

	lsFilesOut := "keep.txt\x00clean_dir/\x00mixed/\x00gone.txt\x00"
	got := splitNulZ(string(untrackedIndexInput(dir, lsFilesOut)))
	sort.Strings(got)
	want := "clean_dir/,keep.txt,mixed/normal.txt"
	if strings.Join(got, ",") != want {
		t.Errorf("untrackedIndexInput = %v, want %s", got, want)
	}
}

// TestHandleGitChangesRetriesOnVanishedUntrackedFile: gitCmdHook deletes an
// untracked file as `add -N` is about to run, so the request must retry
// against a fresh listing rather than 500ing on the vanished pathspec.
func TestHandleGitChangesRetriesOnVanishedUntrackedFile(t *testing.T) {
	dir := newGitRepo(t)
	vanish := filepath.Join(dir, "vanish.txt")
	writeTestFile(t, vanish, "v\n")
	writeTestFile(t, filepath.Join(dir, "keep.txt"), "k\n")

	deleted := false
	old := gitCmdHook
	gitCmdHook = func(args []string) {
		if deleted || !slices.Contains(args, "-N") {
			return
		}
		os.Remove(vanish)
		deleted = true
	}
	t.Cleanup(func() { gitCmdHook = old })

	got := gitChangesUncommitted(t, dir)
	if len(got.Files) != 1 || got.Files[0].Path != "keep.txt" {
		t.Errorf("Files = %+v, want only keep.txt (vanish.txt dropped, not fatal)", got.Files)
	}
}

// TestHandleGitChangesIndexNeverWritten: .git/index's inode and mtime are unchanged, even with a stat-dirty file.
func TestHandleGitChangesIndexNeverWritten(t *testing.T) {
	dir := newGitRepo(t)
	seed := filepath.Join(dir, "seed.txt")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(seed, future, future); err != nil {
		t.Fatal(err)
	}

	indexPath := filepath.Join(dir, ".git", "index")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	gitChangesUncommitted(t, dir)

	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Errorf(".git/index changed: before mtime=%s, after mtime=%s", before.ModTime(), after.ModTime())
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

// TestHandleGitChangesNeutralizesFilterDrivers: a filter driver never runs, including one named with a dot ("a.b").
func TestHandleGitChangesNeutralizesFilterDrivers(t *testing.T) {
	for _, driver := range []string{"testdrv", "a.b"} {
		t.Run(driver, func(t *testing.T) {
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

// TestHandleGitChangesFileCounts covers scenarios distinguished only by
// how many files (and which) end up in the response: a gitignored
// untracked file, an untracked nested repo, and a dirty submodule are all
// excluded.
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
