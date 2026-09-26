package engine

import (
	"fmt"
	"hash/fnv"
	"io"
	"reflect"

	"github.com/majorcontext/harness/message"
)

// interfaceVariants lists every concrete type reflection cannot discover
// for an interface schemaVersion may meet; writeSchemaShape panics on an
// interface with no entry rather than hashing just its kind.
var interfaceVariants = map[reflect.Type][]reflect.Type{
	reflect.TypeFor[message.Part](): partVariantTypes(),
}

func partVariantTypes() []reflect.Type {
	variants := message.PartVariants()
	out := make([]reflect.Type, len(variants))
	for i, v := range variants {
		out[i] = reflect.TypeOf(v)
	}
	return out
}

// schemaVersion digests a cache struct's serialized shape. Both persisted
// caches discard a file whose version differs and rebuild from the journal, so
// deriving the version from the shape is what makes a field addition
// invalidate that cache on its own: there is no number to bump by hand, and a
// change to one cache never invalidates the other.
func schemaVersion(t reflect.Type) int {
	h := fnv.New64a()
	writeSchemaShape(h, t, map[reflect.Type]bool{})
	return int(h.Sum64() & 0x7fff_ffff)
}

func writeSchemaShape(w io.Writer, t reflect.Type, path map[reflect.Type]bool) {
	if name := t.Name(); name != "" {
		fmt.Fprintf(w, "%s:", t.String())
	}
	if path[t] {
		fmt.Fprint(w, "cycle")
		return
	}
	path[t] = true
	defer delete(path, t)

	switch t.Kind() {
	case reflect.Struct:
		fmt.Fprint(w, "struct{")
		for i := range t.NumField() {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			fmt.Fprintf(w, "%s %q ", f.Name, f.Tag.Get("json"))
			writeSchemaShape(w, f.Type, path)
			fmt.Fprint(w, ";")
		}
		fmt.Fprint(w, "}")
	case reflect.Pointer, reflect.Slice:
		fmt.Fprintf(w, "%s(", t.Kind())
		writeSchemaShape(w, t.Elem(), path)
		fmt.Fprint(w, ")")
	case reflect.Array:
		fmt.Fprintf(w, "array[%d](", t.Len())
		writeSchemaShape(w, t.Elem(), path)
		fmt.Fprint(w, ")")
	case reflect.Map:
		fmt.Fprint(w, "map(")
		writeSchemaShape(w, t.Key(), path)
		fmt.Fprint(w, ",")
		writeSchemaShape(w, t.Elem(), path)
		fmt.Fprint(w, ")")
	case reflect.Interface:
		if t.NumMethod() == 0 {
			// any has no finite implementor set; it is deliberately opaque
			// JSON, so hashing its kind alone does not under-specify it.
			fmt.Fprint(w, t.Kind())
			break
		}
		variants, ok := interfaceVariants[t]
		if !ok {
			panic(fmt.Sprintf("schema_version: %s has no registered variants; reflection cannot discover its implementors, so schemaVersion cannot account for a change to one", t))
		}
		fmt.Fprint(w, "interface{")
		for _, v := range variants {
			writeSchemaShape(w, v, path)
			fmt.Fprint(w, ";")
		}
		fmt.Fprint(w, "}")
	default:
		fmt.Fprint(w, t.Kind())
	}
}
