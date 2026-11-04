package main

import (
	"crypto/sha256"
	"fmt"
	"go/types"
	"strings"
	"unicode"
)

func (g *generator) typeName(t types.Type) string {
	return types.TypeString(t, func(p *types.Package) string {
		if p.Path() == g.pkg.Path() {
			return ""
		}
		return p.Name()
	})
}

func (g *generator) add(t types.Type) string {
	for _, existing := range g.queue {
		if types.Identical(existing, t) {
			return g.names[existing]
		}
	}
	n := codecName(g.typeName(t))
	if g.used[n] {
		sum := sha256.Sum256([]byte(types.TypeString(t, func(p *types.Package) string { return p.Path() })))
		n += fmt.Sprintf("%x", sum[:8])
	}
	g.names[t] = n
	g.used[n] = true
	g.queue = append(g.queue, t)
	return n
}

func codecName(s string) string {
	s = strings.ReplaceAll(s, "[]", "SliceOf")
	s = strings.ReplaceAll(s, "map[string]", "MapOf")
	s = strings.ReplaceAll(s, "*", "PtrTo")
	s = strings.ReplaceAll(s, "protocol.", "Protocol")
	s = strings.ReplaceAll(s, "json.", "JSON")
	s = strings.ReplaceAll(s, "time.", "Time")
	if s == "any" || s == "interface{}" {
		return "Any"
	}
	if len(s) != 0 {
		r := []rune(s)
		r[0] = unicode.ToUpper(r[0])
		s = string(r)
	}
	return s
}

func (g *generator) emit(t types.Type) ([]byte, error) {
	n, typ := g.names[t], g.typeName(t)
	var enc, dec strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&enc, f, a...) }
	q := func(f string, a ...any) { fmt.Fprintf(&dec, f, a...) }
	if typ == "time.Time" {
		p("b, err := v.MarshalBinary(); if err != nil { w.fail(err) }; w.bytes(b)\n")
		q("b := r.bytes(); if r.err == nil { r.fail(v.UnmarshalBinary(b)) }\n")
	} else {
		switch u := t.Underlying().(type) {
		case *types.Struct:
			if typ == "protocol.LogPart" {
				q("if !r.enter() { return }; defer r.leave()\n")
			}
			for i := 0; i < u.NumFields(); i++ {
				f := u.Field(i)
				if typ == "State" && f.Name() == "light" {
					continue
				}
				child := g.add(f.Type())
				p("codec%sEncode(w, v.%s)\n", child, f.Name())
				q("codec%sDecode(r, &v.%s)\n", child, f.Name())
			}
		case *types.Basic:
			method := ""
			conv := ""
			switch {
			case u.Info()&types.IsBoolean != 0:
				method = "boolean"
				conv = "bool"
			case u.Info()&types.IsString != 0:
				method = "string"
				conv = "string"
			case u.Info()&types.IsUnsigned != 0:
				method = "unsigned"
				conv = "uint64"
			case u.Info()&types.IsInteger != 0:
				method = "signed"
				conv = "int64"
			case u.Info()&types.IsFloat != 0:
				method = "float64"
				conv = "float64"
			default:
				return nil, fmt.Errorf("unsupported basic %s", typ)
			}
			p("w.%s(%s(v))\n", method, conv)
			q("*v = %s(r.%s())\n", typ, method)
		case *types.Interface:
			if u.NumMethods() != 0 {
				return nil, fmt.Errorf("unsupported interface %s", typ)
			}
			p("w.value(v)\n")
			q("*v = r.value()\n")
		case *types.Pointer:
			child := g.add(u.Elem())
			elem := g.typeName(u.Elem())
			p("w.boolean(v != nil); if v != nil { codec%sEncode(w, *v) }\n", child)
			q("if r.boolean() { if !r.allocate(1, unsafe.Sizeof(*new(%s))) { return }; v2 := new(%s); codec%sDecode(r, v2); *v = v2 } else { *v = nil }\n", elem, elem, child)
		case *types.Slice:
			g.slice(&enc, &dec, typ, u.Elem())
		case *types.Map:
			if b, ok := u.Key().Underlying().(*types.Basic); !ok || b.Info()&types.IsString == 0 {
				return nil, fmt.Errorf("unsupported map key %s", typ)
			}
			g.mapType(&enc, &dec, typ, u)
		default:
			return nil, fmt.Errorf("unsupported %s (%T)", typ, u)
		}
	}
	return []byte(fmt.Sprintf("\nfunc codec%sEncode(w *codecWriter, v %s) {\n%s}\nfunc codec%sDecode(r *codecReader, v *%s) {\n%s}\n", n, typ, enc.String(), n, typ, dec.String())), nil
}

func (g *generator) slice(enc, dec *strings.Builder, typ string, elem types.Type) {
	child, et := g.add(elem), g.typeName(elem)
	fmt.Fprintf(enc, "if v == nil { w.length(-1); return }; w.length(len(v)); for _, x := range v { codec%sEncode(w, x) }\n", child)
	size := "unsafe.Sizeof(*new(" + et + "))"
	if _, ok := elem.Underlying().(*types.Struct); ok {
		size = "unsafe.Sizeof(" + et + "{})"
	}
	if typ == "json.RawMessage" {
		size = "1"
	}
	fmt.Fprintf(dec, "n := r.length(); if n < 0 { *v = nil; return }; if !r.count(n) { return }; if !r.allocate(n, %s) { return }; *v = make(%s, n); for i := range *v { codec%sDecode(r, &(*v)[i]); if r.err != nil { return } }\n", size, typ, child)
}

func (g *generator) mapType(enc, dec *strings.Builder, typ string, m *types.Map) {
	child, et, kt := g.add(m.Elem()), g.typeName(m.Elem()), g.typeName(m.Key())
	fmt.Fprintf(enc, "if v == nil { w.length(-1); return }; w.length(len(v)); keys := make([]string, 0, len(v)); for k := range v { keys = append(keys, string(k)) }; sort.Strings(keys); for _, k := range keys { w.string(k); codec%sEncode(w, v[%s(k)]) }\n", child, kt)
	fmt.Fprintf(dec, "n := r.length(); if n < 0 { *v = nil; return }; if !r.count(n) { return }; if !r.allocateMap(n, unsafe.Sizeof(*new(%s))) { return }; *v = make(%s, n); for i := 0; i < n; i++ { k := %s(r.string()); var x %s; codec%sDecode(r, &x); if r.err != nil { return }; (*v)[k] = x }\n", et, typ, kt, et, child)
}
