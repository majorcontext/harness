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

// TestRegistryTypeOpenAIBuildsKeyedNativeClient: a `type: "openai"` entry
// registers the native Responses adapter under its PROVIDERS-MAP KEY, not
// under the package family constant. The key is what routes a
// "<key>/<model>" ref, so keying it any other way would make the entry
// unreachable — or, worse, silently replace the built-in "openai" entry.
func TestRegistryTypeOpenAIBuildsKeyedNativeClient(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {
			Type:          config.TypeOpenAI,
			BaseURL:       "https://gateway.example",
			APIKeyEnv:     "SECONDARY_API_KEY",
			ResponsesPath: "/alt/responses",
		},
	}})

	c, ok := reg["secondary"].(*openai.Client)
	if !ok {
		t.Fatalf("secondary provider is %T, want *openai.Client", reg["secondary"])
	}
	if c.Family != "secondary" {
		t.Errorf("Family = %q, want the providers-map key %q", c.Family, "secondary")
	}
	if c.BaseURL != "https://gateway.example" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.APIKey != "sk-secondary" {
		t.Errorf("APIKey = %q, want sk-secondary", c.APIKey)
	}
	if c.ResponsesPath != "/alt/responses" {
		t.Errorf("ResponsesPath = %q, want /alt/responses", c.ResponsesPath)
	}

	// The built-in native entry must be untouched: a keyed entry ADDS a
	// provider, it never rebinds the "openai" key.
	builtin, ok := reg[openai.Family].(*openai.Client)
	if !ok {
		t.Fatalf("openai provider is %T, want *openai.Client", reg[openai.Family])
	}
	if builtin.APIKey != "sk-builtin" {
		t.Errorf("built-in openai APIKey = %q, want sk-builtin", builtin.APIKey)
	}
	if builtin.BaseURL != "" || builtin.ResponsesPath != "" {
		t.Errorf("built-in openai client = %+v, want the untouched defaults", builtin)
	}
}

// TestRegistryTypeOpenAIRoutesRefToConfiguredEndpoint drives the production
// path end to end: a "<key>/<model>" ref resolves through the registry and
// POSTs to the configured base URL AND request path, passing the model id
// through unchanged — exactly as an openai-compat entry does.
func TestRegistryTypeOpenAIRoutesRefToConfiguredEndpoint(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		gotModel = body.Model
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n") //nolint:errcheck
	}))
	defer srv.Close()

	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {
			Type:          config.TypeOpenAI,
			BaseURL:       srv.URL,
			APIKeyEnv:     "SECONDARY_API_KEY",
			ResponsesPath: "/backend/responses",
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

	if gotPath != "/backend/responses" {
		t.Errorf("path = %q, want /backend/responses", gotPath)
	}
	if gotAuth != "Bearer sk-secondary" {
		t.Errorf("Authorization = %q, want Bearer sk-secondary", gotAuth)
	}
	if gotModel != "gpt-5" {
		t.Errorf("model = %q, want the ref's model id passed through unchanged", gotModel)
	}
}

// TestRegistryTypeOpenAIDefaultResponsesPath: an entry that names no
// responses_path keeps the adapter's own /v1/responses default, so the new
// type is usable for an ordinary Responses endpoint with two config keys.
func TestRegistryTypeOpenAIDefaultResponsesPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n") //nolint:errcheck
	}))
	defer srv.Close()

	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {Type: config.TypeOpenAI, BaseURL: srv.URL, APIKeyEnv: "SECONDARY_API_KEY"},
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
	if gotPath != "/v1/responses" {
		t.Errorf("path = %q, want the default /v1/responses", gotPath)
	}
}

// TestRegistryTypeOpenAIUnderNativeKey covers the one shape where the two
// wiring paths address the same key: an entry keyed "openai" that also names
// type "openai". It is legal config, and it must apply WHOLE — base URL,
// path, and key together — rather than half-landing because the built-in
// entry supplied one field and the keyed entry another.
func TestRegistryTypeOpenAIUnderNativeKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"openai": {
			Type:          config.TypeOpenAI,
			BaseURL:       "https://gateway.example",
			APIKeyEnv:     "SECONDARY_API_KEY",
			ResponsesPath: "/alt/responses",
		},
	}})
	c, ok := reg[openai.Family].(*openai.Client)
	if !ok {
		t.Fatalf("openai provider is %T, want *openai.Client", reg[openai.Family])
	}
	if c.APIKey != "sk-secondary" || c.BaseURL != "https://gateway.example" || c.ResponsesPath != "/alt/responses" {
		t.Errorf("entry applied only in part: %+v", c)
	}
	// Family here equals the package constant, so this entry's reasoning
	// attachments stay tagged exactly as the built-in entry's would be.
	if got := c.Name(); got != openai.Family {
		t.Errorf("Name() = %q, want %q", got, openai.Family)
	}
}

