package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

// recordSync acknowledges each batch and keeps its blobs.
type recordSync struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func (s *recordSync) Deliver(_ context.Context, b protocol.SyncBatch) (protocol.SyncAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range b.Blobs {
		s.blobs[k] = v
	}
	return protocol.SyncAck{Head: b.FromSeq + uint64(len(b.Records)) - 1}, nil
}

// A Sync receiver builds the history of a session from the batches alone, so
// the batch of an input must carry the bytes of its attachments.
func TestSyncCarriesTheAttachmentOfAnInput(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}})
	rec := &recordSync{blobs: map[string][]byte{}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Sync: rec, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	pdf := rowAttachments()[1].data
	in := protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "read it"}, {Type: protocol.PartBlob, MediaType: "application/pdf", Data: pdf}}}
	if _, err := s.Submit(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pdf)
	if got := rec.blobs["attachment-"+hex.EncodeToString(sum[:])]; !slices.Equal(got, pdf) {
		t.Errorf("receiver blob = %q, want the attachment", got)
	}
}

// A Sync receiver also needs the blob of a retained tool result, because the
// history that it builds holds only the preview.
func TestSyncCarriesTheBlobOfARetainedResult(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "bash", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "bash", Input: ftArgs("command", seq5000)}}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("bash"), Reply: harnesstest.Reply{Text: "ok"}})
	rec := &recordSync{blobs: map[string][]byte{}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Sync: rec, WorkDir: t.TempDir(), Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var kept []byte
	for k, v := range rec.blobs {
		if strings.HasPrefix(k, "trh_1-") {
			kept = v
		}
	}
	if want := "1\n2\n"; len(kept) != 23893 || !strings.HasPrefix(string(kept), want) {
		t.Errorf("receiver blob of trh_1 = %d bytes starting %.10q, want the whole result of 23893 bytes", len(kept), kept)
	}
}

// A Sync receiver also needs the image of a tool result, because the history
// that it builds holds only the key of the blob.
func TestSyncCarriesTheImageOfAToolResult(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_KEY", "k")
	png := rowAttachments()[0].data
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := harnesstest.NewChat(t,
		harnesstest.Step{Name: "read", Match: harnesstest.LastUserText("go"), Reply: harnesstest.Reply{ToolCalls: []harnesstest.ToolCall{{ID: "call_1", Name: "read_file", Input: ftArgs("path", "a.png")}}}},
		harnesstest.Step{Name: "after", Match: harnesstest.LastToolResult("read_file"), Reply: harnesstest.Reply{Text: "ok"}})
	rec := &recordSync{blobs: map[string][]byte{}}
	r, err := harness.New(harness.Options{Store: harness.NewMemStore(), Sync: rec, WorkDir: dir, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"bifrost": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: "bifrost/gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(t.Context(), protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "go"}}}); err != nil {
		t.Fatal(err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == "turn.ended" {
			break
		}
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(png)
	if got := rec.blobs["toolblob-"+hex.EncodeToString(sum[:])]; !slices.Equal(got, png) {
		t.Errorf("receiver blob = %d bytes, want the %d bytes of the image", len(got), len(png))
	}
}
