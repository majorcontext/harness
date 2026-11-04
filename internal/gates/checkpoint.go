package gates

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"slices"
	"strings"
)

type checkpointSchema struct {
	fset          *token.FileSet
	types         map[string]*ast.TypeSpec
	funcs         []*ast.FuncDecl
	foldFuncs     []*ast.FuncDecl
	protocolFuncs []*ast.FuncDecl
	foldValues    map[string]*ast.GenDecl
	parts         []string
	version       string
}

func checkCheckpointCodec(fsys fs.FS) error {
	s := &checkpointSchema{fset: token.NewFileSet(), types: map[string]*ast.TypeSpec{}, foldValues: map[string]*ast.GenDecl{}}
	for _, dir := range []string{"internal/eventlog", "protocol"} {
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := fs.ReadFile(fsys, path.Join(dir, name))
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(s.fset, name, b, 0)
			if err != nil {
				return err
			}
			s.collect(f, dir, name)
		}
	}
	s.foldFingerprint()
	if err := s.checkFields(); err != nil {
		return err
	}
	slices.Sort(s.parts)
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(s.parts, "\n"))))
	versions := map[string]string{"3": "a0cb64c089d46ca2e4b7a93be8a010ed2195ce69dd25806ddd03141e0e18bbcf", "4": "941fb30233d6e462461ab6a59de63e28eb89f8a1f098f85a04e2eca6a755998a"}
	if versions[s.version] != sum {
		return fmt.Errorf("checkpoint schema or codec changed: version %s has fingerprint %s; increment checkpointVersion and record the new fingerprint", s.version, sum)
	}
	return nil
}

func (s *checkpointSchema) collect(f *ast.File, dir, file string) {
	prefix := ""
	if dir == "protocol" {
		prefix = "protocol."
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch v := spec.(type) {
				case *ast.TypeSpec:
					s.types[prefix+v.Name.Name] = v
				case *ast.ValueSpec:
					for _, name := range v.Names {
						s.foldValues[prefix+name.Name] = d
					}
					if file == "light_codec.go" && len(v.Names) == 1 && v.Names[0].Name == "checkpointVersion" && len(v.Values) == 1 {
						s.version = s.node(v.Values[0])
					}
				}
			}
		case *ast.FuncDecl:
			if dir == "protocol" {
				s.protocolFuncs = append(s.protocolFuncs, d)
			}
			if dir == "internal/eventlog" && !strings.HasPrefix(file, "light_codec") {
				s.foldFuncs = append(s.foldFuncs, d)
			}
			if strings.HasPrefix(file, "light_codec_fields_") {
				s.funcs = append(s.funcs, d)
				s.parts = append(s.parts, s.node(d))
			}
			if file == "light_codec.go" && (strings.HasPrefix(d.Name.Name, "codec") || d.Recv != nil && len(d.Recv.List) == 1 && strings.Contains(s.node(d.Recv.List[0].Type), "codec")) {
				s.parts = append(s.parts, s.node(d))
			}
		}
	}
}

func (s *checkpointSchema) foldFingerprint() {
	pending := map[string]bool{"Apply": true, "remember": true, "trim": true}
	values := map[string]bool{}
	seen := map[*ast.FuncDecl]bool{}
	seenValues := map[*ast.GenDecl]bool{}
	for len(pending) > 0 || len(values) > 0 {
		for name := range values {
			delete(values, name)
			decl := s.foldValues[name]
			if decl == nil || seenValues[decl] {
				continue
			}
			seenValues[decl] = true
			s.parts = append(s.parts, s.node(decl))
			prefix := ""
			if strings.HasPrefix(name, "protocol.") {
				prefix = "protocol."
			}
			checkpointReferences(decl, prefix, pending, values)
		}
		var name string
		for name = range pending {
			break
		}
		delete(pending, name)
		funcs, prefix := s.foldFuncs, ""
		if strings.HasPrefix(name, "protocol.") {
			funcs, prefix = s.protocolFuncs, "protocol."
			name = strings.TrimPrefix(name, prefix)
		}
		for _, fn := range funcs {
			if fn.Name.Name != name || seen[fn] {
				continue
			}
			seen[fn] = true
			s.parts = append(s.parts, s.node(fn))
			checkpointReferences(fn.Body, prefix, pending, values)
		}
	}
}

func checkpointReferences(node ast.Node, prefix string, funcs, values map[string]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			values[prefix+n.Name] = true
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && id.Name == "protocol" {
				values["protocol."+n.Sel.Name] = true
			}
		case *ast.CallExpr:
			switch f := n.Fun.(type) {
			case *ast.Ident:
				funcs[prefix+f.Name] = true
			case *ast.SelectorExpr:
				if prefix == "" {
					funcs[f.Sel.Name] = true
				}
				if id, ok := f.X.(*ast.Ident); ok && id.Name == "protocol" {
					funcs["protocol."+f.Sel.Name] = true
				}
			}
		}
		return true
	})
}

func (s *checkpointSchema) checkFields() error {
	seen := map[string]bool{}
	for _, fn := range s.funcs {
		if !strings.HasSuffix(fn.Name.Name, "Encode") || len(fn.Type.Params.List) != 2 {
			continue
		}
		name := s.node(fn.Type.Params.List[1].Type)
		ts := s.types[name]
		if ts == nil {
			continue
		}
		if !seen[name] {
			s.parts = append(s.parts, name+s.node(ts))
			seen[name] = true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		want, err := checkpointStructFields(name, st)
		if err != nil {
			return err
		}
		for _, suffix := range []string{"Encode", "Decode"} {
			partner := strings.TrimSuffix(fn.Name.Name, "Encode") + suffix
			var match *ast.FuncDecl
			for _, f := range s.funcs {
				if f.Name.Name == partner {
					match = f
					break
				}
			}
			if match == nil {
				return fmt.Errorf("missing %s", partner)
			}
			if got := checkpointFields(match); !slices.Equal(want, got) {
				return fmt.Errorf("%s fields: got %v, want %v", partner, got, want)
			}
		}
	}
	return nil
}

func checkpointStructFields(name string, st *ast.StructType) ([]string, error) {
	var fields []string
	for _, field := range st.Fields.List {
		for _, id := range field.Names {
			if name != "State" || id.Name != "light" {
				fields = append(fields, id.Name)
			}
		}
		if len(field.Names) == 0 {
			switch x := field.Type.(type) {
			case *ast.Ident:
				fields = append(fields, x.Name)
			case *ast.SelectorExpr:
				fields = append(fields, x.Sel.Name)
			default:
				return nil, fmt.Errorf("%s: unsupported embedded checkpoint field", name)
			}
		}
	}
	return fields, nil
}

func (s *checkpointSchema) node(n ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, s.fset, n); err != nil {
		panic(err)
	}
	return b.String()
}

func checkpointFields(fn *ast.FuncDecl) []string {
	var fields []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if ok && id.Name == "v" {
			fields = append(fields, sel.Sel.Name)
		}
		return true
	})
	return fields
}
