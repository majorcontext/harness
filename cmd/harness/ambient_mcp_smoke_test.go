package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/engine"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
)

func TestAmbientMCPSourcesConfigToEngineSmoke(t *testing.T) {
	if _, err := os.Stat("/tmp/box-skills-mcp"); errors.Is(err, os.ErrNotExist) {
		t.Skip("box-skills-mcp fixture is unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotAuth string
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"content_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","entries":[{"skill_id":"skill_test","revision_id":"revision_test","qualified_label":"shared:test/review","name":"review","description":"Review code."}]}`)
	}))
	t.Cleanup(fixture.Close)

	workDir := t.TempDir()
	t.Setenv("HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	configPath := filepath.Join(workDir, ".harness.json")
	configJSON := fmt.Sprintf(`{
		"mcp_servers": {"boxes": {
			"command": ["/tmp/box-skills-mcp"],
			"env": ["BOXES_SKILLS_URL=%s", "BOXES_SKILLS_TOKEN=fake", "BOX_ID=box_fixture"]
		}},
		"ambient_mcp_sources": {"skills": {"server": "boxes", "tool": "list_adopted_skills"}}
	}`, fixture.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadProject(workDir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}

	mcpMgr := buildMCPManager(cfg.MCPServers)
	t.Cleanup(func() { closeMCPManager(mcpMgr) })
	prov := &scriptedProvider{name: "test"}
	sess := engine.NewSession(engine.Config{
		Providers:            provider.Registry{"test": prov},
		Model:                message.ModelRef{Provider: "test", Model: "m1"},
		WorkDir:              workDir,
		MCP:                  mcpRegistry(mcpMgr),
		AmbientMCPSources:    ambientMCPSources(cfg.AmbientMCPSources),
		RequireContextWindow: false,
		Instructions:         &engine.InstructionsConfig{Disabled: true},
		SkillsDirs:           []string{},
	})
	if _, err := sess.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if gotPath != "/v1/boxes/box_fixture/skills" {
		t.Errorf("fixture path = %q, want /v1/boxes/box_fixture/skills", gotPath)
	}
	if gotAuth != "Bearer fake" {
		t.Errorf("fixture Authorization = %q, want Bearer fake", gotAuth)
	}
	if len(prov.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(prov.requests))
	}
	var contextText string
	for _, msg := range prov.requests[0].Messages {
		for _, part := range msg.Parts {
			if ambient, ok := part.(*message.EngineContext); ok {
				contextText += ambient.Text
			}
		}
	}
	for _, want := range []string{"Adopted skills catalog (skills)", "skill_test", "Review code."} {
		if !strings.Contains(contextText, want) {
			t.Errorf("EngineContext = %q, want %q", contextText, want)
		}
	}
}
