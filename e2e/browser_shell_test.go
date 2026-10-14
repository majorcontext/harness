package e2e

import (
	"strings"
	"testing"
)

// shellRows adds the row of the shell tool and of h.fs: the page seeds a file
// through h.fs, the shell tool and the file tools see the same /work, and the
// page reads back what the shell wrote. One module serves both.
func (k *browserKit) shellRows() {
	t, add, noFailure := k.t, k.add, k.noFailure
	steps := toolChain(
		ftWrite("/work/fruits.txt", "apple\nbanana\napple\ncherry\napple\n"),
		ftTool("bash", ftArgs("command", "cat fruits.txt")),
		ftTool("bash", ftArgs("command", "grep -c apple fruits.txt | sort")),
		ftTool("bash", ftArgs("command", "echo 'written by shell' > note.txt")),
		ftRead(ftArgs("path", "/work/note.txt")),
		ftTool("bash", ftArgs("command", "jq '[.people[].age] | add' people.json")),
		ftTool("bash", ftArgs("command", "curl -s https://example.com")),
		ftTool("bash", ftArgs("command", "while true; do :; done")),
		ftTool("bash", ftArgs("command", "cat /work")),
	)
	f := newBrowserFront(t, nil, steps...)
	add("the shell tool and h.fs share the work directory with the file tools", map[string]any{"kind": "shell", "baseURL": f.URL}, func(t *testing.T, res browserResult, _ string) {
		noFailure(t, res)
		if res.StopReason != "end_turn" {
			t.Fatalf("stopReason = %q, error %+v, console %v", res.StopReason, res.Error, res.Console)
		}
		reqs := f.fake.Requests()
		if len(reqs) != 10 {
			t.Fatalf("model requests = %d, want 10", len(reqs))
		}
		if n := strings.Count(strings.Join(reqs[0].Tools, " ")+" ", "bash "); n != 1 {
			t.Errorf("tools of the first request = %v, want one bash", reqs[0].Tools)
		}
		spec := reqs[0].ToolSpecs["bash"]
		if !strings.Contains(spec.Description, "sandboxed shell") || !strings.Contains(string(spec.InputSchema), `"command"`) {
			t.Errorf("bash tool of the first request = %+v, want the shell description and the command schema", spec)
		}
		for i, want := range map[int]string{
			2: "banana\napple\ncherry",
			3: "3\n",
			5: "written by shell",
			6: "58",
			7: "command not found",
			8: "[exit 126]",
			9: "cat: /work: ",
		} {
			if got := requestText(reqs[i]); !strings.Contains(got, want) {
				t.Errorf("request %d lacks %q: %s", i, want, got)
			}
		}
		checkHostFs(t, res)
	})
}

// checkHostFs checks the note that the shell wrote and the h.fs calls of the page.
func checkHostFs(t *testing.T, res browserResult) {
	t.Helper()
	var r struct {
		Note string
		Fs   struct {
			Text     string
			Bytes    []int
			Appended string
			File     struct {
				IsDirectory bool
				Size        int
			}
			Dir         struct{ IsDirectory bool }
			Listed      []string
			RenamedOld  string
			RenamedText string
			RemovedCode string
			MissingCode string
			DirReadCode string
			Tmp         string
			ForceOK     bool
			NotEmpty    string
		}
	}
	rowData(t, res, &r)
	if r.Note != "written by shell\n" {
		t.Errorf("h.fs read %q of the file that the shell wrote", r.Note)
	}
	fs := r.Fs
	if fs.Text != "hello" || len(fs.Bytes) != 3 || fs.Bytes[2] != 3 || fs.Appended != "hello world" {
		t.Errorf("h.fs write and read: %+v", fs)
	}
	if fs.File.IsDirectory || fs.File.Size != 11 || !fs.Dir.IsDirectory {
		t.Errorf("h.fs stat: file %+v, dir %+v", fs.File, fs.Dir)
	}
	if strings.Join(fs.Listed, ",") != "a.txt,sub" {
		t.Errorf("h.fs readdir = %v", fs.Listed)
	}
	if fs.RenamedOld != "ENOENT" || fs.RenamedText != "hello world" {
		t.Errorf("h.fs rename: old %q, new %q", fs.RenamedOld, fs.RenamedText)
	}
	if fs.RemovedCode != "ENOENT" || fs.MissingCode != "ENOENT" || !fs.ForceOK || fs.NotEmpty != "ENOTEMPTY" {
		t.Errorf("h.fs remove: %+v", fs)
	}
	if fs.DirReadCode != "EISDIR" {
		t.Errorf("h.fs readFile of a directory gave code %q, want EISDIR", fs.DirReadCode)
	}
	if fs.Tmp != "tmp" {
		t.Errorf("h.fs in /tmp read %q", fs.Tmp)
	}
}
