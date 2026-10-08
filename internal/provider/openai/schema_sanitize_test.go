package openai

import (
	"encoding/json"
	"testing"
)

// mustSanitize round-trips a JSON Schema literal through
// sanitizeToolParameterSchema and unmarshals the result back into a generic
// map for assertion.
func mustSanitize(t *testing.T, schema string) map[string]interface{} {
	t.Helper()
	out := sanitizeToolParameterSchema(json.RawMessage(schema))
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal sanitized schema: %v\nraw: %s", err, out)
	}
	return got
}

// TestSanitizeToolSchemaDropsUnsupportedKeywords is the direct regression
// case: the ChatGPT Codex backend 400s on a `pattern` using regex
// lookaround ("Invalid JSON schema: regex lookaround is not supported.
// Found at $.properties.email.pattern"). Sanitizing must strip `pattern`,
// `format`, and `minLength` while preserving the surrounding structure,
// types, and required list.
func TestSanitizeToolSchemaDropsUnsupportedKeywords(t *testing.T) {
	got := mustSanitize(t, `{
		"type": "object",
		"properties": {
			"email": {
				"type": "string",
				"pattern": "^(?=.*@)[^\\s]+$",
				"format": "email",
				"minLength": 3,
				"description": "the user's email"
			},
			"name": {"type": "string"}
		},
		"required": ["email"]
	}`)

	props, ok := got["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties missing or wrong type: %#v", got["properties"])
	}
	email, ok := props["email"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties.email missing or wrong type: %#v", props["email"])
	}
	for _, key := range []string{"pattern", "format", "minLength"} {
		if _, present := email[key]; present {
			t.Errorf("properties.email retained %q, want dropped: %#v", key, email)
		}
	}
	if email["type"] != "string" {
		t.Errorf("properties.email.type = %v, want string", email["type"])
	}
	if email["description"] != "the user's email" {
		t.Errorf("properties.email.description = %v, want preserved", email["description"])
	}
	if got["type"] != "object" {
		t.Errorf("type = %v, want object", got["type"])
	}
	required, ok := got["required"].([]interface{})
	if !ok || len(required) != 1 || required[0] != "email" {
		t.Errorf("required = %#v, want [email]", got["required"])
	}
}

// TestSanitizeToolSchemaConstBecomesEnum mirrors opencode's const->enum
// rewrite: the Codex validator does not carry `const` through the same
// allowlist path as `enum`, so opencode always converts it.
func TestSanitizeToolSchemaConstBecomesEnum(t *testing.T) {
	got := mustSanitize(t, `{"type": "string", "const": "fixed"}`)
	enum, ok := got["enum"].([]interface{})
	if !ok || len(enum) != 1 || enum[0] != "fixed" {
		t.Errorf("enum = %#v, want [fixed]", got["enum"])
	}
	if _, present := got["const"]; present {
		t.Errorf("const retained, want dropped: %#v", got)
	}
}

// TestSanitizeToolSchemaNestedItemsAndAnyOf exercises recursion through
// `items` and a composition keyword (`anyOf`), and confirms an unsupported
// type on one branch is dropped while a supported branch survives.
func TestSanitizeToolSchemaNestedItemsAndAnyOf(t *testing.T) {
	got := mustSanitize(t, `{
		"type": "array",
		"items": {
			"anyOf": [
				{"type": "string", "pattern": "no"},
				{"type": "widget"}
			]
		}
	}`)
	if got["type"] != "array" {
		t.Fatalf("type = %v, want array", got["type"])
	}
	items, ok := got["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("items missing or wrong type: %#v", got["items"])
	}
	anyOf, ok := items["anyOf"].([]interface{})
	if !ok || len(anyOf) != 2 {
		t.Fatalf("anyOf = %#v, want 2 entries", items["anyOf"])
	}
	first, ok := anyOf[0].(map[string]interface{})
	if !ok || first["type"] != "string" {
		t.Errorf("anyOf[0] = %#v, want type string", anyOf[0])
	}
	if _, present := first["pattern"]; present {
		t.Errorf("anyOf[0] retained pattern, want dropped: %#v", first)
	}
	second, ok := anyOf[1].(map[string]interface{})
	if !ok {
		t.Fatalf("anyOf[1] wrong type: %#v", anyOf[1])
	}
	if _, present := second["type"]; present {
		t.Errorf("anyOf[1] retained unsupported type %q, want dropped entirely: %#v", second["type"], second)
	}
}

