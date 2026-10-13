package gates

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestDeletedReferencesReportTheFirstHitLine(t *testing.T) {
	files := fstest.MapFS{
		"first.go": {Data: []byte("// engine/a.go\npackage p\n")},
		"last.go":  {Data: []byte("package p\n\n// engine/a.go")},
		"two.go":   {Data: []byte("package p\n// engine/a.go and server/b.go\n// server/c.go\n")},
		"crlf.go":  {Data: []byte("package p\r\n\r\n// see engine.Run  \r\n")},
		"clean.go": {Data: []byte("package p\n// the engine's loop\n")},
		"decoy.go": {Data: []byte("package p\n// internal/server/x.go, engineer\n")},
		"nolit.go": {Data: []byte("package p\nvar x = 1\n")},
		"empty.go": {},
		"moved.go": {Data: []byte("package p\nimport _ \"github.com/majorcontext/harness/message\"\n")},
		"cl.go":    {Data: []byte("package p\n\n\n// harness-migrate\n")},
		"prov.go":  {Data: []byte("package p\n// provider/claudecode\n")},
	}
	vs, err := CheckDeletedReferences(files, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vs {
		got[v.Path] = v.Detail
	}
	want := map[string]string{
		"first.go": "line 1 names a deleted package path: // engine/a.go",
		"last.go":  "line 3 names a deleted package path: // engine/a.go",
		"two.go":   "line 2 names a deleted package path: // engine/a.go and server/b.go",
		"crlf.go":  "line 3 names a deleted package path: // see engine.Run",
		"moved.go": "line 2 names a deleted package path: import _ \"github.com/majorcontext/harness/message\"",
		"cl.go":    "line 4 names a deleted package path: // harness-migrate",
		"prov.go":  "line 2 names a deleted package path: // provider/claudecode",
	}
	if len(got) != len(want) {
		t.Errorf("violations = %v, want %v", got, want)
	}
	for p, d := range want {
		if got[p] != d {
			t.Errorf("%s: detail = %q, want %q", p, got[p], d)
		}
	}
}

func TestDeletedRulesHoldTheirLiteral(t *testing.T) {
	for _, r := range deletedRules {
		if !strings.Contains(r.re.String(), regexp.QuoteMeta(r.literal)) {
			t.Errorf("pattern %q does not contain its literal %q", r.re, r.literal)
		}
	}
}
