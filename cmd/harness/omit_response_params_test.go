package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/message"
	"github.com/majorcontext/harness/provider"
	"github.com/majorcontext/harness/provider/openai"
)

// TestRegistryTypeOpenAIThreadsOmitResponseParams: a `type: "openai"` entry's
// omit_response_params must reach the built *openai.Client, exactly as
// ResponsesPath does — the seam a keyed second Responses provider routed to
// a strict upstream (e.g. the ChatGPT Codex backend) actually needs.
func TestRegistryTypeOpenAIThreadsOmitResponseParams(t *testing.T) {
	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {
			Type:               config.TypeOpenAI,
			BaseURL:            "https://gateway.example",
			APIKeyEnv:          "SECONDARY_API_KEY",
			OmitResponseParams: []string{"max_output_tokens", "temperature", "top_p", "metadata"},
		},
	}})
	c, ok := reg["secondary"].(*openai.Client)
	if !ok {
		t.Fatalf("secondary provider is %T, want *openai.Client", reg["secondary"])
	}
	want := []string{"max_output_tokens", "temperature", "top_p", "metadata"}
	if len(c.OmitResponseParams) != len(want) {
		t.Fatalf("OmitResponseParams = %v, want %v", c.OmitResponseParams, want)
	}
	for i, v := range want {
		if c.OmitResponseParams[i] != v {
			t.Errorf("OmitResponseParams[%d] = %q, want %q", i, c.OmitResponseParams[i], v)
		}
	}
}

// TestRegistryNativeOpenAIHonorsOmitResponseParams: the bare "openai" key
// builds the same adapter, so its omit_response_params must reach the
// client too — mirrors TestRegistryNativeOpenAIHonorsResponsesPath.
func TestRegistryNativeOpenAIHonorsOmitResponseParams(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"openai": {OmitResponseParams: []string{"max_output_tokens"}},
	}})
	c := reg[openai.Family].(*openai.Client)
	if len(c.OmitResponseParams) != 1 || c.OmitResponseParams[0] != "max_output_tokens" {
		t.Errorf("OmitResponseParams = %v, want [max_output_tokens]", c.OmitResponseParams)
	}
}

// TestRegistryOmitResponseParamsEmitsNoneOnWire drives the production path
// end to end: a provider configured to omit all four params must actually
// send a request body without them, over real HTTP through Client.Stream —
// the direct proof this feature fixes the Codex-backend 400, not just that
// config threads a value through.
func TestRegistryOmitResponseParamsEmitsNoneOnWire(t *testing.T) {
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
			Type:               config.TypeOpenAI,
			BaseURL:            srv.URL,
			APIKeyEnv:          "SECONDARY_API_KEY",
			OmitResponseParams: []string{"max_output_tokens", "temperature", "top_p", "metadata"},
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
	temp := 0.5
	stream, err := p.Stream(context.Background(), &provider.Request{
		Model:       ref,
		Messages:    []message.Message{{ID: "msg_1", Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hello"}}}},
		MaxTokens:   4096,
		Temperature: &temp,
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

	for _, field := range []string{"max_output_tokens", "temperature", "top_p", "metadata"} {
		if _, ok := gotBody[field]; ok {
			t.Errorf("request body has field %q, want it omitted", field)
		}
	}
	if _, ok := gotBody["model"]; !ok {
		t.Error("request body is missing model — omission must not touch required fields")
	}
}
