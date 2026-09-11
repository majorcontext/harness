package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

type ambientMCPFake struct {
	body  string
	isErr bool
	calls int
}

func (f *ambientMCPFake) Tools(context.Context) []provider.ToolDef { return nil }
func (f *ambientMCPFake) CallTool(context.Context, string, json.RawMessage) (message.Parts, bool, error) {
	return nil, false, nil
}
func (f *ambientMCPFake) CallServerTool(_ context.Context, _ string, _ string, args json.RawMessage) (message.Parts, bool, error) {
	f.calls++
	if string(args) != "{}" {
		return nil, false, nil
	}
	return message.Parts{&message.Text{Text: f.body}}, f.isErr, nil
}

func TestAmbientMCPSourceRendersPinnedCatalog(t *testing.T) {
	f := &ambientMCPFake{body: `{"version":1,"hash":"h1","entries":[{"id":"b","revision_id":"2","qualified_label":"B","name":"B","description":"second"},{"id":"a","revision_id":"1","qualified_label":"A","name":"A","description":"first"}]}`}
	s := NewSession(Config{MCP: f, AmbientMCPSources: map[string]AmbientMCPSource{"skills": {Server: "boxes", Tool: "list_adopted_skills", Label: "adopted skills"}}})
	s.refreshAmbientMCPSources(context.Background())
	segments := s.ambientMCPSourceSegments()
	if f.calls != 1 || len(segments) != 1 || !strings.Contains(segments[0].text, `"id":"a"`) || strings.Index(segments[0].text, `"id":"a"`) > strings.Index(segments[0].text, `"id":"b"`) {
		t.Fatalf("calls=%d segments=%+v", f.calls, segments)
	}
	if segments[0].kind != "ambient_mcp:skills" {
		t.Errorf("kind=%q", segments[0].kind)
	}
}
func TestAmbientMCPSourceUnavailableContinues(t *testing.T) {
	f := &ambientMCPFake{isErr: true}
	s := NewSession(Config{MCP: f, AmbientMCPSources: map[string]AmbientMCPSource{"skills": {Server: "boxes", Tool: "list_adopted_skills"}}})
	s.refreshAmbientMCPSources(context.Background())
	if got := s.ambientMCPSourceSegments()[0].text; !strings.Contains(got, "unavailable") {
		t.Errorf("%q", got)
	}
}
