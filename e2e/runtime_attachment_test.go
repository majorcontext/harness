package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// recorder keeps the body of each request that passes through it.
type recorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(body))
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(body))
	return http.DefaultTransport.RoundTrip(req)
}

// onePixelPNG is a 1x1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func TestRuntimeSendsAPromptAttachmentToTheModelAndKeepsItInTheStore(t *testing.T) {
	skipShort(t)
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}})
	rec, store := &recorder{}, harness.NewMemStore()
	r, err := harness.New(harness.Options{Store: store, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}},
		ModelTransport: func(string) http.RoundTripper { return rec }})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_E2E_KEY", "k")
	s, err := r.Create(context.Background(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	in := protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "look"}, {Type: protocol.PartBlob, MediaType: "image/png", Data: data}}}
	if _, err := s.Submit(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(context.Background(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 || !strings.Contains(rec.bodies[0], "data:image/png;base64,"+onePixelPNG) {
		t.Errorf("model request = %v, want the image as a data URL", rec.bodies)
	}
	recs, _ := store.Read(context.Background(), "s1", 0, 100)
	var log strings.Builder
	for _, rc := range recs {
		log.Write(rc.Data)
	}
	if !strings.Contains(log.String(), `"type":"blob","media_type":"image/png","blob_key":"attachment-`) || strings.Contains(log.String(), onePixelPNG) {
		t.Errorf("log = %s, want a blob part with a key and no bytes", log.String())
	}
}
