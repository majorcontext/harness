package gates

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/internal/server"
)

func TestErrorCodesAgreeAcrossTheirSources(t *testing.T) {
	files, err := filepath.Glob("../../protocol/*.go")
	if err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		srcs = append(srcs, string(b))
	}
	doc, err := os.ReadFile("../../protocol/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	constants, err := ProtocolCodes(srcs...)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := SpecErrorStatuses(readSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	enum, err := OpenAPICodeEnum(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range CheckErrorCodes(ErrorCodeSources{Constants: constants, Statuses: server.CodeStatuses(), Spec: spec, Enum: enum}) {
		t.Errorf("%s: %s: %s", v.Path, v.Rule, v.Detail)
	}
}

func TestErrorCodeRule(t *testing.T) {
	base := ErrorCodeSources{
		Constants: []string{"a", "b"},
		Statuses:  map[string]int{"a": 400, "b": 404},
		Spec:      map[string]int{"a": 400, "b": 404},
		Enum:      []string{"a", "b"},
	}
	mutate := func(f func(s *ErrorCodeSources)) ErrorCodeSources {
		s := ErrorCodeSources{Constants: slices.Clone(base.Constants), Statuses: maps.Clone(base.Statuses), Spec: maps.Clone(base.Spec), Enum: slices.Clone(base.Enum)}
		f(&s)
		return s
	}
	cases := []struct {
		name string
		in   ErrorCodeSources
		want []string
	}{
		{"agreeing_sources_pass", base, nil},
		{"a_constant_without_a_status", mutate(func(s *ErrorCodeSources) { s.Constants = append(s.Constants, "c") }), []string{"c"}},
		{"a_status_without_a_spec_row", mutate(func(s *ErrorCodeSources) { s.Statuses["c"] = 409 }), []string{"c"}},
		{"a_spec_row_without_a_constant", mutate(func(s *ErrorCodeSources) { s.Spec["c"] = 409 }), []string{"c"}},
		{"a_code_missing_from_the_enum", mutate(func(s *ErrorCodeSources) { s.Enum = s.Enum[:1] }), []string{"b"}},
		{"a_status_that_differs_from_the_spec", mutate(func(s *ErrorCodeSources) { s.Statuses["b"] = 409 }), []string{"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, v := range CheckErrorCodes(tc.in) {
				if !slices.Contains(got, v.Path) {
					got = append(got, v.Path)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("codes reported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProtocolCodesReadsTheCodeConstants(t *testing.T) {
	got, err := ProtocolCodes("package p\n\nconst (\n\tCodeB = \"b\"\n\tCodeA = \"a\"\n\tOther = \"x\"\n)\n", "package p\n\nconst CodeC = \"c\"\n")
	if err != nil || strings.Join(got, ",") != "a,b,c" {
		t.Errorf("ProtocolCodes = %v, %v", got, err)
	}
}
