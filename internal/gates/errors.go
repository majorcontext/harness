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

// ProtocolCodes returns the value of each error code constant of the protocol
// source: a constant of a const declaration whose name starts with "Code".
func ProtocolCodes(src string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "protocol.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Code") || i >= len(vs.Values) {
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
// protocol constants, the status map of internal/server, the Errors table of
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
			{"the status map of internal/server", inStatuses},
			{"the Errors table of the spec", inSpec},
			{"the enum of Error.code in protocol/openapi.json", slices.Contains(s.Enum, c)},
		} {
			if !place.has {
				add(c, "is named elsewhere and is not "+place.name)
			}
		}
		if inStatuses && inSpec && s.Statuses[c] != s.Spec[c] {
			add(c, fmt.Sprintf("answers %d in internal/server and %d in the Errors table of the spec", s.Statuses[c], s.Spec[c]))
		}
	}
	return out
}
