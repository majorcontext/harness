package gates

import (
	"go/ast"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
)

// providerNames derives the names of the providers and backends that the spec
// forbids branching on (rule 3) from the places that define them: the Type
// constants of config, the Family constants of the provider wires, the default
// names of the router, the provider constants of modelmeta, and the case
// labels of the modelmeta tables. A new provider extends the set when it
// extends one of these.
func providerNames(srcs []source) map[string]bool {
	out := map[string]bool{}
	add := func(e ast.Expr) {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil && v != "" {
				out[v] = true
			}
		}
	}
	for _, s := range srcs {
		dir := path.Dir(s.path)
		for _, decl := range s.file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						if i >= len(vs.Values) {
							continue
						}
						n := name.Name
						switch {
						case dir == "config" && strings.HasPrefix(n, "Type"),
							strings.HasPrefix(s.path, "provider/") && strings.HasSuffix(n, "Family"),
							s.path == "internal/backend/router.go" && strings.HasPrefix(n, "default"),
							dir == "internal/modelmeta" && strings.HasSuffix(n, "Provider"):
							add(vs.Values[i])
						}
					}
				}
			case *ast.FuncDecl:
				if s.path != "internal/modelmeta/modelmeta.go" || (d.Name.Name != "ContextWindow" && d.Name.Name != "Models") {
					continue
				}
				ast.Inspect(d, func(n ast.Node) bool {
					if cc, ok := n.(*ast.CaseClause); ok {
						for _, e := range cc.List {
							add(e)
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// providerConsts holds each constant of the tree whose value is a provider
// name, keyed by package directory and name, so that a name resolves only
// against the constants of its own package or of the package it imports.
func providerConsts(srcs []source, names map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range srcs {
		dir := path.Dir(s.path)
		for _, decl := range s.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) && isProviderLiteral(vs.Values[i], names) {
						out[dir+"."+name.Name] = true
					}
				}
			}
		}
	}
	return out
}

func isProviderLiteral(e ast.Expr, names map[string]bool) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && names[strings.TrimSuffix(v, "/")]
}

type providerScan struct {
	names  map[string]bool
	consts map[string]bool
}

func (ps providerScan) namesProvider(e ast.Expr, s source) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return ps.namesProvider(x.X, s)
	case *ast.BasicLit:
		return isProviderLiteral(x, ps.names)
	case *ast.Ident:
		return ps.consts[path.Dir(s.path)+"."+x.Name]
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return false
		}
		imp, ok := s.imports[id.Name]
		return ok && ps.consts[strings.TrimPrefix(imp, modulePath+"/")+"."+x.Sel.Name]
	}
	return false
}

func (ps providerScan) branches(n ast.Node, s source) bool {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		return (x.Op == token.EQL || x.Op == token.NEQ) && (ps.namesProvider(x.X, s) || ps.namesProvider(x.Y, s))
	case *ast.CaseClause:
		return slices.ContainsFunc(x.List, func(e ast.Expr) bool { return ps.namesProvider(e, s) })
	case *ast.CompositeLit:
		if _, list := x.Type.(*ast.ArrayType); !list {
			return false
		}
		return slices.ContainsFunc(x.Elts, func(e ast.Expr) bool { return ps.namesProvider(e, s) })
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || (s.imports[id.Name] != "strings" && s.imports[id.Name] != "slices") {
			return false
		}
		return slices.ContainsFunc(x.Args, func(e ast.Expr) bool { return ps.namesProvider(e, s) })
	}
	return false
}

// checkProviderBranches reports each function that compares a value with the
// name of a provider or a backend, or switches on one, outside the owners and
// the allow list. A name is a string literal of providerNames, or a constant
// of the tree that holds one.
func checkProviderBranches(srcs []source, owners []string, allow map[string]string) []Violation {
	names := providerNames(srcs)
	ps := providerScan{names: names, consts: providerConsts(srcs, names)}
	const detail = "branches on a provider or backend name (spec: Four rules, 3)"
	hit := map[string]bool{}
	var out []Violation
	for _, s := range srcs {
		for _, decl := range s.file.Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = fd.Name.Name
			}
			found := false
			ast.Inspect(decl, func(n ast.Node) bool {
				found = found || ps.branches(n, s)
				return true
			})
			if !found {
				continue
			}
			key := s.path + "#" + fn
			hit[key] = true
			if _, listed := allow[key]; !listed && !allows(owners, s.path, fn) {
				out = append(out, Violation{Path: key, Rule: "provider_name_branch", Detail: detail})
			}
		}
	}
	for key := range allow {
		if !hit[key] {
			out = append(out, Violation{Path: key, Rule: "provider_name_branch", Detail: "the allow list names a function that is no violation; delete the entry"})
		}
	}
	return out
}