// TestSanitizeToolSchemaDefs exercises $defs recursion.
func TestSanitizeToolSchemaDefs(t *testing.T) {
	got := mustSanitize(t, `{
		"type": "object",
		"properties": {"thing": {"$ref": "#/$defs/Thing"}},
		"$defs": {
			"Thing": {"type": "string", "format": "uri"}
		}
	}`)
	defs, ok := got["$defs"].(map[string]interface{})
	if !ok {
		t.Fatalf("$defs missing or wrong type: %#v", got["$defs"])
	}
	thing, ok := defs["Thing"].(map[string]interface{})
	if !ok {
		t.Fatalf("$defs.Thing wrong type: %#v", defs["Thing"])
	}
	if thing["type"] != "string" {
		t.Errorf("$defs.Thing.type = %v, want string", thing["type"])
	}
	if _, present := thing["format"]; present {
		t.Errorf("$defs.Thing retained format, want dropped: %#v", thing)
	}
	props, ok := got["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("properties missing: %#v", got)
	}
	thingRef, ok := props["thing"].(map[string]interface{})
	if !ok || thingRef["$ref"] != "#/$defs/Thing" {
		t.Errorf("properties.thing = %#v, want $ref preserved", props["thing"])
	}
}

// TestSanitizeToolSchemaObjectGetsEmptyProperties: an object-typed node with
// no properties must gain an empty properties object, matching opencode's
// rebuild (some validators require it).
func TestSanitizeToolSchemaObjectGetsEmptyProperties(t *testing.T) {
	got := mustSanitize(t, `{"type": "object"}`)
	props, ok := got["properties"].(map[string]interface{})
	if !ok || len(props) != 0 {
		t.Errorf("properties = %#v, want empty object", got["properties"])
	}
}

// TestSanitizeToolSchemaArrayGetsStringItems: an array-typed node with no
// items must gain a default {"type":"string"} items schema.
func TestSanitizeToolSchemaArrayGetsStringItems(t *testing.T) {
	got := mustSanitize(t, `{"type": "array"}`)
	items, ok := got["items"].(map[string]interface{})
	if !ok || items["type"] != "string" {
		t.Errorf("items = %#v, want {type: string}", got["items"])
	}
}

// TestSanitizeToolSchemaInferredObjectFromProperties: no explicit "type",
// but "properties" is present — infer object, matching opencode.
func TestSanitizeToolSchemaInferredObjectFromProperties(t *testing.T) {
	got := mustSanitize(t, `{"properties": {"x": {"type": "string"}}}`)
	if got["type"] != "object" {
		t.Errorf("type = %v, want inferred object", got["type"])
	}
}

// TestSanitizeToolSchemaUnsupportedTypeDropsToEmpty: a node whose type is
// unsupported and that carries no inferable structure collapses to {}
// entirely, matching opencode's rebuild rather than emitting a
// half-populated node.
func TestSanitizeToolSchemaUnsupportedTypeDropsToEmpty(t *testing.T) {
	got := mustSanitize(t, `{"type": "widget", "description": "should vanish too"}`)
	if len(got) != 0 {
		t.Errorf("got %#v, want empty object (node dropped)", got)
	}
}

// TestSanitizeToolSchemaBooleanValueBecomesStringSchema: a bare JSON Schema
// boolean value (used as a subschema, e.g. additionalProperties or an items
// entry) becomes a permissive {"type":"string"} node, matching opencode.
func TestSanitizeToolSchemaBooleanValueBecomesStringSchema(t *testing.T) {
	got := mustSanitize(t, `{"type": "object", "additionalProperties": true}`)
	if got["additionalProperties"] != true {
		t.Errorf("additionalProperties = %v, want true preserved as-is", got["additionalProperties"])
	}

	got2 := mustSanitize(t, `{"type": "array", "items": false}`)
	items, ok := got2["items"].(map[string]interface{})
	if !ok || items["type"] != "string" {
		t.Errorf("items (from boolean false) = %#v, want {type: string}", got2["items"])
	}
}

// TestSanitizeToolSchemaEmptyOrUnparseablePassesThrough: an empty schema and
// malformed JSON must pass through unchanged rather than risk corrupting a
// tool definition this code does not understand.
func TestSanitizeToolSchemaEmptyOrUnparseablePassesThrough(t *testing.T) {
	if out := sanitizeToolParameterSchema(nil); out != nil {
		t.Errorf("nil input: got %q, want nil", out)
	}
	bad := json.RawMessage(`{not valid json`)
	if out := sanitizeToolParameterSchema(bad); string(out) != string(bad) {
		t.Errorf("malformed input: got %q, want unchanged %q", out, bad)
	}
}
