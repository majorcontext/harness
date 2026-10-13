package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/majorcontext/harness/internal/gates"
	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

const (
	defsPrefix       = "#/$defs/"
	componentsPrefix = "#/components/schemas/"
)

// wireTypes are the types of the wire that no route names: the data of an
// ephemeral frame, the replication call of Sync, the payload of each durable
// event, and the progress of a log.
var wireTypes = append([]any{protocol.ItemFrame{}, protocol.StatusFrame{}, protocol.SyncBatch{}, protocol.SyncAck{}, protocol.Progress{}}, payloadTypes()...)

func payloadTypes() []any {
	var out []any
	for _, p := range protocol.DurablePayloads() {
		out = append(out, reflect.ValueOf(p).Elem().Interface())
	}
	return out
}

func rootTypes() []reflect.Type {
	types := []reflect.Type{reflect.TypeOf(protocol.ErrorBody{})}
	for _, r := range server.Table {
		for _, v := range []any{r.Request, r.Response} {
			if v != nil {
				types = append(types, reflect.TypeOf(v))
			}
		}
	}
	for _, v := range wireTypes {
		types = append(types, reflect.TypeOf(v))
	}
	return types
}

// unlisted are the string types that get no enum from their constants:
// ErrorClass is open, because a reader must accept a value that no constant
// names, and Code gets its list from enumerateCodes.
var unlisted = map[string]bool{"ErrorClass": true, "Code": true}

// enumValues maps each closed string type of the protocol sources in the
// working directory to the values of its constants, which go generate sets to
// the protocol directory.
var enumValues = sync.OnceValues(func() (map[string][]string, error) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING && !unlisted[typ.Name] {
						val, err := strconv.Unquote(lit.Value)
						if err != nil {
							return nil, err
						}
						out[typ.Name] = append(out[typ.Name], val)
					}
				}
			}
		}
	}
	return out, nil
})

func mapper(t reflect.Type) *jsonschema.Schema {
	if t == reflect.TypeFor[json.RawMessage]() {
		return &jsonschema.Schema{}
	}
	if t == reflect.TypeFor[protocol.Rev]() {
		return &jsonschema.Schema{Type: "string"}
	}
	if t.Kind() == reflect.String && t.PkgPath() == reflect.TypeFor[protocol.Error]().PkgPath() {
		if values, err := enumValues(); err == nil && len(values[t.Name()]) > 0 {
			s := &jsonschema.Schema{Type: "string"}
			for _, v := range values[t.Name()] {
				s.Enum = append(s.Enum, v)
			}
			return s
		}
	}
	return nil
}

func newReflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{AllowAdditionalProperties: true, Anonymous: true, Mapper: mapper}
}

// components reflects every type that a route or a wire type names.
func components() (map[string]*jsonschema.Schema, error) {
	if _, err := enumValues(); err != nil {
		return nil, err
	}
	r := newReflector()
	defs := map[string]*jsonschema.Schema{}
	structs := map[string]reflect.Type{}
	for _, t := range rootTypes() {
		for name, s := range r.ReflectFromType(t).Definitions {
			defs[name] = s
		}
		if err := collectStructs(t, structs); err != nil {
			return nil, err
		}
	}
	for name, st := range structs {
		def, ok := defs[name]
		if !ok {
			return nil, fmt.Errorf("no schema for %s", name)
		}
		adjust(def, st)
	}
	if err := enumerateCodes(defs); err != nil {
		return nil, err
	}
	return defs, closeRequests(defs)
}

// enumerateCodes limits Error.code to the Code constants of the protocol sources in the working directory,
// which go generate sets to the protocol directory.
func enumerateCodes(defs map[string]*jsonschema.Schema) error {
	def, ok := defs[reflect.TypeFor[protocol.Error]().Name()]
	if !ok {
		return errors.New("no schema for the error")
	}
	code, ok := def.Properties.Get("code")
	if !ok {
		return errors.New("the error has no code")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		return err
	}
	var srcs []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		srcs = append(srcs, string(b))
	}
	codes, err := gates.ProtocolCodes(srcs...)
	if err != nil {
		return err
	}
	for _, c := range codes {
		code.Enum = append(code.Enum, c)
	}
	return nil
}

// closeRequests refuses unknown fields in each type that only a request body
// holds, because the handler answers an unknown request field with 400. A
// type that a reply holds too stays open, as a client must accept new fields
// in a reply.
func closeRequests(defs map[string]*jsonschema.Schema) error {
	requests, replies := map[string]reflect.Type{}, map[string]reflect.Type{}
	for _, r := range server.Table {
		for _, side := range []struct {
			body any
			into map[string]reflect.Type
		}{{r.Request, requests}, {r.Response, replies}} {
			if side.body == nil {
				continue
			}
			if err := collectStructs(reflect.TypeOf(side.body), side.into); err != nil {
				return err
			}
		}
	}
	for _, v := range wireTypes {
		if err := collectStructs(reflect.TypeOf(v), replies); err != nil {
			return err
		}
	}
	for name := range requests {
		if _, shared := replies[name]; !shared {
			defs[name].AdditionalProperties = jsonschema.FalseSchema
		}
	}
	return nil
}

