package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir with the inherited GIT_* variables and the user's and
// the system's git config out of the way, so the host cannot change the
// repository the row builds.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=contract", "-c", "user.email=contract@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeIn(t *testing.T, dir, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// changedFiles flattens the files of a /git/changes body into "path status +add -del".
func changedFiles(t *testing.T, body map[string]any) []string {
	t.Helper()
	var out []string
	for _, f := range body["files"].([]any) {
		m := f.(map[string]any)
		out = append(out, strings.Join([]string{
			m["path"].(string), m["status"].(string), "+" + m["additions"].(json.Number).String(), "-" + m["deletions"].(json.Number).String(),
		}, " "))
	}
	return out
}

func checkFiles(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("files = %q, want %q", got, want)
	}
}

// newRepoWorkdir commits a.txt and gone.txt on main and records origin/main
// at that commit, so the branch scope has a base.
func newRepoWorkdir(t *testing.T) (workdir, baseSHA string) {
	t.Helper()
	workdir = runtimeWorkdir(t, map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n"})
	gitIn(t, workdir, "init", "-q", "-b", "main")
	gitIn(t, workdir, "add", ".")
	gitIn(t, workdir, "commit", "-q", "-m", "base")
	gitIn(t, workdir, "update-ref", "refs/remotes/origin/main", "HEAD")
	return workdir, gitIn(t, workdir, "rev-parse", "HEAD")
}

func TestContractRuntimeGitChanges(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"git_changes_uncommitted_scope_reports_modified_deleted_and_untracked_files", gitUncommitted},
		{"git_changes_branch_scope_diffs_the_work_tree_against_the_default_branch", gitBranch},
		{"git_changes_uncommitted_scope_on_an_unborn_repo_reports_files_as_added", gitUnborn},
		{"git_changes_refusals", gitRefusals},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func getChanges(t *testing.T, d *httpDriver, query string) callResult {
	t.Helper()
	return d.call(t, http.MethodGet, "/git/changes"+query, nil)
}

func gitUncommitted(t *testing.T) {
	workdir, base := newRepoWorkdir(t)
	// The edit changes the file size: a same-size edit within a second of the commit is missed.
	writeIn(t, workdir, "a.txt", "one\nTWO!\n")
	writeIn(t, workdir, "new.txt", "n\n")
	if err := os.Remove(filepath.Join(workdir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	d, _ := startRuntime(t, workdir, nil)
	res := getChanges(t, d, "?scope=uncommitted")
	body := bodyOf(t, res)
	if res.Status != http.StatusOK || body["scope"] != "uncommitted" || body["branch"] != "main" || body["head"] != base || body["dir"] != workdir || body["truncated"] != false {
		t.Fatalf("GET /git/changes?scope=uncommitted = %d %v, want main at %s in %s", res.Status, body, base, workdir)
	}
	if _, has := body["base"]; has {
		t.Errorf("uncommitted scope carries a base: %v", body["base"])
	}
	checkFiles(t, changedFiles(t, body), "a.txt modified +1 -1", "gone.txt deleted +0 -1", "new.txt added +1 -0")
	patch := body["patch"].(string)
	for _, want := range []string{"diff --git a/a.txt b/a.txt", "-two\n+TWO!\n", "deleted file mode", "diff --git a/new.txt b/new.txt", "+n\n"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, patch)
		}
	}
	if status := gitIn(t, workdir, "status", "--porcelain"); !strings.Contains(status, "?? new.txt") {
		t.Errorf("git status after the request = %q, want new.txt still untracked: the request must not stage files", status)
	}
}

func gitBranch(t *testing.T) {
	workdir, base := newRepoWorkdir(t)
	gitIn(t, workdir, "checkout", "-q", "-b", "feat")
	writeIn(t, workdir, "a.txt", "one\nTWO!\n")
	gitIn(t, workdir, "commit", "-q", "-am", "feat")
	head := gitIn(t, workdir, "rev-parse", "HEAD")
	writeIn(t, workdir, "new.txt", "n\n")
	d, _ := startRuntime(t, workdir, nil)

	res := getChanges(t, d, "")
	body := bodyOf(t, res)
	if res.Status != http.StatusOK || body["scope"] != "branch" || body["branch"] != "feat" || body["head"] != head {
		t.Fatalf("GET /git/changes = %d %v, want the branch scope on feat at %s", res.Status, body, head)
	}
	if got := body["base"]; got == nil || got.(map[string]any)["ref"] != "origin/main" || got.(map[string]any)["sha"] != base {
		t.Errorf("base = %v, want origin/main at %s", got, base)
	}
	checkFiles(t, changedFiles(t, body), "a.txt modified +1 -1", "new.txt added +1 -0")
}

func gitUnborn(t *testing.T) {
	workdir := runtimeWorkdir(t, map[string]string{"first.txt": "1\n"})
	gitIn(t, workdir, "init", "-q", "-b", "main")
	d, _ := startRuntime(t, workdir, nil)
	body := bodyOf(t, getChanges(t, d, "?scope=uncommitted"))
	if body["head"] != "" || body["branch"] != "main" {
		t.Errorf("unborn repo = head %q branch %q, want no head on main", body["head"], body["branch"])
	}
	checkFiles(t, changedFiles(t, body), "first.txt added +1 -0")
}

func gitRefusals(t *testing.T) {
	root := runtimeWorkdir(t, map[string]string{"plain/x.txt": "x\n", "unborn/x.txt": "x\n", "nobase/x.txt": "x\n"})
	gitIn(t, filepath.Join(root, "unborn"), "init", "-q", "-b", "main")
	nobase := filepath.Join(root, "nobase")
	gitIn(t, nobase, "init", "-q", "-b", "main")
	gitIn(t, nobase, "add", ".")
	gitIn(t, nobase, "commit", "-q", "-m", "only")
	d, _ := startRuntime(t, root, nil)

	missing := filepath.Join(root, "missing")
	plain := filepath.Join(root, "plain")
	rows := []struct {
		name, query string
		status      int
		want        string
	}{
		{"unknown scope", "?scope=bogus", 400, `scope "bogus" must be "branch" or "uncommitted"`},
		{"dir outside the workspace root", "?dir=/nonexistent", 400, `workdir "/nonexistent" is not under an allowed workspace root`},
		{"missing dir under the root", "?dir=" + missing, 400, `dir "` + missing + `": stat ` + missing + `: no such file or directory`},
		{"not a git work tree", "?scope=uncommitted&dir=" + plain, 409, `not_a_git_repo: "` + plain + `" is not a git work tree`},
		{"branch scope with no commit", "?dir=" + filepath.Join(root, "unborn"), 409, "no_base: HEAD has no commit yet"},
		{"branch scope with no default branch", "?dir=" + nobase, 409, "no_base: no default branch found (checked origin/HEAD, origin/main, origin/master)"},
	}
	for _, row := range rows {
		res := getChanges(t, d, row.query)
		if res.Status != row.status || bodyOf(t, res)["error"] != row.want {
			t.Errorf("%s: GET /git/changes%s = %d %v, want %d %q", row.name, row.query, res.Status, res.Body, row.status, row.want)
		}
	}
}
