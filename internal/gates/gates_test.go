package gates

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func longFunc(n int) string {
	return "package a\n\nfunc F() {\n" + strings.Repeat("\t_ = 1\n", n) + "}\n"
}

func comments(n int) string { return strings.Repeat("// x\n", n) }

func code(n int) string { return strings.Repeat("var _ = 1\n", n) }

func rules(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, v.Path+":"+v.Rule)
	}
	slices.Sort(out)
	return out
}

var checkCases = []struct {
	name       string
	head, base fstest.MapFS
	renames    map[string]string
	want       []string
}{
	{
		name: "new_file_comment_share_at_limit_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(25) + code(74))},
	},
	{
		name: "new_file_comment_share_over_limit_fails",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(26) + code(73))},
		want: []string{"a/a.go:comment_share"},
	},
	{
		name: "new_file_size_at_limit_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(799))},
	},
	{
		name: "new_file_size_over_limit_fails",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(800))},
		want: []string{"a/a.go:file_size"},
	},
	{
		name: "new_func_brace_span_80_passes",
		head: fstest.MapFS{"a/a.go": file(longFunc(79))},
	},
	{
		name: "new_func_brace_span_81_fails",
		head: fstest.MapFS{"a/a.go": file(longFunc(80))},
		want: []string{"a/a.go:long_func"},
	},
	{
		name: "func_with_line_directive_measured_by_raw_lines",
		head: fstest.MapFS{"a/a.go": file("package a\n\nfunc F() {\n//line x.go:1\n" + strings.Repeat("\t_ = 1\n", 80) + "}\n")},
		want: []string{"a/a.go:long_func"},
	},
	{
		name: "new_file_history_marker_fails",
		head: fstest.MapFS{"a/a.go": file("package a\n\n// fixed in #123\nvar A = 1\nvar B = 2\nvar C = 3\n")},
		want: []string{"a/a.go:history"},
	},
	{
		name: "new_test_sleep_and_after_fail",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestX(t *testing.T) {\n\ttime.Sleep(1)\n\t<-time.After(1)\n}\n")},
		want: []string{"a/a_test.go:sleep_after"},
	},
	{
		name: "aliased_time_import_sleep_fails",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\nimport clock \"time\"\n\nfunc TestX() { clock.Sleep(1) }\n")},
		want: []string{"a/a_test.go:sleep_after"},
	},
	{
		name: "sleep_method_on_value_named_time_passes",
		head: fstest.MapFS{"a/a_test.go": file("package a\n\ntype T struct{}\n\nfunc (T) Sleep(int) {}\n\nfunc TestX() {\n\ttime := T{}\n\ttime.Sleep(1)\n}\n")},
	},
	{
		name: "sleep_outside_test_file_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n\nimport \"time\"\n\nfunc F() { time.Sleep(1) }\n")},
	},
	{
		name: "changed_file_within_limit_stays_within",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(10) + code(40))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(30) + code(40))},
	},
	{
		name: "changed_file_crossing_a_limit_it_met_fails",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(30) + code(40))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(10) + code(40))},
		want: []string{"a/a.go:comment_share"},
	},
	{
		name: "changed_over_limit_file_may_not_get_worse",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(900))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(850))},
		want: []string{"a/a.go:file_size"},
	},
	{
		name: "changed_over_limit_file_may_improve",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(850))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(900))},
	},
	{
		name: "changed_over_limit_comment_share_may_not_rise",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(40) + code(60))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(30) + code(70))},
		want: []string{"a/a.go:comment_share"},
	},
	{
		name: "changed_file_may_keep_its_history_marker",
		head: fstest.MapFS{"a/a.go": file("package a\n\n// fixed in #123\nvar A = 1\nvar B = 2\nvar C = 3\nvar D = 4\n")},
		base: fstest.MapFS{"a/a.go": file("package a\n\n// fixed in #123\nvar A = 1\nvar B = 2\nvar C = 3\n")},
	},
	{
		name: "changed_file_may_not_add_a_history_marker",
		head: fstest.MapFS{"a/a.go": file("package a\n\n// fixed in #123 and #124\nvar A = 1\nvar B = 2\nvar C = 3\n")},
		base: fstest.MapFS{"a/a.go": file("package a\n\n// fixed in #123\nvar A = 1\nvar B = 2\nvar C = 3\n")},
		want: []string{"a/a.go:history"},
	},
	{
		name: "changed_file_may_not_add_a_long_func",
		head: fstest.MapFS{"a/a.go": file(longFunc(80) + "\nfunc G() {\n" + strings.Repeat("\t_ = 1\n", 80) + "}\n")},
		base: fstest.MapFS{"a/a.go": file(longFunc(80))},
		want: []string{"a/a.go:long_func"},
	},
	{
		name: "unchanged_over_limit_file_ignored",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(900))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(900))},
	},
	{
		name: "doc_go_package_comment_not_counted",
		head: fstest.MapFS{"a/doc.go": file("// Package a x.\n// y\n// z\n// w\npackage a\n")},
	},
	{
		name: "generated_file_skipped",
		head: fstest.MapFS{"a/a.go": file("// Code generated by x. DO NOT EDIT.\n\npackage a\n// #12\n" + code(900))},
	},
	{
		name: "generated_marker_after_build_tag_skipped",
		head: fstest.MapFS{"a/a.go": file("//go:build linux\n\n// Code generated by x. DO NOT EDIT.\n\npackage a\n" + code(900))},
	},
	{
		name: "directives_are_not_comments",
		head: fstest.MapFS{"a/a.go": file("package a\n\n//go:generate x\n//nolint:all\n//lint:ignore X y\nvar A = 1\n")},
	},
	{
		name: "testdata_and_nested_module_skipped",
		head: fstest.MapFS{
			"a/testdata/x.go": file("package a\n// #123\n"),
			"sub/go.mod":      file("module sub\n"),
			"sub/x.go":        file("package sub\n" + code(900)),
			"wt/.git":         file("gitdir: x\n"),
			"wt/x.go":         file("package wt\n" + code(900)),
		},
	},
	{
		name: "agents_md_caps_root_80_scoped_25",
		head: fstest.MapFS{
			"AGENTS.md":   file(strings.Repeat("x\n", 81)),
			"a/AGENTS.md": file(strings.Repeat("x\n", 25) + "x"),
			"b/AGENTS.md": file(strings.Repeat("x\n", 25)),
		},
		want: []string{"AGENTS.md:agents_cap", "a/AGENTS.md:agents_cap"},
	},
	{
		name: "agents_md_over_cap_may_shrink_not_grow",
		head: fstest.MapFS{
			"a/AGENTS.md": file(strings.Repeat("x\n", 30)),
			"b/AGENTS.md": file(strings.Repeat("x\n", 40)),
		},
		base: fstest.MapFS{
			"a/AGENTS.md": file(strings.Repeat("x\n", 35)),
			"b/AGENTS.md": file(strings.Repeat("x\n", 35)),
		},
		want: []string{"b/AGENTS.md:agents_cap"},
	},
	{
		name: "package_ratio_rise_over_limit_fails",
		head: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(4))},
		base: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(2))},
		want: []string{"a:test_ratio"},
	},
	{
		name: "package_ratio_rise_within_limit_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(4))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(2))},
	},
	{
		name: "package_ratio_over_limit_may_fall",
		head: fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(8))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(11))},
	},
	{
		name: "deleting_untested_dead_code_passes",
		head: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(4))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(4)), "a/a_test.go": file("package a\n" + code(4))},
	},
	{
		name: "deleting_code_while_adding_tests_over_limit_fails",
		head: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(4))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + code(4)), "a/a_test.go": file("package a\n" + code(2))},
		want: []string{"a:test_ratio"},
	},
	{
		name: "deleting_code_from_a_commented_file_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(40) + code(50))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(40) + code(60))},
	},
	{
		name: "comment_count_rise_that_lowers_share_passes",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(50) + code(100))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(40) + code(60))},
	},
	{
		name: "comment_count_rise_that_raises_share_fails",
		head: fstest.MapFS{"a/a.go": file("package a\n" + comments(41) + code(60))},
		base: fstest.MapFS{"a/a.go": file("package a\n" + comments(40) + code(60))},
		want: []string{"a/a.go:comment_share"},
	},
	{
		name:    "moved_over_limit_file_passes",
		head:    fstest.MapFS{"b/new.go": file("package b\n" + code(900))},
		base:    fstest.MapFS{"a/old.go": file("package a\n" + code(900))},
		renames: map[string]string{"b/new.go": "a/old.go"},
	},
	{
		name:    "moved_file_that_grows_past_its_old_size_fails",
		head:    fstest.MapFS{"b/new.go": file("package b\n" + code(901))},
		base:    fstest.MapFS{"a/old.go": file("package a\n" + code(900))},
		renames: map[string]string{"b/new.go": "a/old.go"},
		want:    []string{"b/new.go:file_size"},
	},
	{
		name:    "moved_package_keeps_its_old_ratio",
		head:    fstest.MapFS{"b/b.go": file("package b\n" + code(3)), "b/b_test.go": file("package b\n" + code(11))},
		base:    fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(11))},
		renames: map[string]string{"b/b.go": "a/a.go", "b/b_test.go": "a/a_test.go"},
	},
	{
		name:    "moved_package_may_not_raise_its_old_ratio",
		head:    fstest.MapFS{"b/b.go": file("package b\n" + code(3)), "b/b_test.go": file("package b\n" + code(12))},
		base:    fstest.MapFS{"a/a.go": file("package a\n" + code(3)), "a/a_test.go": file("package a\n" + code(11))},
		renames: map[string]string{"b/b.go": "a/a.go", "b/b_test.go": "a/a_test.go"},
		want:    []string{"b:test_ratio"},
	},
	{
		name: "new_package_at_ratio_limit_passes",
		head: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(2))},
	},
	{
		name: "new_package_over_ratio_limit_fails",
		head: fstest.MapFS{"a/a.go": file("package a\nvar A = 1\n"), "a/a_test.go": file("package a\n" + code(4))},
		want: []string{"a:test_ratio"},
	},
}

