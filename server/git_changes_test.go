package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newGitChangesHarness builds a harness whose WorkspaceRoots is set to root,
// so GET /git/changes' dir parameter can be validated deterministically.
func newGitChangesHarness(t *testing.T, root string) *harness {
	t.Helper()
	return newWorkdirHarness(t, &scriptedProvider{name: "test"}, []string{root})
}

func gitChangesGet(t *testing.T, h *harness, query string) (*http.Response, gitChangesJSON) {
	t.Helper()
	resp, body := h.do(http.MethodGet, "/git/changes"+query, nil)
	var got gitChangesJSON
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, body)
		}
	}
	return resp, got
}

// TestHandleGitChangesInvalidScope400 proves an unrecognized scope value is
// rejected outright rather than silently defaulting.
func TestHandleGitChangesInvalidScope400(t *testing.T) {
	dir := newGitRepo(t)
	h := newGitChangesHarness(t, dir)
	for _, scope := range []string{"commit", "foo", "BRANCH"} {
		resp, body := h.do(http.MethodGet, "/git/changes?scope="+scope+"&dir="+dir, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("scope=%q: status = %d, want 400: %s", scope, resp.StatusCode, body)
		}
	}
}

// TestHandleGitChangesDirOutsideRoots400 proves a dir outside every
// configured workspace root is rejected, exactly like POST /session's own
// workdir validation (resolveWorkDir).
func TestHandleGitChangesDirOutsideRoots400(t *testing.T) {
	root := newGitRepo(t)
	outside := t.TempDir()
	h := newGitChangesHarness(t, root)
	resp, body := h.do(http.MethodGet, "/git/changes?dir="+outside, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
}

// TestHandleGitChangesNotAGitRepo409 proves a dir that resolves fine but is
// not inside any git work tree gets a clean 409, not a 500.
func TestHandleGitChangesNotAGitRepo409(t *testing.T) {
	dir := t.TempDir() // deliberately not a git repo
	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "not_a_git_repo") {
		t.Errorf("body = %s, want it to name error code not_a_git_repo", body)
	}
}

// TestHandleGitChangesNoBase409 proves scope=branch 409s with error code
// no_base when the repo has no origin remote at all — none of
// origin/HEAD, origin/main, origin/master can possibly resolve.
func TestHandleGitChangesNoBase409(t *testing.T) {
	dir := newGitRepo(t)
	h := newGitChangesHarness(t, dir)
	resp, body := h.do(http.MethodGet, "/git/changes?scope=branch&dir="+dir, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "no_base") {
		t.Errorf("body = %s, want it to name error code no_base", body)
	}
}

// TestHandleGitChangesRequiresAuth proves the route sits behind the same
// auth as every other route (s.auth), mirroring TestHandleProcessRequiresAuth.
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

// TestHandleGitChangesUncommitted proves scope=uncommitted diffs HEAD
// against the working tree, folding in both a tracked modification and an
// untracked file as "added", with no base reported.
func TestHandleGitChangesUncommitted(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Base != nil {
		t.Errorf("Base = %+v, want nil for scope=uncommitted", got.Base)
	}
	if got.Dir != dir {
		t.Errorf("Dir = %q, want %q", got.Dir, dir)
	}
	byPath := map[string]gitChangeFile{}
	for _, f := range got.Files {
		byPath[f.Path] = f
	}
	seed, ok := byPath["seed.txt"]
	if !ok || seed.Status != "modified" || seed.Additions != 1 {
		t.Errorf("seed.txt entry = %+v, ok=%v, want modified/+1", seed, ok)
	}
	nf, ok := byPath["new.txt"]
	if !ok || nf.Status != "added" || nf.Additions != 1 {
		t.Errorf("new.txt entry = %+v, ok=%v, want added/+1", nf, ok)
	}
	if !strings.Contains(got.Patch, "+more") {
		t.Errorf("patch missing tracked hunk: %s", got.Patch)
	}
	if !strings.Contains(got.Patch, "+hello") {
		t.Errorf("patch missing untracked file's content: %s", got.Patch)
	}
	if got.Truncated {
		t.Error("Truncated = true, want false")
	}
}

