package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// newResourceSession builds a session with one MCP server registered under
// serverName.
func newResourceSession(t *testing.T, serverName string, srv *fakeMCPHTTPServer) *Session {
	t.Helper()
	return newResourceSessionMulti(t, map[string]*fakeMCPHTTPServer{serverName: srv})
}

// newResourceSessionMulti builds a session with one MCP server per entry.
func newResourceSessionMulti(t *testing.T, servers map[string]*fakeMCPHTTPServer) *Session {
	t.Helper()
	cfg := make(map[string]MCPServerConfig, len(servers))
	for name, srv := range servers {
		cfg[name] = MCPServerConfig{URL: srv.start(t)}
	}
	mgr := NewMCPManager(cfg)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	return NewSession(Config{MCP: mgr})
}

// TestMCPResourceToolsGate covers the visibility gate: absent from toolDefs
// with no resource-capable server, present with one.
func TestMCPResourceToolsGate(t *testing.T) {
	cases := []struct {
		name string
		srv  *fakeMCPHTTPServer
		want bool
	}{
		{"no resources capability", &fakeMCPHTTPServer{tools: []fakeMCPTool{{name: "ping"}}}, false},
		{"resources capability", &fakeMCPHTTPServer{resourcesCapability: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newResourceSession(t, "svc", tc.srv)
			var gotList, gotRead bool
			for _, d := range s.toolDefs(context.Background()) {
				gotList = gotList || d.Name == mcpListResourcesToolName
				gotRead = gotRead || d.Name == mcpReadResourceToolName
			}
			if gotList != tc.want || gotRead != tc.want {
				t.Fatalf("list present=%v read present=%v, want both %v", gotList, gotRead, tc.want)
			}
		})
	}
}

// TestMCPListResourcesFiltersAndMergesAcrossServers: no filter merges every
// resource-capable server, tagged by owning server; a filter restricts to one.
func TestMCPListResourcesFiltersAndMergesAcrossServers(t *testing.T) {
	s := newResourceSessionMulti(t, map[string]*fakeMCPHTTPServer{
		"figma": {resourcesCapability: true, resources: []map[string]any{resourceObj("skill://index.json", "index")}},
		"other": {resourcesCapability: true, resources: []map[string]any{resourceObj("skill://other.md", "other")}},
	})

	out, err := s.RunTool(context.Background(), mcpListResourcesToolName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_mcp_resources: %v", err)
	}
	var all mcpListResourcesResult
	if err := json.Unmarshal([]byte(out.Text()), &all); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, out.Text())
	}
	if len(all.Resources) != 2 {
		t.Fatalf("Resources = %+v, want 2 entries across both servers", all.Resources)
	}

	filtered, err := s.RunTool(context.Background(), mcpListResourcesToolName, json.RawMessage(`{"server":"figma"}`))
	if err != nil {
		t.Fatalf("list_mcp_resources(server=figma): %v", err)
	}
	var one mcpListResourcesResult
	if err := json.Unmarshal([]byte(filtered.Text()), &one); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, filtered.Text())
	}
	if len(one.Resources) != 1 || one.Resources[0].URI != "skill://index.json" || one.Resources[0].Server != "figma" {
		t.Fatalf("Resources = %+v, want exactly skill://index.json from figma", one.Resources)
	}
}

// TestMCPListResourcesUnfilteredToleratesOneServerFailing: an unfiltered
// listing returns every OTHER server's resources plus a per-server error
// entry naming the failing one, rather than failing the whole call.
func TestMCPListResourcesUnfilteredToleratesOneServerFailing(t *testing.T) {
	s := newResourceSessionMulti(t, map[string]*fakeMCPHTTPServer{
		"ok":      {resourcesCapability: true, resources: []map[string]any{resourceObj("skill://index.json", "index")}},
		"failing": {resourcesCapability: true, failResourcesList: true},
	})

	out, err := s.RunTool(context.Background(), mcpListResourcesToolName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("list_mcp_resources: %v", err)
	}
	var res mcpListResourcesResult
	if err := json.Unmarshal([]byte(out.Text()), &res); err != nil {
		t.Fatalf("result not valid JSON: %v (%s)", err, out.Text())
	}
	if len(res.Resources) != 1 || res.Resources[0].Server != "ok" {
		t.Fatalf("Resources = %+v, want the healthy server's resource despite the other failing", res.Resources)
	}
	if len(res.Errors) != 1 || res.Errors[0].Server != "failing" {
		t.Fatalf("Errors = %+v, want one entry naming %q", res.Errors, "failing")
	}
}

