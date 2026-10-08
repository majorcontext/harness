package gates

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const modulePath = "github.com/majorcontext/harness"

// publicAllow lists the packages that are public and that the Public packages
// table of the spec does not list. An entry that stops being a violation
// fails, so the list shrinks to nothing.
var publicAllow = map[string]string{
	"provider":              "2026-10-08: the wire clients are public, and the table lists no provider package; the spec text on provider/ is owed",
	"provider/anthropic":    "2026-10-08: see provider",
	"provider/openai":       "2026-10-08: see provider",
	"provider/openaicompat": "2026-10-08: see provider",
}

type source struct {
	path    string
	file    *ast.File
	imports map[string]string
}

// nodeRule forbids the nodes that match, except in the files and functions of
// allow. An entry of allow is a file, a directory that ends in "/", or
// "file#Func".
type nodeRule struct {
	name, detail string
	match        func(n ast.Node, imports map[string]string) bool
	allow        []string
}

var storeWriters = []string{
	"internal/session/actor.go#appendCtx", "internal/session/actor.go#fence",
	"runtime.go#Append", "runtime.go#PutBlob",
	"sync.go#ApplySync", "sync.go#fenceEpoch",
	"cmd/harness/storelog.go#Append", "cmd/harness/storelog.go#PutBlob",
	"storetest/",
}

var nodeRules = []nodeRule{
	{
		name:   "context_window_resolver",
		detail: "modelmeta.ContextWindow is read only by the model API backend's Capabilities (spec: Backend)",
		match:  selectsImport(modulePath+"/internal/modelmeta", "ContextWindow"),
		allow:  []string{"internal/modelmeta/", "internal/backend/modelapi/modelapi.go#Capabilities"},
	},
	{
		name:   "single_appender",
		detail: "only the session actor, ApplySync, and the store wrappers call Store.Append (spec: Actor, Store)",
		match:  callsMethod("Append", 3),
		allow:  storeWriters,
	},
	{
		name:   "single_blob_writer",
		detail: "only the session, the runtime, ApplySync, and the store wrappers call Store.PutBlob (spec: Inputs, Store)",
		match:  callsMethod("PutBlob", 4),
		allow:  append([]string{"session.go#Submit"}, storeWriters...),
	},
	{
		name:   "single_applier",
		detail: "only the session actor applies a record to State (spec: Apply)",
		match:  callsMethod("Apply", 1),
		allow:  []string{"internal/eventlog/", "internal/session/actor.go#appendCtx", "internal/session/actor.go#replay"},
	},
	{
		name:   "single_error_envelope",
		detail: "only internal/server and the serve token check build the error body (spec: Errors)",
		match:  literalOf(modulePath+"/protocol", "ErrorBody", "Error"),
		allow:  []string{"protocol/", "internal/server/server.go", "cmd/harness/serve.go"},
	},
	{
		name:   "single_route_mount",
		detail: "only the muxes of internal/server mount the routes of the API, from server.Table (spec: Routes, Contract source)",
		match:  mountsRoutes("HandleFunc", "Handle"),
		allow: []string{
			"internal/server/reads.go#NewReads", "internal/server/routes.go#bind",
			"internal/backend/external/tools.go#ServeTools",
		},
	},
	{
		name:   "single_route_mux",
		detail: "only internal/server creates the mux of the API (spec: Routes, Contract source)",
		match:  mountsRoutes("NewServeMux"),
		allow: []string{
			"internal/server/server.go#New", "internal/server/reads.go#NewReads",
			"internal/backend/external/tools.go#ServeTools",
		},
	},
	{
		name:   "engine_context_creator",
		detail: "only the model API backend creates message.EngineContext, from the runtime's banner and logged parts (AGENTS.md: Invariants)",
		match:  literalOf(modulePath+"/internal/message", "EngineContext"),
		allow:  []string{"internal/message/", "internal/backend/modelapi/convert.go", "internal/backend/modelapi/modelapi.go"},
	},
}

func selectorOf(n ast.Node) (*ast.SelectorExpr, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	return sel, ok
}

func selectsImport(pkg string, names ...string) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, imports map[string]string) bool {
		sel, ok := selectorOf(n)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && imports[id.Name] == pkg && slices.Contains(names, sel.Sel.Name)
	}
}

