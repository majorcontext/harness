package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// acpSchemaDigest is the sha256 of testdata/acp/schema.json, the schema.json
// asset of the schema-v1.25.0 release of agentclientprotocol/agent-client-protocol.
const acpSchemaDigest = "4bcd1d2051ff7ed379bd9b234fb9b09c5ab9245478698e9fab805bd9b52a33a6"

// acpAssertions are the keywords whose rule the validator checks. acpAnnotations
// are the keywords it ignores. A keyword in neither list fails the load.
var (
	acpAssertions = []string{
		"$ref", "type", "properties", "required", "additionalProperties", "unevaluatedProperties",
		"items", "enum", "const", "oneOf", "anyOf", "allOf", "not", "minimum", "maximum", "minLength", "format",
	}
	acpAnnotations = []string{"$schema", "$defs", "description", "title", "default", "discriminator"}
)

// acpSchema validates a JSON value against the pinned ACP schema.
type acpSchema struct {
	root map[string]any
	defs map[string]any
}

// loadACPSchema reads the pinned schema, checks its digest, and fails on a
// keyword that the validator does not implement.
func loadACPSchema(t *testing.T) *acpSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "acp", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != acpSchemaDigest {
		t.Fatalf("testdata/acp/schema.json has sha256 %s, want the pinned %s", got, acpSchemaDigest)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	s := &acpSchema{root: root}
	s.defs, _ = root["$defs"].(map[string]any)
	if errs := s.audit(root, "#"); len(errs) > 0 {
		t.Fatalf("the pinned schema uses keywords that the validator lacks:\n%s", strings.Join(errs, "\n"))
	}
	return s
}

func acpKeywordKnown(k string) bool {
	return slices.Contains(acpAssertions, k) || slices.Contains(acpAnnotations, k) || strings.HasPrefix(k, "x-")
}

