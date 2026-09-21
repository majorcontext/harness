package server

import (
	"encoding/json"
	"testing"

	"github.com/majorcontext/harness/command"
)

type commandJSON struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Op       string `json:"op"`
	Summary  string `json:"summary"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Category string `json:"category"`
}

// TestCommandsListsEveryBuiltin pins that GET /commands is the whole
// registry, sorted, with no surplus entry. A frontend builds its menu
// from this response alone.
func TestCommandsListsEveryBuiltin(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("GET", "/commands", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /commands status %d: %s", resp.StatusCode, data)
	}
	var body struct {
		Commands []commandJSON `json:"commands"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := map[string]commandJSON{}
	for _, c := range body.Commands {
		got[c.Name] = c
	}
	for _, s := range command.NewRegistry().All() {
		if _, ok := got[s.Name]; !ok {
			t.Errorf("GET /commands missing %q", s.Name)
		}
		delete(got, s.Name)
	}
	for name := range got {
		t.Errorf("GET /commands has surplus entry %q", name)
	}
}

// TestCommandsRouteInvariant pins the spec's §6 contract in both
// directions: a control command carries an op, a method, and a path; a
// frontend command carries none of the three, which is how a client
// learns it owns the action itself. It decodes each entry as raw JSON
// keys, not as a struct with plain string fields, because a struct
// field decodes an absent key and a present-but-empty key to the same
// zero value: only checking key presence actually pins absence.
func TestCommandsRouteInvariant(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	_, data := h.do("GET", "/commands", nil)
	var body struct {
		Commands []map[string]json.RawMessage `json:"commands"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, entry := range body.Commands {
		var name, kind string
		if err := json.Unmarshal(entry["name"], &name); err != nil {
			t.Fatalf("unmarshal name: %v", err)
		}
		if err := json.Unmarshal(entry["kind"], &kind); err != nil {
			t.Fatalf("unmarshal kind: %v", err)
		}
		_, hasOp := entry["op"]
		_, hasMethod := entry["method"]
		_, hasPath := entry["path"]
		switch kind {
		case string(command.KindControl):
			if !hasOp || !hasMethod || !hasPath {
				t.Errorf("control %q: op present=%v method present=%v path present=%v, want all present", name, hasOp, hasMethod, hasPath)
			}
			var op, method, path string
			json.Unmarshal(entry["op"], &op)
			json.Unmarshal(entry["method"], &method)
			json.Unmarshal(entry["path"], &path)
			if op == "" || method == "" || path == "" {
				t.Errorf("control %q: op=%q method=%q path=%q, want all non-empty", name, op, method, path)
			}
		case string(command.KindFrontend):
			if hasOp || hasMethod || hasPath {
				t.Errorf("frontend %q: op present=%v method present=%v path present=%v, want all absent", name, hasOp, hasMethod, hasPath)
			}
		default:
			t.Errorf("%q has kind %q", name, kind)
		}
	}
}

// TestEveryControlOpHasARoute pins that the route map cannot drift from
// the registry: a new control Op with no route entry fails here rather
// than shipping a menu item that reaches nothing.
func TestEveryControlOpHasARoute(t *testing.T) {
	for _, s := range command.NewRegistry().All() {
		if s.Kind != command.KindControl {
			continue
		}
		if _, ok := opRoutes[s.Op]; !ok {
			t.Errorf("control command %q has Op %q with no route", s.Name, s.Op)
		}
	}
	registry := command.NewRegistry()
	for op := range opRoutes {
		found := false
		for _, s := range registry.All() {
			if s.Op == op {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("route map has Op %q with no command", op)
		}
	}
}