// TestRegistryNativeOpenAIHonorsResponsesPath: the bare "openai" key builds
// the same adapter, so its responses_path must reach the client too.
func TestRegistryNativeOpenAIHonorsResponsesPath(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"openai": {BaseURL: "http://proxy", ResponsesPath: "/alt/responses"},
	}})
	c := reg[openai.Family].(*openai.Client)
	if c.ResponsesPath != "/alt/responses" {
		t.Errorf("ResponsesPath = %q, want /alt/responses", c.ResponsesPath)
	}
	if c.Family != "" && c.Family != openai.Family {
		t.Errorf("Family = %q, want the package default for the built-in entry", c.Family)
	}
}

// TestRegistryTypeOpenAIFallsBackToDefaultKeyEnv: an entry that names no
// api_key_env is asking for the adapter's default key source, not for no
// key at all. The bare "openai" entry has always read OPENAI_API_KEY that
// way (providerAuth), and a type:"openai" entry keyed "openai" REPLACES
// that built-in client — so without the same fallback, adding a type to an
// existing entry would silently unauthenticate every request it makes.
//
// An entry that must NOT receive that key names its own api_key_env; the
// case immediately below pins that.
func TestRegistryTypeOpenAIFallsBackToDefaultKeyEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"openai": {Type: config.TypeOpenAI, BaseURL: "https://gateway.example"},
	}})
	c, ok := reg[openai.Family].(*openai.Client)
	if !ok {
		t.Fatalf("openai provider is %T, want *openai.Client", reg[openai.Family])
	}
	if c.APIKey != "sk-builtin" {
		t.Errorf("APIKey = %q, want the OPENAI_API_KEY fallback %q", c.APIKey, "sk-builtin")
	}
}

// TestRegistryTypeOpenAIKeyedEntryFallsBackToDefaultKeyEnv holds the same
// rule for a non-native key, so the fallback is a property of the adapter
// rather than of one map key.
func TestRegistryTypeOpenAIKeyedEntryFallsBackToDefaultKeyEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {Type: config.TypeOpenAI, BaseURL: "https://gateway.example"},
	}})
	c, ok := reg["secondary"].(*openai.Client)
	if !ok {
		t.Fatalf("secondary provider is %T, want *openai.Client", reg["secondary"])
	}
	if c.APIKey != "sk-builtin" {
		t.Errorf("APIKey = %q, want the OPENAI_API_KEY fallback %q", c.APIKey, "sk-builtin")
	}
}

// TestRegistryTypeOpenAIExplicitKeyEnvWins is the other half: an explicit
// api_key_env is how a deployment keeps its real OpenAI key away from a
// third-party endpoint. It must win over the fallback, and an unset named
// variable must resolve empty rather than silently borrowing OPENAI_API_KEY.
func TestRegistryTypeOpenAIExplicitKeyEnvWins(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-builtin")
	t.Setenv("SECONDARY_API_KEY", "sk-secondary")
	reg := registry(&config.Config{Providers: map[string]config.Provider{
		"secondary": {Type: config.TypeOpenAI, BaseURL: "https://gateway.example", APIKeyEnv: "SECONDARY_API_KEY"},
		"tertiary":  {Type: config.TypeOpenAI, BaseURL: "https://other.example", APIKeyEnv: "UNSET_API_KEY"},
	}})
	if c := reg["secondary"].(*openai.Client); c.APIKey != "sk-secondary" {
		t.Errorf("secondary APIKey = %q, want sk-secondary", c.APIKey)
	}
	if c := reg["tertiary"].(*openai.Client); c.APIKey != "" {
		t.Errorf("tertiary APIKey = %q, want empty: an entry naming an unset variable must not borrow the default key", c.APIKey)
	}
}
