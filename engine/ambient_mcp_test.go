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
func TestDelegatedAmbientMCPCatalogFingerprintUsesRenderedState(t *testing.T) {
	s := NewSession(Config{AmbientMCPSources: map[string]AmbientMCPSource{"skills": {Label: "skills"}}})
	s.mu.Lock()
	s.claudeCodeCLISessionID = "cli"
	s.ambientMCPSources = map[string]ambientMCPSourceSnapshot{
		"skills": {catalog: ambientMCPCatalog{Version: 1, Entries: []ambientMCPEntry{{ID: "id", RevisionID: "1", QualifiedLabel: "skill", Name: "Skill", Description: "test"}}}},
	}
	text := renderAmbientMCPCatalog("skills", "skills", s.ambientMCPSources["skills"])
	s.delegatedAmbientMCPSourceHashes = map[string]string{"skills": delegatedAmbientMCPFingerprint(s.ambientMCPSources["skills"], text)}
	s.mu.Unlock()
	if got := s.delegatedAmbientMCPSourceSegments(); len(got) != 0 {
		t.Fatalf("unchanged rendered catalog = %q, want suppressed", got)
	}
}

func TestDelegatedAmbientMCPCatalogResendsUntilCLIIdentityIsDurable(t *testing.T) {
	s := NewSession(Config{AmbientMCPSources: map[string]AmbientMCPSource{"skills": {Label: "skills"}}})
	s.mu.Lock()
	s.ambientMCPSources = map[string]ambientMCPSourceSnapshot{"skills": {catalog: ambientMCPCatalog{Version: 1}}}
	text := renderAmbientMCPCatalog("skills", "skills", s.ambientMCPSources["skills"])
	s.delegatedAmbientMCPSourceHashes = map[string]string{"skills": delegatedAmbientMCPFingerprint(s.ambientMCPSources["skills"], text)}
	s.mu.Unlock()
	if got := s.delegatedAmbientMCPSourceSegments(); len(got) != 1 {
		t.Fatalf("uncertain CLI identity suppressed catalog: %q", got)
	}
}

func TestDelegatedAmbientMCPRemovalEmitsRevocation(t *testing.T) {
	s := NewSession(Config{})
	s.mu.Lock()
	s.claudeCodeCLISessionID = "cli"
	s.delegatedAmbientMCPSourceHashes = map[string]string{"skills": "available:old"}
	s.mu.Unlock()
	got := s.delegatedAmbientMCPSourceSegments()
	if len(got) != 1 || !strings.Contains(got[0], "revoked") {
		t.Fatalf("removed source segments = %q, want revocation", got)
	}
}

func TestAmbientMCPCatalogAcceptsOptionalLiveAndUnknownFields(t *testing.T) {
	var catalog ambientMCPCatalog
	if err := decodeAmbientMCPCatalog(`{"version":1,"hash":"opaque","future":true,"entries":[],"live":false}`, &catalog); err != nil {
		t.Fatalf("decodeAmbientMCPCatalog: %v", err)
	}
	if !sanitizeAmbientMCPCatalog(&catalog) {
		t.Fatal("sanitizeAmbientMCPCatalog rejected optional or unknown fields")
	}
}

func TestDelegatedAmbientMCPDeliveryDoesNotAdvanceCursorWhenJournalFails(t *testing.T) {
	s := NewSession(Config{SessionDir: t.TempDir()})
	s.append(message.Message{ID: "msg_seed", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "seed"}}})
	s.mu.Lock()
	s.ambientMCPSources = map[string]ambientMCPSourceSnapshot{
		"skills": {catalog: ambientMCPCatalog{Version: 1}},
	}
	if err := s.logFile.Close(); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()

	s.markDelegatedAmbientMCPDelivered()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastPersistErr == nil {
		t.Fatal("cursor journal write unexpectedly succeeded")
	}
	if s.delegatedAmbientMCPSourceHashes != nil {
		t.Fatalf("in-memory cursor changed after a failed journal write: %+v", s.delegatedAmbientMCPSourceHashes)
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