// TestHandleGitChangesBranchScopeMergeBase proves scope=branch diffs
// merge-base(HEAD, origin's default branch) against the working tree: a
// commit made after the push, plus an uncommitted edit, both appear, and
// Base names the merge-base commit precisely.
func TestHandleGitChangesBranchScopeMergeBase(t *testing.T) {
	dir := newGitRepo(t)
	bare := t.TempDir()
	runTestGit(t, bare, "init", "-q", "--bare")
	runTestGit(t, dir, "remote", "add", "origin", bare)
	runTestGit(t, dir, "push", "-q", "origin", "HEAD:master")
	runTestGit(t, dir, "remote", "set-head", "origin", "-a")
	baseSHA := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "feature.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "add feature")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\nuncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?dir="+dir) // scope omitted: defaults to branch
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Scope != "branch" {
		t.Errorf("Scope = %q, want branch (default)", got.Scope)
	}
	if got.Base == nil || got.Base.Ref != "origin/master" || got.Base.SHA != baseSHA {
		t.Errorf("Base = %+v, want {origin/master %s}", got.Base, baseSHA)
	}
	var sawFeature, sawSeed bool
	for _, f := range got.Files {
		if f.Path == "feature.txt" && f.Status == "added" {
			sawFeature = true
		}
		if f.Path == "seed.txt" && f.Status == "modified" {
			sawSeed = true
		}
	}
	if !sawFeature {
		t.Errorf("files = %+v, want feature.txt added", got.Files)
	}
	if !sawSeed {
		t.Errorf("files = %+v, want seed.txt modified", got.Files)
	}
}

// TestHandleGitChangesDetachedHead proves branch is "" when HEAD is
// detached, rather than e.g. a synthetic or stale branch name.
func TestHandleGitChangesDetachedHead(t *testing.T) {
	dir := newGitRepo(t)
	head := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))
	runTestGit(t, dir, "checkout", "-q", head)
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got.Branch != "" {
		t.Errorf("Branch = %q, want empty (detached HEAD)", got.Branch)
	}
	if got.Head != head {
		t.Errorf("Head = %q, want %q", got.Head, head)
	}
}

// TestHandleGitChangesRenamedFile proves a git-mv'd, further-modified file
// reports status "renamed" with old_path set, and its patch shows the
// rename rather than an add+delete pair.
func TestHandleGitChangesRenamedFile(t *testing.T) {
	dir := newGitRepo(t)
	// A large-enough shared body keeps post-rename similarity comfortably
	// above git's default 50% rename-detection threshold.
	body := strings.Repeat("seed\n", 20)
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "seed.txt")
	runTestGit(t, dir, "commit", "-q", "-m", "grow seed")
	runTestGit(t, dir, "mv", "seed.txt", "renamed.txt")
	if err := os.WriteFile(filepath.Join(dir, "renamed.txt"), []byte(body+"extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
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

// TestHandleGitChangesDeletedFile proves a removed tracked file reports
// status "deleted" and its removed content's line count.
func TestHandleGitChangesDeletedFile(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.Remove(filepath.Join(dir, "seed.txt")); err != nil {
		t.Fatal(err)
	}
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(got.Files) != 1 || got.Files[0].Status != "deleted" || got.Files[0].Deletions != 1 {
		t.Fatalf("Files = %+v, want one deleted seed.txt with 1 deletion", got.Files)
	}
}

// TestHandleGitChangesBinaryFile proves a binary file's entry sets
// Binary=true with zero counts, and its patch carries no raw content — just
// git's own "Binary files ... differ" notice.
func TestHandleGitChangesBinaryFile(t *testing.T) {
	dir := newGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
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

// TestHandleGitChangesPatchTruncatesAtFileBoundary proves the patch cap
// stops at a whole file: with two untracked files that together exceed the
// cap, the first (smaller) file's full patch is kept and the second is
// dropped entirely rather than cut mid-hunk, and Truncated is set.
func TestHandleGitChangesPatchTruncatesAtFileBoundary(t *testing.T) {
	dir := newGitRepo(t)
	small := strings.Repeat("a\n", 10)
	big := strings.Repeat("b\n", gitChangesPatchCap) // its patch alone exceeds the cap
	if err := os.WriteFile(filepath.Join(dir, "a_small.txt"), []byte(small), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "z_big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newGitChangesHarness(t, dir)
	resp, got := gitChangesGet(t, h, "?scope=uncommitted&dir="+dir)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if len(got.Files) != 2 {
		t.Fatalf("Files = %+v, want both entries present regardless of truncation", got.Files)
	}
	if !strings.Contains(got.Patch, "a_small.txt") {
		t.Errorf("patch missing the smaller, in-budget file: %q", got.Patch)
	}
	if strings.Contains(got.Patch, "z_big.txt") {
		t.Error("patch includes the over-budget file; want it dropped at the whole-file boundary")
	}
	if len(got.Patch) > gitChangesPatchCap {
		t.Errorf("patch length %d exceeds cap %d", len(got.Patch), gitChangesPatchCap)
	}
}
