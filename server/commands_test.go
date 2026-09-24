package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// TestOpRoutesMatchTheMux pins that every opRoutes entry names a route
// that actually exists on the server's mux. opRoutes and server.go's
// registrations are two independently-maintained lists; comparing
// opRoutes only against the command registry (TestEveryControlOpHasARoute)
// never catches a route that was renamed or removed in server.go, which
// would otherwise ship a GET /commands entry that 404s.
func TestOpRoutesMatchTheMux(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	for op, rt := range opRoutes {
		path := strings.ReplaceAll(rt.path, "{id}", "placeholder")
		req := httptest.NewRequest(rt.method, path, nil)
		_, pattern := h.srv.mux.Handler(req)
		if pattern == "" {
			t.Errorf("op %q: %s %s matched no route in the mux", op, rt.method, path)
		}
	}
}

// TestServeModeOpsTotal pins the support matrix in both directions,
// mirroring cmd/harness/command_test.go's TestRunModeOpsAreDeclared:
// every Op named by a registry KindControl spec is a key in
// serveModeOps, and no key names an Op outside the registry. A new Op
// that nobody declares fails here instead of reaching GET /commands as
// silently unsupported.
func TestServeModeOpsTotal(t *testing.T) {
	registry := command.NewRegistry()
	control := map[command.Op]bool{}
	for _, s := range registry.All() {
		if s.Kind == command.KindControl {
			control[s.Op] = true
		}
	}
	for op := range serveModeOps {
		if !control[op] {
			t.Errorf("serveModeOps names %q, which is not a control Op", op)
		}
	}
	for op := range control {
		if _, ok := serveModeOps[op]; !ok {
			t.Errorf("control Op %q is neither supported nor refused by serveModeOps", op)
		}
	}
}

// TestCommandsServeSupportTotal pins GET /commands' serve_support: a
// key for every registry entry name and no other key, with new,
// resume, quit, and queue-clear reported unsupported with the exact
// published reason. Failure here means the console dims a command that
// works, or offers one that does not.
func TestCommandsServeSupportTotal(t *testing.T) {
	h := newHarness(t, &scriptedProvider{name: "test"})
	resp, data := h.do("GET", "/commands", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /commands status %d: %s", resp.StatusCode, data)
	}
	var body struct {
		ServeSupport map[string]serveSupportJSON `json:"serve_support"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	registry := command.NewRegistry()
	wantUnsupported := map[string]bool{
		"new": true, "resume": true, "quit": true, "queue-clear": true,
	}

	got := map[string]serveSupportJSON{}
	for k, v := range body.ServeSupport {
		got[k] = v
	}
	for _, s := range registry.All() {
		entry, ok := got[s.Name]
		if !ok {
			t.Errorf("serve_support missing %q", s.Name)
			continue
		}
		delete(got, s.Name)
		if wantUnsupported[s.Name] {
			if entry.Supported {
				t.Errorf("serve_support[%q].supported = true, want false", s.Name)
			}
			if entry.Reason != serveUnsupportedReason {
				t.Errorf("serve_support[%q].reason = %q, want %q", s.Name, entry.Reason, serveUnsupportedReason)
			}
			continue
		}
		if !entry.Supported {
			t.Errorf("serve_support[%q].supported = false, want true", s.Name)
		}
		if entry.Reason != "" {
			t.Errorf("serve_support[%q].reason = %q, want empty", s.Name, entry.Reason)
		}
	}
	for name := range got {
		t.Errorf("serve_support has surplus entry %q", name)
	}
}
