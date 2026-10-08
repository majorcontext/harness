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
		name: "the_configured_window_is_read_outside_the_backend_construction",
		files: fstest.MapFS{"config/c.go": goFile("", "func F(c C) { _ = c.ContextWindowTokens }"), "x.go": goFile("", "func G(c C) { _ = c.ContextWindowTokens }"),
			"internal/backend/router.go": goFile("", "func New(c C) { _ = c.ContextWindowTokens }\nfunc Other(c C) { _ = c.ContextWindowTokens }")},
		want: []string{"context_window_config:internal/backend/router.go", "context_window_config:x.go"},
	},
	{
		name: "the_default_window_is_written_outside_the_backend_and_the_table",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", "var w = 128000\nvar v = 128_000\nvar u = 128001"),
			"internal/modelmeta/t.go":               goFile("", "var t = 128000"),
			"internal/backend/modelapi/modelapi.go": goFile("", "const d = 128000\nfunc Capabilities() int { return 128000 }")},
		want: []string{"context_window_default:internal/backend/modelapi/modelapi.go", "context_window_default:x.go", "context_window_default:x.go"},
	},
	{
		name: "writers_and_mounts_outside_their_owners_fail",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/a/a.go": goFile(`import ("net/http"; "github.com/majorcontext/harness/protocol"; "github.com/majorcontext/harness/internal/message")`,
				"func F(s S, mux *http.ServeMux) {\n\ts.Append(1, 2, 3)\n\ts.PutBlob(1, 2, 3, 4)\n\ts.Apply(1)\n\t_ = protocol.ErrorBody{}\n\t_ = &message.EngineContext{}\n\tmux.HandleFunc(\"/\", nil)\n\t_ = http.NewServeMux()\n}")},
		want: []string{"engine_context_creator:internal/a/a.go", "single_appender:internal/a/a.go", "single_applier:internal/a/a.go",
			"single_blob_writer:internal/a/a.go", "single_error_envelope:internal/a/a.go", "single_route_mount:internal/a/a.go", "single_route_mux:internal/a/a.go"},
	},
	{
		name: "writers_in_their_owners_pass",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/session/actor.go": goFile("", "func appendCtx(s S) {\n\ts.Append(1, 2, 3)\n\ts.Apply(1)\n}"),
			"internal/server/server.go": goFile(`import ("net/http"; "github.com/majorcontext/harness/protocol")`, "func errorBody() {\n\t_ = protocol.ErrorBody{}\n}\nfunc New() {\n\t_ = http.NewServeMux()\n}")},
	},
	{
		name: "the_error_body_and_the_engine_context_are_built_in_another_function_of_their_owner_file",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"internal/server/server.go":             goFile(`import "github.com/majorcontext/harness/protocol"`, "func errorBody() { _ = protocol.ErrorBody{} }\nfunc New() { _ = protocol.ErrorBody{} }"),
			"cmd/harness/serve.go":                  file("package main\n\nimport \"github.com/majorcontext/harness/protocol\"\n\nfunc bearer() { _ = protocol.Error{} }\nfunc other() { _ = protocol.Error{} }\n"),
			"internal/backend/modelapi/convert.go":  goFile(`import "github.com/majorcontext/harness/internal/message"`, "func toMessage() { _ = &message.EngineContext{} }\nfunc other() { _ = &message.EngineContext{} }"),
			"internal/backend/modelapi/modelapi.go": goFile(`import "github.com/majorcontext/harness/internal/message"`, "func request() { _ = &message.EngineContext{} }\nfunc Run() { _ = &message.EngineContext{} }")},
		want: []string{"single_error_envelope:internal/server/server.go", "single_error_envelope:cmd/harness/serve.go",
			"engine_context_creator:internal/backend/modelapi/convert.go", "engine_context_creator:internal/backend/modelapi/modelapi.go"},
	},
	{
		name: "a_stray_write_or_route_in_an_owner_file_fails",
		files: fstest.MapFS{"config/c.go": goFile("", ""), "x.go": goFile("", ""),
			"runtime.go":                goFile("", "func scratch(s S) {\n\ts.Append(1, 2, 3)\n}"),
			"internal/server/server.go": goFile(`import "net/http"`, "func New(mux *http.ServeMux) {\n\tmux.HandleFunc(\"GET /debug\", nil)\n}")},
		want: []string{"single_appender:runtime.go", "single_route_mount:internal/server/server.go"},
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

func TestWireClientsOfTheContractSuite(t *testing.T) {
	vs, err := CheckWireClients(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Errorf("%s: %s: %s", v.Path, v.Rule, v.Detail)
	}
}

func TestWireClientRule(t *testing.T) {
	files := fstest.MapFS{
		"e2e/a_test.go": file("package e2e\n\nimport \"net/http\"\n\nfunc F() {\n\twireClient(t, http.DefaultClient).Get(\"x\")\n\twireClientFor(r, &http.Client{})\n\twireClientReadOnly(t, srv.Client())\n}\n"),
		"e2e/b_test.go": file("package e2e\n\nimport \"net/http\"\n\nfunc G() {\n\thttp.Get(\"x\")\n\thttp.DefaultClient.Do(nil)\n\t_ = &http.Client{}\n\t_ = srv.Client()\n}\n"),
	}
	vs, err := CheckWireClients(files)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 {
		t.Errorf("violations = %v, want 4 in e2e/b_test.go", vs)
	}
	for _, v := range vs {
		if v.Path != "e2e/b_test.go" {
			t.Errorf("violation in %s", v.Path)
		}
	}
}
