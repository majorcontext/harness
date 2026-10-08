package gates

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
)

// providerNames are the names of the providers and backends that the spec
// forbids branching on (rule 3).
var providerNames = []string{"anthropic", "openai", "openrouter", "openai-compat", "claude-code", "claude-code-cli", "codex", "bifrost"}

// providerOwners lists the places where a provider name is data: the config
// package, which validates and defaults the provider entries; the router,
// which builds a backend for each provider of the registry; the provider
// wires; and the tables of modelmeta, which are keyed by provider (spec: Rules,
// Backend, Model API backend, Contract source).
var providerOwners = []string{"config/", "internal/backend/router.go", "internal/modelmeta/", "provider/"}

// providerAllow lists the functions that branch on a provider name against
// the spec, each with the dated reason. An entry that stops being a violation
// fails, so the list shrinks to nothing.
var providerAllow = map[string]string{
	"modeltool.go#billing":            "2026-10-08: billing of the model tool still names the claude-code and codex families; the Problem table of the spec lists billing as a backend-by-name symptom",
	"cmd/harness/runline.go#refuseOn": "2026-10-08: harness run still tests for the claude-code provider to refuse a command; the spec lists no such place",
}

func providerConsts(srcs []source) map[string]bool {
	out := map[string]bool{}
	for _, s := range srcs {
		ast.Inspect(s.file, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) && isProviderLiteral(vs.Values[i]) {
					out[name.Name] = true
				}
			}
			return true
		})
	}
	return out
}

func isProviderLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	return err == nil && slices.Contains(providerNames, v)
}

func namesProvider(e ast.Expr, consts map[string]bool) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return namesProvider(x.X, consts)
	case *ast.BasicLit:
		return isProviderLiteral(x)
	case *ast.Ident:
		return consts[x.Name]
	case *ast.SelectorExpr:
		return consts[x.Sel.Name]
	}
	return false
}

func branchesOnProvider(n ast.Node, consts map[string]bool) bool {
	switch x := n.(type) {
	case *ast.BinaryExpr:
		return (x.Op == token.EQL || x.Op == token.NEQ) && (namesProvider(x.X, consts) || namesProvider(x.Y, consts))
	case *ast.CaseClause:
		return slices.ContainsFunc(x.List, func(e ast.Expr) bool { return namesProvider(e, consts) })
	}
	return false
}

// checkProviderBranches reports each function that compares a value with the
// name of a provider or a backend, or switches on one, outside the owners and
// the allow list. A name is a string literal of providerNames, or a constant
// of the tree that holds one.
func checkProviderBranches(srcs []source, owners []string, allow map[string]string) []Violation {
	consts := providerConsts(srcs)
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
				found = found || branchesOnProvider(n, consts)
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
