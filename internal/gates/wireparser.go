package gates

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Read is one field that a parser decodes: its path and the JSON type its Go
// type needs, or "" when the Go type takes any JSON type.
type Read struct{ Path, Type string }

// StructReads lists the reads of one struct that a parser declares. Var is the
// name of the variable it types, or the name of its type.
type StructReads struct {
	Var   string
	Reads []Read
}

type packageTypes map[string]*ast.StructType

func parsePackage(fsys fs.FS, dir string) (map[string]*ast.File, packageTypes, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	types := packageTypes{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, err
		}
		f, err := parser.ParseFile(fset, e.Name(), src, 0)
		if err != nil {
			return nil, nil, err
		}
		files[e.Name()] = f
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range gd.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok {
					if st, ok := ts.Type.(*ast.StructType); ok {
						types[ts.Name.Name] = st
					}
				}
			}
		}
	}
	return files, types, nil
}

func jsonName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(reflect.StructTag(raw).Get("json"), ",")
	if name == "-" {
		return ""
	}
	return name
}

var goJSONTypes = map[string]string{
	"string": "string", "bool": "boolean", "int": "number", "int64": "number", "int32": "number", "uint64": "number", "float64": "number",
}

// typeReads appends the reads of a field of type expr at path.
func (pt packageTypes) typeReads(expr ast.Expr, path string, out *[]Read, depth int) {
	if depth > 8 {
		return
	}
	switch t := expr.(type) {
	case *ast.StarExpr:
		pt.typeReads(t.X, path, out, depth+1)
	case *ast.ArrayType:
		*out = append(*out, Read{path, "array"})
		pt.typeReads(t.Elt, path+"[]", out, depth+1)
	case *ast.MapType:
		*out = append(*out, Read{path, "object"})
		pt.typeReads(t.Value, path+".*", out, depth+1)
	case *ast.StructType:
		*out = append(*out, Read{path, "object"})
		pt.structReads(t, path, out, depth+1)
	case *ast.Ident:
		if st, ok := pt[t.Name]; ok {
			*out = append(*out, Read{path, "object"})
			pt.structReads(st, path, out, depth+1)
			return
		}
		*out = append(*out, Read{path, goJSONTypes[t.Name]})
	default:
		*out = append(*out, Read{path, ""})
	}
}

func (pt packageTypes) structReads(st *ast.StructType, prefix string, out *[]Read, depth int) {
	for _, f := range st.Fields.List {
		name := jsonName(f.Tag)
		if name == "" {
			continue
		}
		pt.typeReads(f.Type, join(prefix, name), out, depth)
	}
}

// TypeReads lists the reads of the named struct type of a package.
func TypeReads(fsys fs.FS, dir, name string) ([]Read, error) {
	_, types, err := parsePackage(fsys, dir)
	if err != nil {
		return nil, err
	}
	st, ok := types[name]
	if !ok {
		return nil, fmt.Errorf("%s has no struct %s", dir, name)
	}
	var out []Read
	types.structReads(st, "", &out, 0)
	return out, nil
}

// CaseReads lists, for the function fn of a package, the reads of each struct
// that it declares inline, keyed by the string case labels of the outermost
// switch clause that holds the struct, joined by "|". A struct outside any clause has the
// label "".
func CaseReads(fsys fs.FS, dir, file, fn string) (map[string][]StructReads, error) {
	files, types, err := parsePackage(fsys, dir)
	if err != nil {
		return nil, err
	}
	f, ok := files[file]
	if !ok {
		return nil, fmt.Errorf("%s/%s not found", dir, file)
	}
	out := map[string][]StructReads{}
	found := false
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Body == nil {
			continue
		}
		found = true
		if err := checkDecodes(fd.Body, fn); err != nil {
			return nil, err
		}
		collect(fd.Body, []string{""}, types, out)
	}
	if !found {
		return nil, fmt.Errorf("%s/%s has no function %s", dir, file, fn)
	}
	return out, nil
}

func caseLabels(cc *ast.CaseClause) []string {
	var labels []string
	for _, e := range cc.List {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil
		}
		labels = append(labels, s)
	}
	return labels
}

func collect(n ast.Node, labels []string, types packageTypes, out map[string][]StructReads) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CaseClause:
			if inner := caseLabels(x); len(inner) > 0 && len(labels) == 1 && labels[0] == "" {
				for _, stmt := range x.Body {
					collect(stmt, inner, types, out)
				}
				return false
			}
		case *ast.ValueSpec:
			st, ok := x.Type.(*ast.StructType)
			if !ok || len(x.Names) == 0 {
				return true
			}
			var reads []Read
			types.structReads(st, "", &reads, 0)
			key := strings.Join(labels, "|")
			out[key] = append(out[key], StructReads{Var: x.Names[0].Name, Reads: reads})
			return false
		}
		return true
	})
}

// Labels lists the case labels of a CaseReads result, sorted.
func Labels(m map[string][]StructReads) []string { return slices.Sorted(maps.Keys(m)) }

func isDecodeCall(call *ast.CallExpr) (target ast.Expr, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || len(call.Args) == 0 {
		return nil, false
	}
	switch sel.Sel.Name {
	case "Unmarshal":
		return call.Args[len(call.Args)-1], len(call.Args) == 2
	case "Decode":
		return call.Args[0], len(call.Args) == 1
	}
	return nil, false
}

// checkDecodes fails when a function decodes JSON into anything but a variable
// declared as an inline struct or a basic type, and when a case clause that
// decodes JSON declares no inline struct, so that a parser rewritten to a named
// type cannot shrink the reads the gate sees.
func checkDecodes(body *ast.BlockStmt, fn string) error {
	inline := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if vs, ok := n.(*ast.ValueSpec); ok {
			switch t := vs.Type.(type) {
			case *ast.StructType:
				for _, nm := range vs.Names {
					inline[nm.Name] = true
				}
			case *ast.Ident:
				if _, basic := goJSONTypes[t.Name]; basic {
					for _, nm := range vs.Names {
						inline[nm.Name] = true
					}
				}
			}
		}
		return true
	})
	var err error
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || err != nil {
			return err == nil
		}
		target, ok := isDecodeCall(call)
		if !ok {
			return true
		}
		var name *ast.Ident
		if addr, ok := target.(*ast.UnaryExpr); ok && addr.Op == token.AND {
			name, _ = addr.X.(*ast.Ident)
		}
		if name == nil || !inline[name.Name] {
			err = fmt.Errorf("%s decodes JSON into a target that is not an inline struct variable; the parser gate cannot read it", fn)
		}
		return true
	})
	if err != nil {
		return err
	}
	ast.Inspect(body, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || err != nil || len(caseLabels(cc)) == 0 {
			return err == nil
		}
		decodes, structs := false, false
		for _, stmt := range cc.Body {
			ast.Inspect(stmt, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if _, ok := isDecodeCall(x); ok {
						decodes = true
					}
				case *ast.ValueSpec:
					if _, ok := x.Type.(*ast.StructType); ok {
						structs = true
					}
				}
				return true
			})
		}
		if decodes && !structs {
			err = fmt.Errorf("%s: case %q decodes JSON but declares no inline struct", fn, caseLabels(cc))
		}
		return true
	})
	return err
}
