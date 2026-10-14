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
var publicAllow = map[string]string{}

// providerOwners lists the places where a provider name is data: the functions
// of config that validate the provider entries, set the proxy base of the native providers, and map an entry with no type
// by its key, the router functions that build a backend for each provider of
// the registry, the tables of modelmeta, which are keyed by provider, and the
// provider wires (spec: Four rules, 3; Model API backend; Contract source).
var providerOwners = []string{
	"config/load.go#validateAppendSystemPromptArgs",
	"config/provider.go#buildsResponsesAdapter", "config/provider.go#validateCacheTTL",
	"config/provider.go#validateClaudeCodeFields", "config/provider.go#validateProviders", "config/provider.go#WithProxyBase", "config/provider.go#UsesClaudeCLI",
	"internal/backend/router.go#New", "internal/backend/router.go#client",
	"internal/modelmeta/modelmeta.go#ContextWindow", "internal/modelmeta/modelmeta.go#Models", "internal/modelmeta/modelmeta.go#infoOf",
	"internal/modelmeta/internal/genctx/main.go#",
	"internal/provider/",
}

// providerAllow lists the functions that branch on a provider name against
// the spec, each with the dated reason. An entry that stops being a violation
// fails, so the list shrinks to nothing.
var providerAllow = map[string]string{
	"internal/selftool/model.go#billing": "2026-10-08: billing of the model tool names the claude-code and codex families; Built-in tools gives the billing by provider, while Four rules, 3 and the CI gates Structure row list no such place, so the replacement (config sets the billing of each provider entry) waits for a spec decision",
	"cmd/harness/runline.go#refuseOn":    "2026-10-08: harness run tests for the claude-code provider to refuse an unknown /name, as the engine did; rule 3 forbids the name and no spec text lists this place, so the replacement (a capability of the backend) waits for a spec decision",
}

// deletedRefAllow lists the files that still name a deleted path, each with
// the dated reason. An entry that stops being a violation fails, so the list
// shrinks to nothing.
var deletedRefAllow = map[string]string{}

type source struct {
	path    string
	file    *ast.File
	imports map[string]string
}

// nodeRule forbids the nodes that match, except in the files and functions of
// allow. An entry of allow is a file, a directory that ends in "/", or
// "file#Func". The entry "file#" names the declarations of the file outside any
// function.
type nodeRule struct {
	name, detail string
	match        func(n ast.Node, imports map[string]string) bool
	// bind builds match for one file from the string constants of the
	// module and the directory of the file. It replaces match.
	bind  func(strs map[string]string, dir string) func(ast.Node, map[string]string) bool
	allow []string
}

var storeWriters = []string{
	"internal/session/actor.go#appendCtx", "internal/session/actor.go#fence",
	"runtime.go#Append", "runtime.go#PutBlob",
	"httpsync/apply.go#ApplySync", "httpsync/apply.go#fenceEpoch",
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
		name:   "context_window_config",
		detail: "Config.ContextWindowTokens is read only to build the model API backend, whose Capabilities resolves the window (spec: Backend)",
		match:  selectsField("ContextWindowTokens"),
		allow:  []string{"config/", "internal/backend/router.go#New"},
	},
	{
		name:   "context_window_default",
		detail: "the default context window of 128000 tokens is written only by the model API backend and the modelmeta table (spec: Backend)",
		match:  intLiteral(128000),
		allow:  []string{"internal/modelmeta/context_windows_gen.go#", "internal/backend/modelapi/modelapi.go#", "internal/gates/structure.go#"},
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
		allow:  append([]string{"session.go#admitInput"}, storeWriters...),
	},
	{
		name:   "single_applier",
		detail: "only the session actor applies a record to State (spec: Apply)",
		match:  callsMethod("Apply", 1),
		allow:  []string{"internal/eventlog/", "internal/session/actor.go#appendCtx", "internal/session/actor.go#replay"},
	},
	{
		name:   "single_error_envelope",
		detail: "only internal/server, the serve token check, and the sentinel errors build the error body (spec: Errors)",
		match:  literalOf(modulePath+"/protocol", "ErrorBody", "Error"),
		allow:  []string{"protocol/", "internal/server/server.go#errorBody", "cmd/harness/serve.go#bearer", "runtime.go#", "box.go#processError", "inputerror.go#invalidInput", "internal/session/actor.go#", "internal/session/actor.go#notOwned", "internal/session/compact.go#", "internal/session/requests.go#", "internal/session/sync.go#", "internal/backend/router.go#"},
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
		name:   "git_subprocess",
		detail: "only the git helpers of internal/workspace and of the gates start the git process (spec: Workspace)",
		bind:   startsProcess("git"),
		allow:  []string{"internal/workspace/git.go#gitCmd", "internal/workspace/git.go#gitCmdMagicPathspecs", "internal/gates/git.go#git"},
	},
	{
		name:   "engine_list_walker",
		detail: "only workspace.Changes names the engine list, outside its definition (spec: Workspace)",
		match:  namesIdent("engineList"),
		allow:  []string{"internal/workspace/workspace.go#Changes"},
	},
	{
		name:   "engine_context_creator",
		detail: "only the model API backend creates message.EngineContext, from the runtime's pins and logged parts (AGENTS.md: Invariants)",
		match:  literalOf(modulePath+"/internal/message", "EngineContext"),
		allow:  []string{"internal/message/", "internal/backend/modelapi/convert.go#toMessage", "internal/backend/modelapi/modelapi.go#request"},
	},
}

