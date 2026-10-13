package gates

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// receiverCodes are the Sync receiver codes. The receiver is not the session
// API, so they are not rows of the Errors table.
var receiverCodes = map[string]bool{"CodeStaleEpoch": true, "CodeSyncConflict": true}

// ProtocolCodes returns the value of each error code constant of the protocol
// source: a constant of a const declaration whose name starts with "Code",
// except the Sync receiver codes.
func ProtocolCodes(srcs ...string) ([]string, error) {
	var out []string
	for _, src := range srcs {
		f, err := parser.ParseFile(token.NewFileSet(), "protocol.go", src, parser.SkipObjectResolution)
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
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Code") || receiverCodes[name.Name] || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return nil, fmt.Errorf("%s is not a string literal", name.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						return nil, err
					}
					out = append(out, v)
				}
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// ProtocolStatusCodes returns the value of each code constant that the
// statuses map literal of the protocol source names as a key, except the Sync
// receiver codes. It reads the table itself, so a code that HTTPStatus
// answers by its fallback is not listed.
func ProtocolStatusCodes(srcs ...string) ([]string, error) {
	values, keys := map[string]string{}, map[string]bool{}
	for _, src := range srcs {
		f, err := parser.ParseFile(token.NewFileSet(), "errors.go", src, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					switch {
					case gd.Tok == token.CONST && strings.HasPrefix(name.Name, "Code") && i < len(vs.Values):
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, err := strconv.Unquote(lit.Value)
							if err != nil {
								return nil, err
							}
							values[name.Name] = v
						}
					case gd.Tok == token.VAR && name.Name == "statuses" && i < len(vs.Values):
						if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
							for _, el := range lit.Elts {
								if kv, ok := el.(*ast.KeyValueExpr); ok {
									if id, ok := kv.Key.(*ast.Ident); ok {
										keys[id.Name] = true
									}
								}
							}
						}
					}
				}
			}
		}
	}
	var out []string
	for name := range keys {
		v, ok := values[name]
		if !ok {
			return nil, fmt.Errorf("the statuses map names %s, which is no Code constant", name)
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out, nil
}

// OpenAPICodeEnum returns the enum of Error.code in the OpenAPI document.
func OpenAPICodeEnum(doc []byte) ([]string, error) {
	var d struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Code struct {
							Enum []string `json:"enum"`
						} `json:"code"`
					} `json:"properties"`
				} `json:"Error"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	out := slices.Clone(d.Components.Schemas.Error.Properties.Code.Enum)
	slices.Sort(out)
	return out, nil
}

// ErrorCodeSources holds the four places that name the error codes: the
// protocol constants, protocol.HTTPStatus, the Errors table of
// the spec, and the enum of Error.code in protocol/openapi.json.
type ErrorCodeSources struct {
	Constants []string
	Statuses  map[string]int
	Spec      map[string]int
	Enum      []string
}

// CheckErrorCodes reports each code that the four places do not all name, and
// each code whose status differs between the server and the spec.
func CheckErrorCodes(s ErrorCodeSources) []Violation {
	var out []Violation
	add := func(code, detail string) {
		out = append(out, Violation{Path: code, Rule: "error_codes", Detail: detail})
	}
	all := map[string]bool{}
	for _, c := range s.Constants {
		all[c] = true
	}
	for c := range s.Statuses {
		all[c] = true
	}
	for c := range s.Spec {
		all[c] = true
	}
	for _, c := range s.Enum {
		all[c] = true
	}
	for _, c := range slices.Sorted(maps.Keys(all)) {
		_, inStatuses := s.Statuses[c]
		_, inSpec := s.Spec[c]
		for _, place := range []struct {
			name string
			has  bool
		}{
			{"a protocol Code constant", slices.Contains(s.Constants, c)},
			{"an entry of protocol.HTTPStatus", inStatuses},
			{"the Errors table of the spec", inSpec},
			{"the enum of Error.code in protocol/openapi.json", slices.Contains(s.Enum, c)},
		} {
			if !place.has {
				add(c, "is named elsewhere and is not "+place.name)
			}
		}
		if inStatuses && inSpec && s.Statuses[c] != s.Spec[c] {
			add(c, fmt.Sprintf("answers %d in protocol.HTTPStatus and %d in the Errors table of the spec", s.Statuses[c], s.Spec[c]))
		}
	}
	return out
}

// CheckACPErrorCodes reports each protocol error code that the Error mapping
// table of the spec lacks, each code whose JSON-RPC code differs from the
// mapping function of the adapter, and each row of a code that does not exist.
func CheckACPErrorCodes(constants []string, spec map[string]int, mapped func(code string) int) []Violation {
	var out []Violation
	add := func(code, detail string) {
		out = append(out, Violation{Path: code, Rule: "acp_error_codes", Detail: detail})
	}
	for _, c := range constants {
		want, ok := spec[c]
		switch {
		case !ok:
			add(c, "is a protocol code and is not a row of the Error mapping table of the spec")
		case mapped(c) != want:
			add(c, fmt.Sprintf("maps to %d in internal/acp and %d in the Error mapping table of the spec", mapped(c), want))
		}
	}
	for _, c := range slices.Sorted(maps.Keys(spec)) {
		if !slices.Contains(constants, c) {
			add(c, "is a row of the Error mapping table of the spec and is not a protocol code")
		}
	}
	return out
}