func TestCheck(t *testing.T) {
	for _, tc := range checkCases {
		t.Run(tc.name, func(t *testing.T) {
			head, err := Collect(tc.head)
			if err != nil {
				t.Fatal(err)
			}
			base, err := Collect(tc.base)
			if err != nil {
				t.Fatal(err)
			}
			changed := map[string]bool{}
			for p, f := range tc.head {
				if b, ok := tc.base[p]; !ok || string(b.Data) != string(f.Data) {
					changed[p] = true
				}
			}
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if got := rules(Check(head, base, changed, tc.renames)); !slices.Equal(got, want) {
				t.Fatalf("violations = %v, want %v", got, want)
			}
		})
	}
}

func TestCheckSkipsPathsOutsideChangedSet(t *testing.T) {
	head := Report{
		Files:  map[string]FileMetrics{"a.go": {Lines: 900, CodeLines: 900}},
		Agents: map[string]int{"AGENTS.md": 90},
	}
	if got := Check(head, Report{}, nil, nil); len(got) != 0 {
		t.Fatalf("violations = %v, want none", got)
	}
}

func TestAgentsLineCountIncludesUnterminatedLastLine(t *testing.T) {
	r, err := Collect(fstest.MapFS{"AGENTS.md": file("a\nb")})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Agents["AGENTS.md"]; got != 2 {
		t.Fatalf("lines = %d, want 2", got)
	}
}