// collectStructs records each named struct type that t reaches, by name, and
// fails when two types share a name, because a schema is named by its type.
func collectStructs(t reflect.Type, into map[string]reflect.Type) error {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return collectStructs(t.Elem(), into)
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() || t == reflect.TypeFor[protocol.Rev]() {
			return nil
		}
		if t.Name() == "" {
			return collectFields(t, into)
		}
		if seen, ok := into[t.Name()]; ok {
			if seen != t {
				return fmt.Errorf("%s and %s share the schema name %s", seen, t, t.Name())
			}
			return nil
		}
		into[t.Name()] = t
		return collectFields(t, into)
	}
	return nil
}

func collectFields(t reflect.Type, into map[string]reflect.Type) error {
	for i := range t.NumField() {
		if err := collectStructs(t.Field(i).Type, into); err != nil {
			return err
		}
	}
	return nil
}

// fields lists the fields of st that encoding/json writes, with the fields
// of an embedded struct in place.
func fields(st reflect.Type) []reflect.StructField {
	var out []reflect.StructField
	for i := range st.NumField() {
		f := st.Field(i)
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, fields(f.Type)...)
			continue
		}
		out = append(out, f)
	}
	return out
}

// adjust applies the facts that the reflector does not read from a json
// tag. A field tagged omitzero or optional:"true" is not required. A required
// pointer or map accepts null, because encoding/json writes a nil one as null.
func adjust(def *jsonschema.Schema, st reflect.Type) {
	for _, f := range fields(st) {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		flags := strings.Split(opts, ",")
		if slices.Contains(flags, "omitzero") || f.Tag.Get("optional") == "true" {
			def.Required = slices.DeleteFunc(def.Required, func(r string) bool { return r == name })
			continue
		}
		if k := f.Type.Kind(); (k != reflect.Map && k != reflect.Pointer) || slices.Contains(flags, "omitempty") {
			continue
		}
		if prop, ok := def.Properties.Get(name); ok {
			def.Properties.Set(name, &jsonschema.Schema{AnyOf: []*jsonschema.Schema{prop, {Type: "null"}}})
		}
	}
}

// schemaOf is the schema of a body of type v: a reference for a named
// struct, else the type inline.
func schemaOf(v any) *jsonschema.Schema {
	s := *newReflector().ReflectFromType(reflect.TypeOf(v))
	s.Version, s.Definitions = "", nil
	return &s
}

func pathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.Trim(seg, "{}"))
		}
	}
	return out
}

func parameters(r server.Route) []any {
	var params []any
	for _, p := range pathParams(r.Path) {
		params = append(params, map[string]any{"name": p, "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
	}
	for _, q := range r.Query {
		params = append(params, map[string]any{"name": q.Name, "in": "query", "required": false, "schema": map[string]any{"type": queryType(q)}})
	}
	return params
}

func queryType(q server.Param) string {
	if q.Integer {
		return "integer"
	}
	return "string"
}

func openAPI(comps map[string]*jsonschema.Schema) ([]byte, error) {
	paths := map[string]map[string]any{}
	for _, r := range server.Table {
		if paths[r.Path] == nil {
			paths[r.Path] = map[string]any{}
		}
		method := strings.ToLower(r.Method)
		op, ok := paths[r.Path][method].(map[string]any)
		if !ok {
			op = map[string]any{"operationId": r.Name, "responses": map[string]any{
				"default": map[string]any{"description": "Error", "content": jsonContent(protocol.ErrorBody{})},
			}}
			if params := parameters(r); params != nil {
				op["parameters"] = params
			}
			if r.Request != nil {
				op["requestBody"] = map[string]any{"required": false, "content": jsonContent(r.Request)}
			}
			paths[r.Path][method] = op
		}
		addSuccess(op["responses"].(map[string]any), r)
	}
	doc := map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "Harness protocol", "version": "0"},
		"paths":      paths,
		"components": map[string]any{"schemas": comps},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = bytes.ReplaceAll(raw, []byte(defsPrefix), []byte(componentsPrefix))
	return append(raw, '\n'), nil
}

func jsonContent(v any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schemaOf(v)}}
}

// addSuccess adds the reply of r to the success response of its operation.
// The page form and the SSE form of one route share an operation, each under
// its own media type.
func addSuccess(responses map[string]any, r server.Route) {
	key := fmt.Sprint(r.Status)
	ok, found := responses[key].(map[string]any)
	if !found {
		ok = map[string]any{"description": "Success"}
		responses[key] = ok
	}
	if r.Raw {
		ok["content"] = map[string]any{"*/*": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}
		return
	}
	if r.Response == nil {
		return
	}
	content, _ := ok["content"].(map[string]any)
	if content == nil {
		content = map[string]any{}
		ok["content"] = content
	}
	media := "application/json"
	if r.Stream {
		media = "text/event-stream"
	}
	content[media] = map[string]any{"schema": schemaOf(r.Response)}
}