func literalOf(pkg string, names ...string) func(ast.Node, map[string]string) bool {
	typed := selectsImport(pkg, names...)
	return func(n ast.Node, imports map[string]string) bool {
		lit, ok := n.(*ast.CompositeLit)
		return ok && lit.Type != nil && typed(lit.Type, imports)
	}
}

func callsMethod(name string, minArgs int) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, _ map[string]string) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < minArgs {
			return false
		}
		sel, ok := selectorOf(call.Fun)
		return ok && sel.Sel.Name == name
	}
}

func mountsRoutes(names ...string) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, imports map[string]string) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := selectorOf(call.Fun)
		if !ok || !slices.Contains(names, sel.Sel.Name) {
			return false
		}
		if id, isPkg := sel.X.(*ast.Ident); isPkg && imports[id.Name] == "net/http" {
			return true
		}
		return sel.Sel.Name == "HandleFunc" || len(call.Args) == 2
	}
}

func allows(allow []string, file, fn string) bool {
	for _, a := range allow {
		switch f, only, scoped := strings.Cut(a, "#"); {
		case scoped:
			if f == file && only == fn {
				return true
			}
		case strings.HasSuffix(a, "/"):
			if strings.HasPrefix(file, a) {
				return true
			}
		case a == file:
			return true
		}
	}
	return false
}

func parseSources(fsys fs.FS) ([]source, error) {
	var out []source
	fset := token.NewFileSet()
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (skipDirs[d.Name()] || nestedRoot(fsys, p)) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, p, data, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, source{path: p, file: f, imports: importNames(f)})
		return nil
	})
	return out, err
}

func importNames(f *ast.File) map[string]string {
	m := map[string]string{}
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		m[name] = p
	}
	return m
}

func checkNodeRules(srcs []source) []Violation {
	var out []Violation
	for _, s := range srcs {
		for _, decl := range s.file.Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				for _, r := range nodeRules {
					if r.match(n, s.imports) && !allows(r.allow, s.path, fn) {
						out = append(out, Violation{Path: s.path, Rule: r.name, Detail: r.detail})
					}
				}
				return true
			})
		}
	}
	return out
}

func exposedPackages(srcs []source) map[string]bool {
	out := map[string]bool{}
	for _, s := range srcs {
		dir := path.Dir(s.path)
		if dir == "." {
			dir = ""
		}
		if s.file.Name.Name != "main" && !slices.Contains(strings.Split(dir, "/"), "internal") {
			out[dir] = true
		}
	}
	return out
}

func displayPackage(dir string) string {
	if dir == "" {
		return "the module root"
	}
	return dir
}

func checkPublicAPI(srcs []source, spec string, allow map[string]string) ([]Violation, error) {
	listed, err := SpecPublicPackages(spec)
	if err != nil {
		return nil, err
	}
	exposed := exposedPackages(srcs)
	var out []Violation
	for dir := range exposed {
		if !slices.Contains(listed, dir) && allow[dir] == "" {
			out = append(out, Violation{Path: displayPackage(dir), Rule: "public_api", Detail: "is public, and the Public packages table of the spec does not list it"})
		}
	}
	for _, dir := range listed {
		if !exposed[dir] {
			out = append(out, Violation{Path: displayPackage(dir), Rule: "public_api", Detail: "is in the Public packages table of the spec and has no package"})
		}
	}
	for dir := range allow {
		if !exposed[dir] || slices.Contains(listed, dir) {
			out = append(out, Violation{Path: dir, Rule: "public_api", Detail: "the allow list names a package that is no violation; delete the entry"})
		}
	}
	return out, nil
}

// CheckStructure applies the structure rules to the Go files of fsys, against
// the spec text: the public packages are the packages of the spec, and each
// concept that the spec gives one resolver or one writer has only that one.
func CheckStructure(fsys fs.FS, spec string) ([]Violation, error) {
	srcs, err := parseSources(fsys)
	if err != nil {
		return nil, err
	}
	out := checkNodeRules(srcs)
	api, err := checkPublicAPI(srcs, spec, publicAllow)
	if err != nil {
		return nil, err
	}
	out = append(out, api...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Rule+a.Path+a.Detail < b.Rule+b.Path+b.Detail
	})
	return slices.Compact(out), nil
}