// TestMCPReadResourceContents: text contents come back verbatim; a blob
// becomes a short mimeType/size placeholder, never the raw base64 payload.
func TestMCPReadResourceContents(t *testing.T) {
	largeB64 := strings.Repeat("A", 4000) // valid, no padding: 3000 decoded bytes
	const blobB64 = "aGVsbG8td29ybGQ="    // "hello-world", 11 bytes

	cases := []struct {
		name            string
		contents        map[string]any
		want            []string
		wantNotContains string
	}{
		{"text", textResourceContents("skill://r", "application/json", `{"skills":["a"]}`), []string{`{"skills":["a"]}`}, ""},
		{"blob", blobResourceContents("skill://r", "image/png", blobB64), []string{"image/png", "11 bytes"}, blobB64},
		{"blob present but empty", blobResourceContents("skill://r", "image/png", ""), []string{"image/png", "0 bytes"}, ""},
		{"large blob sized without decoding", blobResourceContents("skill://r", "application/pdf", largeB64), []string{"3000 bytes"}, largeB64},
		{"malformed blob", blobResourceContents("skill://r", "image/png", "not-valid-base64!!"), []string{"malformed payload"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fakeMCPHTTPServer{
				resourcesCapability: true,
				resourceContents:    map[string][]map[string]any{"skill://r": {tc.contents}},
			}
			s := newResourceSession(t, "figma", srv)

			out, err := s.RunTool(context.Background(), mcpReadResourceToolName, json.RawMessage(`{"server":"figma","uri":"skill://r"}`))
			if err != nil {
				t.Fatalf("read_mcp_resource: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.Text(), want) {
					t.Errorf("output = %q, want it to contain %q", out.Text(), want)
				}
			}
			if tc.wantNotContains != "" && strings.Contains(out.Text(), tc.wantNotContains) {
				t.Errorf("output = %q, leaked the raw base64 blob", out.Text())
			}
		})
	}
}

// TestMCPReadResourceUnknownServer: an unconfigured server name is a clear
// tool error, never a panic.
func TestMCPReadResourceUnknownServer(t *testing.T) {
	s := newResourceSession(t, "figma", &fakeMCPHTTPServer{resourcesCapability: true})

	_, err := s.RunTool(context.Background(), mcpReadResourceToolName, json.RawMessage(`{"server":"nope","uri":"skill://index.json"}`))
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("err = %v, want a clear unknown-server error naming %q", err, "nope")
	}
}

// TestMCPResourceToolsRestrictTools: restrictTools must recognize the
// resource tools by name, and once restricted away they must neither be
// advertised nor runnable.
func TestMCPResourceToolsRestrictTools(t *testing.T) {
	s := newResourceSession(t, "figma", &fakeMCPHTTPServer{resourcesCapability: true})
	if err := restrictTools(s, []string{mcpListResourcesToolName, mcpReadResourceToolName}); err != nil {
		t.Fatalf("restrictTools(keep resource tools): %v", err)
	}

	s = newResourceSession(t, "figma", &fakeMCPHTTPServer{resourcesCapability: true})
	if err := restrictTools(s, []string{"bash"}); err != nil {
		t.Fatal(err)
	}
	for _, d := range s.toolDefs(context.Background()) {
		if d.Name == mcpListResourcesToolName || d.Name == mcpReadResourceToolName {
			t.Fatalf("toolDefs advertised %q after restrictTools removed it", d.Name)
		}
	}
	if _, err := s.RunTool(context.Background(), mcpListResourcesToolName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("list_mcp_resources ran after restrictTools removed it, want an error")
	}
}

// TestMCPBase64DecodedLenMatchesStdEncoding: mcpBase64DecodedLen's ok=false
// cases must match base64.StdEncoding.DecodeString's own rejections exactly,
// and its length must match len(decoded) whenever both accept the input.
func TestMCPBase64DecodedLenMatchesStdEncoding(t *testing.T) {
	cases := []struct {
		name string
		s    string
	}{
		{"empty", ""},
		{"no padding", "aGVsbG8td29ybGQ="},
		{"one pad", "aGVsbG8="},
		{"two pad", "aA=="},
		{"large", strings.Repeat("A", 4000)},
		{"invalid chars", "not-valid-base64!!"},
		{"length not multiple of 4", "A"},
		{"length not multiple of 4 (3)", "AAA"},
		{"padding mid-string", "AAAA="},
		{"embedded pad char", "A=AA"},
		{"all padding", "===="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded, decodeErr := base64.StdEncoding.DecodeString(tc.s)
			n, ok := mcpBase64DecodedLen(tc.s)
			if ok != (decodeErr == nil) {
				t.Fatalf("mcpBase64DecodedLen(%q) ok = %v, want %v (DecodeString err = %v)", tc.s, ok, decodeErr == nil, decodeErr)
			}
			if ok && n != len(decoded) {
				t.Errorf("mcpBase64DecodedLen(%q) = %d, want %d", tc.s, n, len(decoded))
			}
		})
	}
}
