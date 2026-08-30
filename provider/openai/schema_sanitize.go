package openai

import "encoding/json"

// supportedSchemaTypes are the JSON Schema "type" values the ChatGPT Codex
// backend's tool-schema validator accepts. Anything else is dropped by
// sanitizeSchemaObject, either falling back to inference or vanishing
// entirely — see that function.
var supportedSchemaTypes = map[string]bool{
	"string":  true,
	"number":  true,
	"boolean": true,
	"integer": true,
	"object":  true,
	"array":   true,
	"null":    true,
}

// compositionKeys are the JSON Schema keywords that combine subschemas.
// Each is recursed into like any other subschema slot, never validated
// against supportedSchemaTypes itself.
var compositionKeys = [...]string{"anyOf", "oneOf", "allOf"}

// sanitizeToolParameterSchema rewrites a tool's JSON Schema parameters for
// the ChatGPT Codex backend's stricter tool-schema validator, which rejects
// keywords the OpenAI platform API accepts without complaint — e.g. a
// regex `pattern` using lookaround, `format`, or `minLength` (confirmed
// live: "Invalid JSON schema: regex lookaround is not supported. Found at
// $.properties.email.pattern"). It rebuilds the schema by ALLOWLIST,
// keeping only the keywords sanitizeSchemaObject recognizes and dropping
// everything else, so an unsupported keyword cannot slip through by being
// merely unrecognized.
//
// Ported from opencode's sanitizeOpenAISchema (v1.18.23,
// codex-request-normalize.ts) — same allowlist, same type-inference
// fallback, same edge cases (boolean schema, const->enum, missing
// object/array defaults).
//
// An empty or unparseable schema passes through unchanged rather than risk
// corrupting a tool definition this code does not understand; only
// callers that have opted in via config.Provider.SanitizeToolSchemas
// invoke this at all (see Client.SanitizeToolSchemas / transcodeRequestFamily).
func sanitizeToolParameterSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var parsed interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return raw
	}
	sanitized, err := json.Marshal(sanitizeSchemaValue(parsed))
	if err != nil {
		return raw
	}
	return sanitized
}

// sanitizeSchemaValue is the recursive worker sanitizeToolParameterSchema
// drives, operating on values already decoded from JSON
// (map[string]interface{}, []interface{}, string, float64, bool, nil).
//
// A JSON Schema boolean value ("additionalProperties": true aside — that
// one is handled specially by its caller) such as a bare `true`/`false`
// subschema means "accept anything"/"accept nothing"; OpenCode's rebuild
// treats it as a permissive string schema rather than reproducing the
// boolean-schema form the Codex validator does not accept, so this mirrors
// that.
func sanitizeSchemaValue(value interface{}) interface{} {
	switch v := value.(type) {
	case bool:
		return map[string]interface{}{"type": "string"}
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = sanitizeSchemaValue(item)
		}
		return out
	case map[string]interface{}:
		return sanitizeSchemaObject(v)
	default:
		return value
	}
}

// sanitizeSchemaObject rebuilds one JSON Schema object node by allowlist.
// Keys not named below are dropped unconditionally — notably `pattern`,
// `format` (except as a string-type inference signal), `minLength`/
// `maxLength`, `default`, `examples`, and `title`.
func sanitizeSchemaObject(value map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}

	if ref, ok := value["$ref"].(string); ok {
		result["$ref"] = ref
	}
	if desc, ok := value["description"].(string); ok {
		result["description"] = desc
	}

	if constVal, ok := value["const"]; ok {
		result["enum"] = []interface{}{constVal}
	} else if enumVal, ok := value["enum"].([]interface{}); ok {
		result["enum"] = enumVal
	}

	if props, ok := value["properties"].(map[string]interface{}); ok {
		newProps := make(map[string]interface{}, len(props))
		for k, item := range props {
			newProps[k] = sanitizeSchemaValue(item)
		}
		result["properties"] = newProps
	}

	if reqVal, ok := value["required"].([]interface{}); ok {
		filtered := make([]interface{}, 0, len(reqVal))
		for _, item := range reqVal {
			if s, ok := item.(string); ok {
				filtered = append(filtered, s)
			}
		}
		result["required"] = filtered
	}

	if items, ok := value["items"]; ok {
		result["items"] = sanitizeSchemaValue(items)
	}

	if ap, ok := value["additionalProperties"]; ok {
		if b, isBool := ap.(bool); isBool {
			result["additionalProperties"] = b
		} else {
			result["additionalProperties"] = sanitizeSchemaValue(ap)
		}
	}

	for _, key := range compositionKeys {
		if arr, ok := value[key].([]interface{}); ok {
			out := make([]interface{}, len(arr))
			for i, item := range arr {
				out[i] = sanitizeSchemaValue(item)
			}
			result[key] = out
		}
	}

	for _, key := range [...]string{"$defs", "definitions"} {
		if defs, ok := value[key].(map[string]interface{}); ok {
			newDefs := make(map[string]interface{}, len(defs))
			for k, item := range defs {
				newDefs[k] = sanitizeSchemaValue(item)
			}
			result[key] = newDefs
		}
	}

	schemaTypes := supportedTypesOf(value["type"])

	if len(schemaTypes) == 0 && (hasStringKey(result, "$ref") || hasAnyCompositionKey(result)) {
		return result
	}

	inferredTypes := schemaTypes
	switch {
	case len(inferredTypes) > 0:
		// already resolved from the explicit "type"
	case hasAnyKey(value, "properties", "required", "additionalProperties"):
		inferredTypes = []string{"object"}
	case hasAnyKey(value, "items", "prefixItems"):
		inferredTypes = []string{"array"}
	case hasAnyKey(result, "enum") || hasAnyKey(value, "format"):
		inferredTypes = []string{"string"}
	case hasAnyKey(value, "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"):
		inferredTypes = []string{"number"}
	}

	if len(inferredTypes) == 0 {
		return map[string]interface{}{}
	}

	if len(inferredTypes) == 1 {
		result["type"] = inferredTypes[0]
	} else {
		typeArr := make([]interface{}, len(inferredTypes))
		for i, t := range inferredTypes {
			typeArr[i] = t
		}
		result["type"] = typeArr
	}

	if containsString(inferredTypes, "object") {
		if _, ok := result["properties"]; !ok {
			result["properties"] = map[string]interface{}{}
		}
	}
	if containsString(inferredTypes, "array") {
		if _, ok := result["items"]; !ok {
			result["items"] = map[string]interface{}{"type": "string"}
		}
	}

	return result
}

// supportedTypesOf resolves a decoded "type" value (a string, an array of
// strings, or anything else) to the subset of supportedSchemaTypes it
// names, in the same order, dropping unsupported entries rather than
// rejecting the whole schema.
func supportedTypesOf(rawType interface{}) []string {
	switch t := rawType.(type) {
	case string:
		if supportedSchemaTypes[t] {
			return []string{t}
		}
	case []interface{}:
		var out []string
		for _, item := range t {
			if s, ok := item.(string); ok && supportedSchemaTypes[s] {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func hasStringKey(m map[string]interface{}, key string) bool {
	_, ok := m[key].(string)
	return ok
}

func hasAnyCompositionKey(m map[string]interface{}) bool {
	for _, key := range compositionKeys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func hasAnyKey(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
