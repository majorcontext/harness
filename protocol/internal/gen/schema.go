package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/invopop/jsonschema"

	"github.com/majorcontext/harness/internal/server"
	"github.com/majorcontext/harness/protocol"
)

const (
	defsPrefix       = "#/$defs/"
	componentsPrefix = "#/components/schemas/"
)

// wireTypes are the types of the wire that no route names: the data of an
// ephemeral frame, and the replication call of Sync.
var wireTypes = []any{protocol.ItemFrame{}, protocol.StatusFrame{}, protocol.SyncBatch{}, protocol.SyncAck{}}

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

func mapper(t reflect.Type) *jsonschema.Schema {
	if t == reflect.TypeFor[json.RawMessage]() {
		return &jsonschema.Schema{}
	}
	return nil
}

func newReflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{AllowAdditionalProperties: true, Anonymous: true, Mapper: mapper}
}

// components reflects every type that a route or a wire type names.
func components() (map[string]*jsonschema.Schema, error) {
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
	return defs, closeRequests(defs)
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
		if t == reflect.TypeFor[time.Time]() {
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