func TestHistoryPattern(t *testing.T) {
	for text, want := range map[string]bool{
		"fixed in #123":         true,
		"on 2026-10-02":         true,
		"previously did x":      true,
		"no longer needed":      true,
		"red-verified":          true,
		"confirmed live":        true,
		"an earlier version":    true,
		"before this change":    true,
		"review round two":      true,
		"fix round":             true,
		"fix rounds":            true,
		"Round 1: model calls":  true,
		"Copilot said":          true,
		"use x instead of y":    false,
		"argv used to spawn":    false,
		"stop after finding it": false,
		"around 12 items":       false,
		"the round trip":        false,
		"issue #5 is open":      true,
		"a rounds table":        false,
	} {
		if got := historyRE.MatchString(text); got != want {
			t.Errorf("historyRE.MatchString(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCollectCounts(t *testing.T) {
	r, err := Collect(fstest.MapFS{"a/a.go": file("package a\n\n// x\n/* y\nz */\nvar A = 1 // tail\n//line other.go:500\n")})
	if err != nil {
		t.Fatal(err)
	}
	want := FileMetrics{Lines: 7, CommentLines: 3, CodeLines: 3}
	if got := r.Files["a/a.go"]; got != want {
		t.Fatalf("metrics = %+v, want %+v", got, want)
	}
}

func TestWarningsNameChangedFilesInTheWarnTier(t *testing.T) {
	head := Report{Files: map[string]FileMetrics{
		"warn.go":      {CommentLines: 20, CodeLines: 80},
		"quiet.go":     {CommentLines: 10, CodeLines: 90},
		"failing.go":   {CommentLines: 30, CodeLines: 70},
		"unchanged.go": {CommentLines: 20, CodeLines: 80},
	}}
	changed := map[string]bool{"warn.go": true, "quiet.go": true, "failing.go": true}
	got := Warnings(head, changed)
	if len(got) != 1 || !strings.HasPrefix(got[0], "warn.go:") {
		t.Fatalf("warnings = %v, want only warn.go", got)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadBaseReadsMergeBaseAndChangedPaths(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, dir, "a/old.go", "package a\n"+code(3))
	write(t, dir, "a/same.go", "package a\n")
	write(t, dir, "sub/go.mod", "module sub\n")
	write(t, dir, "sub/x.go", "package sub\n"+code(900))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	base := gitIn(t, dir, "rev-parse", "HEAD")
	write(t, dir, "a/old.go", "package a\n"+code(5))
	gitIn(t, dir, "commit", "-q", "-am", "edit")
	write(t, dir, "a/fresh.go", "package a\n")

	got, err := LoadBase(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if m := got.Report.Files["a/old.go"]; m.CodeLines != 4 {
		t.Fatalf("base a/old.go code lines = %d, want 4", m.CodeLines)
	}
	if _, ok := got.Report.Files["sub/x.go"]; ok {
		t.Fatal("nested module file was measured")
	}
	var paths []string
	for p := range got.Changed {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	if want := []string{"a/fresh.go", "a/old.go"}; !slices.Equal(paths, want) {
		t.Fatalf("changed = %v, want %v", paths, want)
	}
}

func TestLoadBaseNamesTheMissingRef(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, dir, "a.go", "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	if _, err := LoadBase(dir, "origin/nope"); err == nil || !strings.Contains(err.Error(), "origin/nope") {
		t.Fatalf("err = %v, want one naming origin/nope", err)
	}
}

func TestRepository(t *testing.T) {
	const root = "../.."
	ref := os.Getenv("GATES_BASE_REF")
	if ref == "" {
		ref = "origin/main"
	}
	head, err := Collect(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	base, err := LoadBase(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range Warnings(head, base.Changed) {
		t.Log("warning: " + w)
		if os.Getenv("GITHUB_ACTIONS") != "" {
			fmt.Printf("::warning title=comment share::%s\n", w)
		}
	}
	for _, v := range Check(head, base.Report, base.Changed, base.Renames) {
		t.Errorf("%s: %s: %s", v.Path, v.Rule, v.Detail)
	}
}

func TestLoadBaseIgnoresCommitsMadeToTheBaseBranchAfterTheBranchPoint(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, dir, "a.go", "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	trunk := gitIn(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	gitIn(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "a.go", "package a\n"+code(2))
	gitIn(t, dir, "commit", "-q", "-am", "edit")
	gitIn(t, dir, "checkout", "-q", trunk)
	write(t, dir, "moved.go", "package a\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "trunk moves")
	gitIn(t, dir, "checkout", "-q", "feature")

	got, err := LoadBase(dir, trunk)
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed["moved.go"] {
		t.Fatal("a trunk commit after the branch point is in Changed")
	}
	if _, ok := got.Report.Files["moved.go"]; ok {
		t.Fatal("a trunk commit after the branch point is in Report")
	}
}

func TestLoadBaseMapsEachRenamedPathToItsOldPath(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, dir, "a/old.go", "package a\n"+code(20))
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	base := gitIn(t, dir, "rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "mv", "a/old.go", "b/new.go")

	got, err := LoadBase(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"b/new.go": "a/old.go"}; !maps.Equal(got.Renames, want) {
		t.Fatalf("renames = %v, want %v", got.Renames, want)
	}
	if !got.Changed["b/new.go"] {
		t.Fatal("renamed path is not in Changed")
	}
}
