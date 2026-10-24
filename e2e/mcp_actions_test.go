package e2e

import (
	"reflect"
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// expectToolsStable checks that the tools array of every model request equals that of the first.
type expectToolsStable struct{}

func (expectToolsStable) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.RawRequests()
	if len(reqs) < 3 {
		t.Fatalf("model requests = %d, want at least 3", len(reqs))
	}
	want := first(reqs[0], "tools")
	for i, req := range reqs[1:] {
		if got := first(req, "tools"); string(got) != string(want) {
			t.Errorf("request %d: tools differ from request 1:\n got %s\nwant %s", i+2, got, want)
		}
	}
}

type expectMCPCalls struct {
	server  string
	session string
	want    []harnesstest.MCPCall
}

func (a expectMCPCalls) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	want := slices.Clone(a.want)
	if a.session != "" {
		for i := range want {
			want[i].SessionIDHeader = r.ids[a.session]
		}
	}
	if got := srv.Calls(); !reflect.DeepEqual(got, want) {
		t.Errorf("server %s calls = %+v, want %+v", a.server, got, want)
	}
}

type expectNoMCPToolCall struct{ server string }

func (a expectNoMCPToolCall) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	for _, c := range srv.Calls() {
		if c.Method == "tools/call" {
			t.Errorf("server %s got a tools/call for %v", a.server, c.Name)
		}
	}
}

type stopMCPServer struct{ server string }

func (a stopMCPServer) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	srv.Close()
}

type refuseMCPServer struct{ server string }

func (a refuseMCPServer) run(t *testing.T, r *run) {
	t.Helper()
	srv, ok := r.fx[a.server].(*harnesstest.MCPServer)
	if !ok {
		t.Fatalf("no HTTP MCP server %q", a.server)
	}
	srv.SetAvailable(false)
}
