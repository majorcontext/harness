package gates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
)

const wireClientFile = "e2e/wire_openapi_test.go"

// wireClientOwners lists the functions of the contract suite that may hold an
// HTTP client that wireClient or wireClientFor has not wrapped: they store the
// client, and every use of it goes through wireClientFor.
var wireClientOwners = []string{
	"e2e/runtime_driver_test.go#newServeDriverIn",
	"e2e/contract_serve_stop_test.go#TestContractServeStartRemovesTheStopReportOfAnEarlierRun",
}

func unwrappedClient(n ast.Node, imports map[string]string) bool {
	switch x := n.(type) {
	case *ast.CallExpr:
		sel, ok := selectorOf(x.Fun)
		if !ok {
			return false
		}
		if id, isPkg := sel.X.(*ast.Ident); isPkg && imports[id.Name] == "net/http" {
			return strings.Contains(" Get Post Head PostForm ", " "+sel.Sel.Name+" ")
		}
		return sel.Sel.Name == "Client" && len(x.Args) == 0
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && imports[id.Name] == "net/http" && x.Sel.Name == "DefaultClient"
	case *ast.CompositeLit:
		sel, ok := x.Type.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && imports[id.Name] == "net/http" && sel.Sel.Name == "Client"
	}
	return false
}

func isWrap(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && (id.Name == "wireClient" || id.Name == "wireClientFor" || id.Name == "wireClientReadOnly")
}

// CheckWireClients reports each HTTP client of the contract suite that
// neither goes through wireClient, wireClientFor, nor wireClientReadOnly, nor is held by an
// allowed function. Such a client skips the check of each response against
// protocol/openapi.json.
func CheckWireClients(fsys fs.FS) ([]Violation, error) {
	files, err := fs.Glob(fsys, "e2e/*_test.go")
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []Violation
	for _, p := range files {
		if p == wireClientFile {
			continue
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		f, err := parser.ParseFile(fset, p, data, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		imports := importNames(f)
		for _, decl := range f.Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				if isWrap(n) {
					return false
				}
				if unwrappedClient(n, imports) && !allows(wireClientOwners, p, fn) {
					out = append(out, Violation{Path: p, Rule: "wire_client", Detail: "an HTTP client of the contract suite must come from wireClient, wireClientFor, or wireClientReadOnly, which check each response against protocol/openapi.json"})
				}
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
