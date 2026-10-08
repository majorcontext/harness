package gates

import (
	"os"
	"slices"
	"testing"
	"testing/fstest"
)

const miniSpec = "### Public packages\n\n| Package | Owns |\n| --- | --- |\n| `harness` | x |\n| `harness/config` | y |\n"

func goFile(imports, body string) *fstest.MapFile {
	return file("package p\n\n" + imports + "\n" + body + "\n")
}

var structureCases = []struct {
	name  string
	files fstest.MapFS
	allow map[string]string
	want  []string
}{
	{
		name:  "the_packages_of_the_spec_pass",
		files: fstest.MapFS{"x.go": goFile("", "var _ = 1"), "config/c.go": goFile("", "var _ = 1"), "internal/a/a.go": goFile("", "var _ = 1"), "cmd/h/m.go": file("package main\n")},
	},
	{
		name:  "a_package_outside_the_spec_fails",
		files: fstest.MapFS{"x.go": goFile("", ""), "config/c.go": goFile("", ""), "extra/e.go": goFile("", "")},
		want:  []string{"public_api:extra"},
	},
	{
		name:  "a_package_of_the_spec_without_files_fails",
		files: fstest.MapFS{"x.go": goFile("", "")},
		want:  []string{"public_api:config"},
	},
	{
		name:  "an_allow_list_entry_covers_one_package",
		files: fstest.MapFS{"x.go": goFile("", ""), "config/c.go": goFile("", ""), "extra/e.go": goFile("", "")},
		allow: map[string]string{"extra": "2026-10-08: x"},
	},
	{
		name:  "an_allow_list_entry_for_a_listed_package_fails",
		files: fstest.MapFS{"x.go": goFile("", ""), "config/c.go": goFile("", "")},
		allow: map[string]string{"config": "2026-10-08: x"},
		want:  []string{"public_api:config"},
	},
	{
		name:  "the_context_window_is_read_outside_capabilities",
		files: fstest.MapFS{"x.go": goFile(`import "github.com/majorcontext/harness/internal/modelmeta"`, "func F() { modelmeta.ContextWindow() }"), "config/c.go": goFile("", "")},
		want:  []string{"context_window_resolver:x.go"},
	},
	{
		name: "the_context_window_is_read_in_capabilities",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/backend/modelapi/modelapi.go": goFile(`import "github.com/majorcontext/harness/internal/modelmeta"`, "func Capabilities() { modelmeta.ContextWindow() }")},
	},
	{
		name: "the_context_window_is_read_in_another_function_of_the_backend",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/backend/modelapi/modelapi.go": goFile(`import "github.com/majorcontext/harness/internal/modelmeta"`, "func Run() { modelmeta.ContextWindow() }")},
		want: []string{"context_window_resolver:internal/backend/modelapi/modelapi.go"},
	},
	{
		name: "writers_and_mounts_outside_their_owners_fail",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/a/a.go": goFile(`import ("net/http"; "github.com/majorcontext/harness/protocol"; "github.com/majorcontext/harness/internal/message")`,
				"func F(s S, mux *http.ServeMux) {\n\ts.Append(1, 2, 3)\n\ts.PutBlob(1, 2, 3, 4)\n\ts.Apply(1)\n\t_ = protocol.ErrorBody{}\n\t_ = &message.EngineContext{}\n\tmux.HandleFunc(\"/\", nil)\n\t_ = http.NewServeMux()\n}")},
		want: []string{"engine_context_creator:internal/a/a.go", "single_appender:internal/a/a.go", "single_applier:internal/a/a.go",
			"single_blob_writer:internal/a/a.go", "single_error_envelope:internal/a/a.go", "single_route_mount:internal/a/a.go", "single_route_mount:internal/a/a.go"},
	},
	{
		name: "writers_in_their_owners_pass",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/session/actor.go": goFile("", "func F(s S) {\n\ts.Append(1, 2, 3)\n\ts.Apply(1)\n}"),
			"internal/server/server.go": goFile(`import ("net/http"; "github.com/majorcontext/harness/protocol")`, "func F() {\n\t_ = protocol.ErrorBody{}\n\t_ = http.NewServeMux()\n}")},
	},
}

func TestStructureRules(t *testing.T) {
	for _, tc := range structureCases {
		t.Run(tc.name, func(t *testing.T) {
			srcs, err := parseSources(tc.files)
			if err != nil {
				t.Fatal(err)
			}
			api, err := checkPublicAPI(srcs, miniSpec, tc.allow)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, v := range append(checkNodeRules(srcs), api...) {
				got = append(got, v.Rule+":"+v.Path)
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Errorf("violations = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStructureOfTheRepository(t *testing.T) {
	spec, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	vs, err := CheckStructure(os.DirFS("../.."), string(spec))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Errorf("%s: %s: %s", v.Path, v.Rule, v.Detail)
	}
}