func selectorOf(n ast.Node) (*ast.SelectorExpr, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	return sel, ok
}

func namesIdent(name string) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, _ map[string]string) bool {
		id, ok := n.(*ast.Ident)
		return ok && id.Name == name
	}
}

// startsProcess matches a call of os/exec.Command or CommandContext whose
// program is the string program. The program is a string literal, or a name
// that stringConsts resolves: a constant or variable of the same package, or
// of another package of the module. A program that cannot be resolved, such
// as a local variable, does not match.
func startsProcess(program string) func(map[string]string, string) func(ast.Node, map[string]string) bool {
	return func(strs map[string]string, dir string) func(ast.Node, map[string]string) bool {
		return func(n ast.Node, imports map[string]string) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return false
			}
			sel, ok := selectorOf(call.Fun)
			if !ok {
				return false
			}
			arg := 0
			switch sel.Sel.Name {
			case "Command":
			case "CommandContext":
				arg = 1
			default:
				return false
			}
			if id, ok := sel.X.(*ast.Ident); !ok || imports[id.Name] != "os/exec" || len(call.Args) <= arg {
				return false
			}
			got, ok := stringValue(call.Args[arg], strs, dir, imports)
			return ok && got == program
		}
	}
}

// stringValue returns the value of a string literal, or of a name in strs.
func stringValue(e ast.Expr, strs map[string]string, dir string, imports map[string]string) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(e.Value)
		return v, err == nil
	case *ast.Ident:
		v, ok := strs[dir+"#"+e.Name]
		return v, ok
	case *ast.SelectorExpr:
		id, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		rel, ok := strings.CutPrefix(imports[id.Name], modulePath)
		if !ok || (rel != "" && rel[0] != '/') {
			return "", false
		}
		v, ok := strs[strings.TrimPrefix(rel, "/")+"#"+e.Sel.Name]
		return v, ok
	}
	return "", false
}

// stringConsts maps "dir#Name" to the value of each package-level constant
// or variable of the module that a string literal initializes.
func stringConsts(srcs []source) map[string]string {
	out := map[string]string{}
	for _, s := range srcs {
		dir := path.Dir(s.path)
		if dir == "." {
			dir = ""
		}
		for _, decl := range s.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						break
					}
					if v, ok := stringValue(vs.Values[i], nil, "", nil); ok {
						out[dir+"#"+name.Name] = v
					}
				}
			}
		}
	}
	return out
}

func selectsField(name string) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, _ map[string]string) bool {
		sel, ok := selectorOf(n)
		return ok && sel.Sel.Name == name
	}
}

func intLiteral(value int64) func(ast.Node, map[string]string) bool {
	return func(n ast.Node, _ map[string]string) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			return false
		}
		v, err := strconv.ParseInt(lit.Value, 0, 64)
		return err == nil && v == value
	}
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
	strs := stringConsts(srcs)
	for _, s := range srcs {
		dir := path.Dir(s.path)
		if dir == "." {
			dir = ""
		}
		matchers := make([]func(ast.Node, map[string]string) bool, len(nodeRules))
		for i, r := range nodeRules {
			if matchers[i] = r.match; r.bind != nil {
				matchers[i] = r.bind(strs, dir)
			}
		}
		for _, decl := range s.file.Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			defs := map[*ast.Ident]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				if vs, ok := n.(*ast.ValueSpec); ok {
					for _, name := range vs.Names {
						defs[name] = true
					}
				}
				if id, ok := n.(*ast.Ident); ok && defs[id] {
					return true
				}
				for i, r := range nodeRules {
					if matchers[i](n, s.imports) && !allows(r.allow, s.path, fn) {
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
	out = append(out, checkProviderBranches(srcs, providerOwners, providerAllow)...)
	deleted, err := CheckDeletedReferences(fsys, deletedRefAllow)
	if err != nil {
		return nil, err
	}
	out = append(out, deleted...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Rule+a.Path+a.Detail < b.Rule+b.Path+b.Detail
	})
	return slices.Compact(out), nil
}
