package e2e

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// listProcesses lists the processes of the host and records the response.
type listProcesses struct{}

// processAction sends start, stop, restart, or another action word for the
// process name and records the response.
type processAction struct{ name, action string }

// processLogs reads the log of a process, the last tail lines when tail is
// not zero, and records the response.
type processLogs struct {
	name string
	tail int
}

// workspaceChanges reads the changes of the work tree and records the
// response. A relative dir names a directory of the work dir; an absolute
// dir goes out as written.
type workspaceChanges struct{ scope, dir string }

// runGit runs git in dir, a directory of the work dir, with the hardened
// environment of gitIn.
type runGit struct {
	dir  string
	args []string
}

// removeFile removes a file of the work dir.
type removeFile struct{ path string }

// recordGitStatus records the porcelain status of the work dir repository.
type recordGitStatus struct{}

func (listProcesses) run(t *testing.T, r *run) {
	r.record(t, "list_processes", "", r.drv.Processes(t))
}
func (a processAction) run(t *testing.T, r *run) {
	r.record(t, "process_"+a.action, a.name, r.drv.ProcessAction(t, a.name, a.action))
}
func (a processLogs) run(t *testing.T, r *run) {
	r.record(t, "process_logs", a.name, r.drv.ProcessLogs(t, a.name, a.tail))
}
func (a workspaceChanges) run(t *testing.T, r *run) {
	res := r.drv.WorkspaceChanges(t, a.scope, a.dir)
	r.record(t, "workspace_changes", a.scope, res)
	if body, ok := res.Body.(map[string]any); ok && body["patch"] != nil {
		r.record(t, "workspace_changes_wire", a.scope, callResult{Status: res.Status,
			Body: map[string]any{"patch_is_html_escaped": bytes.Contains(res.Wire, []byte(`\u003c`))}})
	}
}
func (a runGit) run(t *testing.T, r *run) {
	dir := filepath.Join(r.drv.Workdir(), a.dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, a.args...)
}
func (a removeFile) run(t *testing.T, r *run) {
	if err := os.Remove(filepath.Join(r.drv.Workdir(), a.path)); err != nil {
		t.Fatal(err)
	}
}
func (recordGitStatus) run(t *testing.T, r *run) {
	out := gitIn(t, r.drv.Workdir(), "status", "--porcelain")
	r.record(t, "git_status", "", callResult{Status: http.StatusOK, Body: map[string]any{"porcelain": out}})
}

// resolved is dir with its symlinks followed, as the route reports it: a
// workspace root is a resolved path.
func resolved(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return real
	}
	return dir
}

const gitDate = "2026-01-01T00:00:00Z"

// gitIn runs git in dir with the inherited GIT_* variables and the user's and
// the system's git config out of the way, so the host cannot change the
// repository that a row builds. The author and the date are fixed, so a
// commit has the same id in every run.
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
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_DATE="+gitDate, "GIT_COMMITTER_DATE="+gitDate)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func git(args ...string) []string { return args }

// baseRepo commits a.txt and gone.txt on main in the work dir and records
// origin/main at that commit, so the branch scope has a base.
func baseRepo() []action {
	return []action{
		writeFile{path: "a.txt", body: "one\ntwo\n"},
		writeFile{path: "gone.txt", body: "bye\n"},
		runGit{args: git("init", "-q", "-b", "main")},
		runGit{args: git("add", ".")},
		runGit{args: git("commit", "-q", "-m", "base")},
		runGit{args: git("update-ref", "refs/remotes/origin/main", "HEAD")},
	}
}

func TestContractProcesses(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:   "process_http_lifecycle",
			config: devProcesses(),
			actions: []action{
				listProcesses{},
				processAction{name: "dev", action: "start"},
				processAction{name: "dev", action: "start"},
				processLogs{name: "dev"},
				processLogs{name: "dev", tail: 1},
				processAction{name: "dev", action: "restart"},
				processAction{name: "dev", action: "stop"},
				listProcesses{},
			},
		},
		{
			name:   "process_http_unknown_name_is_404",
			config: devProcesses(),
			actions: []action{
				processAction{name: "nope", action: "start"},
				processAction{name: "nope", action: "stop"},
				processAction{name: "nope", action: "restart"},
				processLogs{name: "nope"},
				processAction{name: "dev", action: "explode"},
			},
		},
	})
}

func TestContractWorkspace(t *testing.T) {
	// The edit changes the file size: a same-size edit within a second of the commit is missed.
	runScenarios(t, []scenario{
		{
			name: "git_changes_uncommitted_scope_reports_modified_deleted_and_untracked_files",
			actions: append(baseRepo(),
				writeFile{path: "a.txt", body: "one\nTWO!\n"},
				writeFile{path: "new.txt", body: "n\n"},
				writeFile{path: "page.html", body: "<b>&</b>\n"},
				removeFile{path: "gone.txt"},
				workspaceChanges{scope: "uncommitted"},
				recordGitStatus{},
			),
		},
		{
			name: "git_changes_branch_scope_diffs_the_work_tree_against_the_default_branch",
			actions: append(baseRepo(),
				runGit{args: git("checkout", "-q", "-b", "feat")},
				writeFile{path: "a.txt", body: "one\nTWO!\n"},
				runGit{args: git("commit", "-q", "-am", "feat")},
				writeFile{path: "new.txt", body: "n\n"},
				workspaceChanges{},
			),
		},
		{
			name: "git_changes_uncommitted_scope_on_an_unborn_repo_reports_files_as_added",
			actions: []action{
				writeFile{path: "first.txt", body: "1\n"},
				runGit{args: git("init", "-q", "-b", "main")},
				workspaceChanges{scope: "uncommitted"},
			},
		},
		{
			name: "git_changes_refusals",
			actions: []action{
				writeFile{path: "plain/x.txt", body: "x\n"},
				writeFile{path: "unborn/x.txt", body: "x\n"},
				writeFile{path: "nobase/x.txt", body: "x\n"},
				runGit{dir: "unborn", args: git("init", "-q", "-b", "main")},
				runGit{dir: "nobase", args: git("init", "-q", "-b", "main")},
				runGit{dir: "nobase", args: git("add", ".")},
				runGit{dir: "nobase", args: git("commit", "-q", "-m", "only")},
				workspaceChanges{scope: "bogus"},
				workspaceChanges{dir: "/nonexistent"},
				workspaceChanges{dir: "missing"},
				workspaceChanges{scope: "uncommitted", dir: "plain"},
				workspaceChanges{dir: "unborn"},
				workspaceChanges{dir: "nobase"},
			},
		},
	})
}
