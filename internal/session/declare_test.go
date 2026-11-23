package session

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/protocol"
)

func spec(name, desc, schema string) protocol.ToolSpec {
	return protocol.ToolSpec{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}

func TestDeclarationEncoding(t *testing.T) {
	a := declaration{Tools: []protocol.ToolSpec{spec("a", "first", `{"type":"object"}`), spec("b", "second", `{"type":"object"}`)}, Prompt: "p", System: []string{"sys"}}
	_, key, err := a.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, again, _ := (declaration{Tools: slices.Clone(a.Tools), Prompt: "p", System: []string{"sys"}}).encode(); again != key || !strings.HasPrefix(key, "prefix-") {
		t.Errorf("equal declarations gave keys %q and %q", key, again)
	}
	if _, swapped, _ := (declaration{Tools: []protocol.ToolSpec{a.Tools[1], a.Tools[0]}, Prompt: "p", System: []string{"sys"}}).encode(); swapped == key {
		t.Error("a changed tool order kept the key")
	}
	data, _, _ := a.encode()
	back, err := decodeDeclaration(data)
	if err != nil || !reflect.DeepEqual(back, a) {
		t.Errorf("decodeDeclaration(encode()) = %+v, %v; want %+v", back, err, a)
	}
	if _, _, err := (declaration{Prompt: strings.Repeat("x", maxDeclarationBytes)}).encode(); !errors.Is(err, errDeclarationTooLarge) {
		t.Errorf("a prompt of the bound gave %v, want errDeclarationTooLarge", err)
	}
	for _, row := range []struct {
		name string
		live []protocol.ToolSpec
		want bool
	}{
		{"a changed schema", []protocol.ToolSpec{spec("a", "first", `{"type":"string"}`), a.Tools[1]}, true},
		{"a changed description", []protocol.ToolSpec{spec("a", "other", `{"type":"object"}`), a.Tools[1]}, false},
		{"a schema that differs only in white space", []protocol.ToolSpec{spec("a", "first", "{ \"type\": \"object\" }"), a.Tools[1]}, false},
		{"an added tool", append(slices.Clone(a.Tools), spec("c", "", `{}`)), false},
		{"a missing tool", a.Tools[:1], false},
	} {
		if got := a.schemaChanged(row.live); got != row.want {
			t.Errorf("schemaChanged for %s = %v, want %v", row.name, got, row.want)
		}
	}
}
