package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
)

// TestRegistryTypeOpenAIThreadsSanitizeToolSchemas: a `type: "openai"`
// entry's sanitize_tool_schemas must reach the built *openai.Client,
// exactly as OmitResponseParams and ResponsesPath do.
func TestRegistryTypeOpenAIThreadsSanitizeToolSchemas(t *testing.T) {
	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {
			Type:                config.TypeOpenAI,
			BaseURL:             "https://gateway.example",
			APIKeyEnv:           "SECONDARY_API_KEY",
			SanitizeToolSchemas: true,
		},
	}})
	c, ok := reg["secondary"].(*openai.Client)
	if !ok {
		t.Fatalf("secondary provider is %T, want *openai.Client", reg["secondary"])
	}
	if !c.SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true")
	}
}

// TestRegistryNativeOpenAIHonorsSanitizeToolSchemas: the bare "openai" key
// builds the same adapter, so its sanitize_tool_schemas must reach the
// client too — mirrors TestRegistryNativeOpenAIHonorsOmitResponseParams.
func TestRegistryNativeOpenAIHonorsSanitizeToolSchemas(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"openai": {SanitizeToolSchemas: true},
	}})
	c := reg[openai.Family].(*openai.Client)
	if !c.SanitizeToolSchemas {
		t.Error("SanitizeToolSchemas = false, want true")
	}
}

// TestRegistrySanitizeToolSchemasEmitsCleanSchemaOnWire drives the
// production path end to end: a provider configured with
// sanitize_tool_schemas:true must actually send a tool parameter schema
// with `pattern` stripped, over real HTTP through Client.Stream — the
// direct proof this feature fixes the Codex-backend 400 ("Invalid JSON
// schema: regex lookaround is not supported"), not just that config
// threads a value through.
func TestRegistrySanitizeToolSchemasEmitsCleanSchemaOnWire(t *testing.T) {
	var gotBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n") //nolint:errcheck
	}))
	defer srv.Close()

	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {
			Type:                config.TypeOpenAI,
			BaseURL:             srv.URL,
			APIKeyEnv:           "SECONDARY_API_KEY",
			SanitizeToolSchemas: true,
		},
	}})
	ref, err := message.ParseModelRef("secondary/gpt-5")
	if err != nil {
		t.Fatalf("ParseModelRef: %v", err)
	}
	p, err := reg.For(ref)
	if err != nil {
		t.Fatalf("reg.For: %v", err)
	}
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:    ref,
		Messages: []message.Message{{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hello"}}}},
		Tools: []provider.ToolDef{{
			Name: "send_email",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {"email": {"type": "string", "pattern": "^(?=.*@).+$"}},
				"required": ["email"]
			}`),
		}},
		MaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Next: %v", err)
		}
	}

	toolsRaw, ok := gotBody["tools"]
	if !ok {
		t.Fatal("request body has no tools field")
	}
	if strings.Contains(string(toolsRaw), `"pattern"`) {
		t.Errorf("wire tools body contains pattern, want stripped: %s", toolsRaw)
	}
	var tools []struct {
		Parameters map[string]interface{} `json:"parameters"`
	}
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		t.Fatalf("unmarshal tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1 entry", tools)
	}
	if tools[0].Parameters["type"] != "object" {
		t.Errorf("parameters.type = %v, want object preserved", tools[0].Parameters["type"])
	}
}
