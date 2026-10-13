package guard

import (
	"encoding/json"
	"testing"
)

const (
	force = "force push is blocked"
	rm    = "rm -rf on a broad target is blocked"
	env   = ".env files are blocked"
)

type row struct {
	name string
	tool string
	args json.RawMessage
	want string
}

func bashRows() []row {
	var rows []row
	for _, c := range []struct{ cmd, want string }{
		{"git push --force", force},
		{"git push origin main --force", force},
		{"git   push  -f", force},
		{"git push --force-with-lease", force},
		{"git push --force-with-lease=main origin main", force},
		{"git push -fu origin main", force},
		{"echo hi && git push -f", force},
		{"sudo git push --force", force},
		{"git -C repo push -f", force},
		{"git -c user.name=x push --force", force},
		{"git push origin +main", force},
		{"git push origin +HEAD:main", force},
		{`git commit -m "push -f fix"`, ""},
		{"git -C push status --force", ""},
		{"git push --follow-tags", ""},
		{"git push origin main", ""},
		{"git commit -f", ""},
		{"git log --force", ""},
		{"rm -rf /", rm},
		{"rm -fr ~", rm},
		{"rm -r -f .", rm},
		{"rm -rf ..", rm},
		{`rm -rf "$HOME"`, rm},
		{"rm -rf ${HOME}/", rm},
		{"rm -rf *", rm},
		{"rm -rf /*", rm},
		{"rm -rf ./*", rm},
		{"rm   -Rf   /", rm},
		{"rm --recursive --force /", rm},
		{"ls; rm -rf ~", rm},
		{"rm -rf ./build", ""},
		{"rm -rf build/", ""},
		{"rm -rf /tmp/x", ""},
		{"rm -f file", ""},
		{"rm -r /", ""},
		{"ls -rf /", ""},
	} {
		b, _ := json.Marshal(map[string]string{"command": c.cmd})
		rows = append(rows, row{c.cmd, "bash", b, c.want})
	}
	return rows
}

func readRows() []row {
	var rows []row
	for _, c := range []struct{ p, want string }{
		{".env", env},
		{"app/.env", env},
		{"/abs/dir/.env.local", env},
		{"config/.env.production", env},
		{".env.example", env},
		{"environment.go", ""},
		{"docs/env.md", ""},
		{"README.md", ""},
	} {
		b, _ := json.Marshal(map[string]string{"path": c.p})
		rows = append(rows, row{c.p, "read_file", b, c.want})
	}
	return append(rows, row{"other tool", "write_file", json.RawMessage(`{"path":".env"}`), ""})
}

func TestVerdict(t *testing.T) {
	tests := append(bashRows(), readRows()...)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verdict(tc.tool, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := verdict("bash", json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed arguments must fail")
	}
}