// audit walks every schema position and reports each keyword that is neither an
// assertion nor an annotation. Names under properties and $defs are not keywords.
func (s *acpSchema) audit(node any, at string) []string {
	sc, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	var errs []string
	for _, k := range sortedKeys(sc) {
		if !acpKeywordKnown(k) {
			errs = append(errs, at+": unknown keyword "+k)
		}
		switch v := sc[k].(type) {
		case map[string]any:
			switch k {
			case "properties", "$defs":
				for _, name := range sortedKeys(v) {
					errs = append(errs, s.audit(v[name], at+"/"+k+"/"+name)...)
				}
			case "items", "not", "additionalProperties", "unevaluatedProperties":
				errs = append(errs, s.audit(v, at+"/"+k)...)
			}
		case []any:
			if k == "oneOf" || k == "anyOf" || k == "allOf" {
				for i, alt := range v {
					errs = append(errs, s.audit(alt, fmt.Sprintf("%s/%s/%d", at, k, i))...)
				}
			}
		}
	}
	return errs
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// def validates v against the named definition of the schema.
func (s *acpSchema) def(name string, v any) []string {
	d, ok := s.defs[name]
	if !ok {
		return []string{"no definition " + name}
	}
	return s.validate(d, v, name)
}

// frame validates one JSON-RPC message against the whole-protocol schema. The
// first branch of the root describes what an agent sends, the second what a
// client sends.
func (s *acpSchema) frame(fromAgent bool, v any) []string {
	branches, _ := s.root["anyOf"].([]any)
	i := 1
	if fromAgent {
		i = 0
	}
	return s.validate(branches[i], v, "frame")
}

func (s *acpSchema) validate(node, v any, at string) []string {
	sc, ok := node.(map[string]any)
	if !ok {
		return []string{at + ": a schema must be an object"}
	}
	var errs []string
	for _, k := range sortedKeys(sc) {
		if !acpKeywordKnown(k) {
			errs = append(errs, at+": unknown keyword "+k)
			continue
		}
		errs = append(errs, s.assert(sc, k, v, at)...)
	}
	return errs
}

func (s *acpSchema) matches(node, v any) bool { return len(s.validate(node, v, "")) == 0 }

func (s *acpSchema) assert(sc map[string]any, k string, v any, at string) []string {
	arg := sc[k]
	switch k {
	case "$ref":
		name, ok := strings.CutPrefix(fmt.Sprint(arg), "#/$defs/")
		if !ok || s.defs[name] == nil {
			return []string{at + ": unresolved reference " + fmt.Sprint(arg)}
		}
		return s.validate(s.defs[name], v, at)
	case "type":
		return acpType(arg, v, at)
	case "enum":
		if list, _ := arg.([]any); !slices.ContainsFunc(list, func(e any) bool { return reflect.DeepEqual(e, v) }) {
			return []string{fmt.Sprintf("%s: %v is not one of %v", at, v, arg)}
		}
	case "const":
		if !reflect.DeepEqual(arg, v) {
			return []string{fmt.Sprintf("%s: %v is not %v", at, v, arg)}
		}
	case "oneOf", "anyOf", "allOf":
		return s.combine(k, arg, v, at)
	case "not":
		if s.matches(arg, v) {
			return []string{at + ": matches a schema under not"}
		}
	case "minimum", "maximum", "minLength", "format":
		return acpScalar(k, arg, v, at)
	default:
		return s.assertContainer(sc, k, v, at)
	}
	return nil
}

func (s *acpSchema) combine(k string, arg, v any, at string) []string {
	alts, _ := arg.([]any)
	var errs []string
	count := 0
	for i, alt := range alts {
		e := s.validate(alt, v, fmt.Sprintf("%s/%s[%d]", at, k, i))
		if len(e) == 0 {
			count++
		}
		errs = append(errs, e...)
	}
	switch {
	case k == "allOf":
		return errs
	case k == "anyOf" && count == 0, k == "oneOf" && count != 1:
		return []string{fmt.Sprintf("%s: %s matched %d alternatives: %s", at, k, count, strings.Join(errs, "; "))}
	}
	return nil
}

func (s *acpSchema) assertContainer(sc map[string]any, k string, v any, at string) []string {
	arg := sc[k]
	var errs []string
	switch x := v.(type) {
	case map[string]any:
		errs = s.assertObject(sc, k, x, at)
	case []any:
		if k == "items" {
			for i, e := range x {
				errs = append(errs, s.validate(arg, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	}
	return errs
}

func (s *acpSchema) assertObject(sc map[string]any, k string, x map[string]any, at string) []string {
	arg := sc[k]
	var errs []string
	switch k {
	case "required":
		for _, name := range arg.([]any) {
			if _, ok := x[name.(string)]; !ok {
				errs = append(errs, at+": missing required field "+name.(string))
			}
		}
	case "properties":
		props := arg.(map[string]any)
		for _, name := range sortedKeys(x) {
			if sub, ok := props[name]; ok {
				errs = append(errs, s.validate(sub, x[name], at+"."+name)...)
			}
		}
	case "additionalProperties":
		props, _ := sc["properties"].(map[string]any)
		for _, name := range sortedKeys(x) {
			if _, ok := props[name]; ok {
				continue
			}
			switch extra := arg.(type) {
			case bool:
				if !extra {
					errs = append(errs, at+": unexpected field "+name)
				}
			default:
				errs = append(errs, s.validate(extra, x[name], at+"."+name)...)
			}
		}
	case "unevaluatedProperties":
		if arg != true {
			errs = append(errs, at+": the validator accepts only unevaluatedProperties: true")
		}
	}
	return errs
}

func acpType(arg, v any, at string) []string {
	var want []string
	switch a := arg.(type) {
	case string:
		want = []string{a}
	case []any:
		for _, e := range a {
			want = append(want, fmt.Sprint(e))
		}
	}
	for _, w := range want {
		if acpHasType(w, v) {
			return nil
		}
	}
	return []string{fmt.Sprintf("%s: want %v, got %s", at, want, acpKind(v))}
}

func acpHasType(want string, v any) bool {
	switch want {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}

func acpKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	}
	return "number"
}

var acpIntRanges = map[string][2]float64{
	"int32": {math.MinInt32, math.MaxInt32}, "uint32": {0, math.MaxUint32},
	"int64": {math.MinInt64, math.MaxInt64}, "uint64": {0, math.MaxUint64},
	"uint16": {0, math.MaxUint16}, "double": {-math.MaxFloat64, math.MaxFloat64},
}

func acpScalar(k string, arg, v any, at string) []string {
	f, isNum := v.(float64)
	s, isStr := v.(string)
	switch {
	case k == "minimum" && isNum && f < arg.(float64):
		return []string{fmt.Sprintf("%s: %v is below %v", at, f, arg)}
	case k == "maximum" && isNum && f > arg.(float64):
		return []string{fmt.Sprintf("%s: %v is above %v", at, f, arg)}
	case k == "minLength" && isStr && float64(utf8.RuneCountInString(s)) < arg.(float64):
		return []string{fmt.Sprintf("%s: shorter than %v characters", at, arg)}
	case k != "format":
		return nil
	}
	name := arg.(string)
	if r, ok := acpIntRanges[name]; ok {
		if isNum && (f < r[0] || f > r[1]) {
			return []string{fmt.Sprintf("%s: %v is out of range for %s", at, f, name)}
		}
		return nil
	}
	if name != "uri" {
		return []string{at + ": unknown format " + name}
	}
	if _, err := url.Parse(s); isStr && (err != nil || s == "") {
		return []string{at + ": not a URI: " + s}
	}
	return nil
}

func TestACPSchemaValidatorKeywords(t *testing.T) {
	skipShort(t)
	t.Parallel()
	s := &acpSchema{defs: map[string]any{"Name": map[string]any{"type": "string"}}}
	cases := []struct {
		name   string
		schema string
		value  string
		ok     bool
	}{
		{"ref resolves into defs", `{"$ref":"#/$defs/Name"}`, `"x"`, true},
		{"ref rejects a wrong type", `{"$ref":"#/$defs/Name"}`, `1`, false},
		{"ref to a missing def fails", `{"$ref":"#/$defs/Gone"}`, `1`, false},
		{"type list admits null", `{"type":["string","null"]}`, `null`, true},
		{"type integer rejects a fraction", `{"type":"integer"}`, `1.5`, false},
		{"required names a missing field", `{"type":"object","required":["a"]}`, `{}`, false},
		{"properties checks a field", `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`, false},
		{"additionalProperties false rejects an extra field", `{"properties":{"a":{}},"additionalProperties":false}`, `{"b":1}`, false},
		{"additionalProperties true admits an extra field", `{"properties":{"a":{}},"additionalProperties":true}`, `{"b":1}`, true},
		{"additionalProperties schema checks an extra field", `{"additionalProperties":{"type":"string"}}`, `{"b":1}`, false},
		{"unevaluatedProperties true admits an extra field", `{"unevaluatedProperties":true}`, `{"b":1}`, true},
		{"unevaluatedProperties false is refused", `{"unevaluatedProperties":false}`, `{}`, false},
		{"items checks each element", `{"items":{"type":"integer"}}`, `[1,"a"]`, false},
		{"enum admits a listed value", `{"enum":["a","b"]}`, `"b"`, true},
		{"enum rejects another value", `{"enum":["a","b"]}`, `"c"`, false},
		{"const rejects another value", `{"const":-32700}`, `-32600`, false},
		{"oneOf rejects two matches", `{"oneOf":[{"type":"string"},{"minLength":1}]}`, `"x"`, false},
		{"oneOf admits one match", `{"oneOf":[{"const":"a"},{"const":"b"}]}`, `"a"`, true},
		{"anyOf rejects no match", `{"anyOf":[{"const":"a"},{"const":"b"}]}`, `"c"`, false},
		{"allOf needs every branch", `{"allOf":[{"type":"string"},{"minLength":2}]}`, `"x"`, false},
		{"not rejects a match", `{"not":{"type":"string"}}`, `"x"`, false},
		{"minimum rejects a smaller number", `{"minimum":0}`, `-1`, false},
		{"maximum rejects a larger number", `{"maximum":10}`, `11`, false},
		{"minLength counts characters", `{"minLength":2}`, `"é"`, false},
		{"format uint16 rejects 65536", `{"format":"uint16"}`, `65536`, false},
		{"format int32 admits a negative", `{"format":"int32"}`, `-32700`, true},
		{"format uri rejects an empty string", `{"format":"uri"}`, `""`, false},
		{"format ignores null", `{"type":["integer","null"],"format":"uint32"}`, `null`, true},
		{"unknown format fails", `{"format":"date"}`, `"x"`, false},
		{"annotations are ignored", `{"title":"t","description":"d","default":1,"x-method":"m","discriminator":{"propertyName":"p"}}`, `1`, true},
		{"an unknown keyword fails", `{"pattern":"a"}`, `"a"`, false},
		{"an unknown keyword fails inside a branch", `{"anyOf":[{"contains":{}}]}`, `[]`, false},
	}
	for _, tc := range cases {
		var schema, value any
		if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := json.Unmarshal([]byte(tc.value), &value); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if errs := s.validate(schema, value, "v"); (len(errs) == 0) != tc.ok {
			t.Errorf("%s: errors %v, want ok = %t", tc.name, errs, tc.ok)
		}
	}
}

func TestACPSchemaAuditFailsOnUnknownKeyword(t *testing.T) {
	skipShort(t)
	t.Parallel()
	s := &acpSchema{}
	var doc any
	text := `{"$defs":{"A":{"type":"object","properties":{"minimum":{"type":"integer"}},"oneOf":[{"pattern":"x"}]}}}`
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	errs := s.audit(doc, "#")
	if len(errs) != 1 || !strings.Contains(errs[0], "unknown keyword pattern") {
		t.Errorf("audit = %v, want one error for the keyword pattern and none for the property named minimum", errs)
	}
}

func TestACPPinnedSchemaChecksFrames(t *testing.T) {
	skipShort(t)
	t.Parallel()
	s := loadACPSchema(t)
	cases := []struct {
		name      string
		fromAgent bool
		frame     string
		ok        bool
	}{
		{"initialize request", false, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, true},
		{"request with no jsonrpc member", false, `{"id":1,"method":"initialize","params":{"protocolVersion":1}}`, false},
		{"initialize response", true, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`, true},
		{"error response", true, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`, true},
		{"error response with a code the protocol lacks", true, `{"jsonrpc":"2.0","id":1,"error":{"code":"x","message":"m"}}`, false},
		{"response with neither result nor error", true, `{"jsonrpc":"2.0","id":1}`, false},
	}
	for _, tc := range cases {
		var v any
		if err := json.Unmarshal([]byte(tc.frame), &v); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if errs := s.frame(tc.fromAgent, v); (len(errs) == 0) != tc.ok {
			t.Errorf("%s: errors %v, want ok = %t", tc.name, errs, tc.ok)
		}
	}
	var bad any
	if err := json.Unmarshal([]byte(`{"protocolVersion":1,"agentInfo":{"name":"harness"},"authMethods":[]}`), &bad); err != nil {
		t.Fatal(err)
	}
	if errs := s.def("InitializeResponse", bad); len(errs) == 0 {
		t.Error("InitializeResponse admits an agentInfo with no version")
	}
}
