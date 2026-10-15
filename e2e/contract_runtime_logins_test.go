package e2e

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/store"
)

// countingRT counts the requests that pass through it.
type countingRT struct{ n atomic.Int32 }

func (c *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestRuntimesWithoutSharedLoginsUseTheirOwnModelTransport(t *testing.T) {
	skipShort(t)
	o := harnesstest.NewOpenAI(t, harnesstest.OpenAIOptions{Injected: &harnesstest.InjectedOptions{}, Models: chatgptCatalog})
	cfg, err := config.Parse("config.json", []byte(`{"providers":{"codex":{"type":"openai","auth":"injected","base_url":"`+o.URL()+`/backend-api/codex","responses_path":"/responses","models_from":"chatgpt"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var transports [2]countingRT
	for i := range transports {
		rt := &transports[i]
		r, err := harness.New(harness.Options{Store: store.NewMemStore(), Config: *cfg, ModelTransport: func(string) http.RoundTripper { return rt }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close(t.Context()) })
		r.Models(t.Context())
	}
	for i := range transports {
		if n := transports[i].n.Load(); n != 1 {
			t.Errorf("runtime %d: its transport carried %d requests, want 1 (the model list)", i, n)
		}
	}
}
