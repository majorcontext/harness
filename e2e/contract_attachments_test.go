package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
	"github.com/majorcontext/harness/store"
)

// onePixelPNG is a 1x1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// rowAttachments are the files that submitAttachments sends: an image and a PDF.
func rowAttachments() []attachment {
	png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	return []attachment{{"image/png", png}, {"application/pdf", []byte("%PDF-1.4\n%%EOF\n")}}
}

func TestContractAttachments(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:    "bifrost_prompt_attachments",
			chat:    true,
			model:   []harnesstest.Step{{Name: "reply", Reply: harnesstest.Reply{Text: "seen"}}},
			actions: []action{create{as: "a"}, submitAttachments{as: "a", text: "look"}, waitIdle{as: "a"}},
		},
		{
			name:    "claudecode_prompt_attachments",
			driver:  claudeLaneDriver("normal"),
			actions: []action{create{as: "a"}, submitAttachments{as: "a", text: "look"}, waitIdle{as: "a"}, claudeInputs{as: "a"}},
		},
	})
}

func TestContractCheckInputEqualsSubmit(t *testing.T) {
	skipShort(t)
	png, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	blob := func(mediaType string, data []byte) []protocol.Part {
		return []protocol.Part{{Type: protocol.PartBlob, MediaType: mediaType, Data: data}}
	}
	const noImages = "openai/o3-mini"
	image := protocol.Input{ID: "in_img", Parts: blob("image/png", png)}
	tooLargeData := make([]byte, (20<<20)+1)
	tooLarge := protocol.Input{ID: "in_big", Parts: blob("application/pdf", tooLargeData)}
	tooLargeBase64 := strings.Repeat("AAAA", len(tooLargeData)/3)
	rows := []struct {
		name string
		in   protocol.Input
		body any
	}{
		{"unsupported_media_type", protocol.Input{ID: "in_type", Parts: blob("application/zip", []byte("PK"))}, nil},
		{"image_bytes_that_do_not_decode", protocol.Input{ID: "in_bad", Parts: blob("image/png", []byte("not a png"))}, nil},
		{"too_large", tooLarge, rawBody(`{"id":"in_big","parts":[{"type":"blob","media_type":"application/pdf","data":"` + tooLargeBase64 + `"}]}`)},
		{"image_for_a_model_without_image_input", image, nil},
	}
	if err := harness.CheckInput("anthropic/claude-sonnet-4-5", image); err != nil {
		t.Errorf("CheckInput of a valid image = %v, want nil", err)
	}
	if err := harness.CheckInput("", image); err != nil {
		t.Errorf("CheckInput with no model = %v, want nil", err)
	}
	onHosts(t, func(t *testing.T, h host) {
		extra := map[string]any{"model": noImages, "providers": map[string]any{"openai": map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}}}
		d, _ := startOn(t, h, runtimeWorkdir(t, nil), extra, replyText("ok"))
		for _, row := range rows {
			t.Run(row.name, func(t *testing.T) {
				id := d.Create(t)
				before := len(d.events(t, id))
				requestBody := any(row.in)
				if row.body != nil {
					requestBody = row.body
				}
				res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", requestBody)
				if after := len(d.events(t, id)); after != before {
					t.Errorf("a rejected input appended %d records", after-before)
				}
				want, _ := bodyOf(t, res)["error"].(map[string]any)
				var got *protocol.Error
				if !errors.As(harness.CheckInput(noImages, row.in), &got) {
					t.Fatalf("CheckInput = %v, want a protocol.Error", got)
				}
				details, _ := want["details"].(map[string]any)
				if res.Status != http.StatusBadRequest || string(got.Code) != want["code"] || got.Message != want["message"] || got.Details["input_id"] != details["input_id"] {
					t.Errorf("CheckInput = %+v, Submit = %d %v", got, res.Status, want)
				}
			})
		}
	})
}

func TestContractFileAttachments(t *testing.T) {
	skipShort(t)
	rows := []struct {
		input, name, safe string
		data              []byte
	}{
		{"in_csv", "report.csv", "report.csv", []byte("a,b\n1,2\n")},
		{"in_evil", "../../etc/passwd", "etcpasswd", []byte("a,b\n1,2\n")},
		{"a/b", "data.csv", "data.csv", []byte("c,d\n3,4\n")},
		{"ab", "data.csv", "data.csv", []byte("e,f\n5,6\n")},
	}
	file := func(name string, data []byte) []protocol.Part {
		return []protocol.Part{{Type: protocol.PartText, Text: "read it"}, {Type: protocol.PartBlob, MediaType: "text/csv", Name: name, Data: data}}
	}
	onHosts(t, func(t *testing.T, h host) {
		dir := runtimeWorkdir(t, nil)
		pathOf := func(data []byte, safe string) string {
			sum := sha256.Sum256(data)
			return filepath.Join(dir, ".harness/attachments", hex.EncodeToString(sum[:]), safe)
		}
		var steps []harnesstest.Step
		for _, row := range rows {
			tag := `<harness-attachment name="` + row.name + `" path="` + pathOf(row.data, row.safe) + `" media_type="text/csv" bytes="8"/>`
			steps = append(steps, harnesstest.Step{Name: row.input, Match: anyPartContains(tag), Reply: harnesstest.Reply{Text: "read"}})
		}
		d, _ := startOn(t, h, dir, nil, steps...)
		id := d.Create(t)
		for _, row := range rows {
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", protocol.Input{ID: row.input, Parts: file(row.name, row.data)}, nil)
			d.WaitIdle(t, id)
			if got, err := os.ReadFile(pathOf(row.data, row.safe)); err != nil || !bytes.Equal(got, row.data) {
				t.Errorf("attachment of %s = %q, %v, want its own bytes inside the attachments directory", row.input, got, err)
			}
		}
		res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", protocol.Input{ID: "in_noname", Parts: file("", rows[0].data)})
		if res.Status != http.StatusBadRequest {
			t.Errorf("a file with no name = %d, want 400 invalid_request", res.Status)
		}
	})
}

// anyPartContains matches a request whose last message has a part with substr.
func anyPartContains(substr string) harnesstest.Matcher {
	return func(r harnesstest.Request) bool {
		return len(r.Messages) > 0 && slices.ContainsFunc(r.Messages[len(r.Messages)-1].Parts, func(p harnesstest.Part) bool { return strings.Contains(p.Text, substr) })
	}
}

func TestContractFileImageReachesATextOnlyModelAsATag(t *testing.T) {
	skipShort(t)
	t.Setenv("HARNESS_E2E_TOOL_KEY", "k")
	dir := runtimeWorkdir(t, nil)
	heic := []byte("heic bytes")
	sum := sha256.Sum256(heic)
	tag := `<harness-attachment name="photo.heic" path="` + filepath.Join(dir, ".harness/attachments", hex.EncodeToString(sum[:]), "photo.heic") + `" media_type="image/heic" bytes="10"/>`
	fake := harnesstest.NewChat(t, harnesstest.Step{Name: "tag", Match: anyPartContains(tag), Reply: harnesstest.Reply{Text: "ok"}})
	r, err := harness.New(harness.Options{Store: store.NewMemStore(), Getenv: os.Getenv, WorkDir: dir, Config: config.Config{ContextWindowTokens: 100000,
		Providers: map[string]config.Provider{"openai": {Type: config.TypeOpenAICompat, BaseURL: fake.URL(), APIKeyEnv: "HARNESS_E2E_TOOL_KEY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	const noImages = "openai/o3-mini"
	s, err := r.Create(t.Context(), protocol.CreateSession{ID: "s1", Model: noImages})
	if err != nil {
		t.Fatal(err)
	}
	in := protocol.Input{ID: "a", Parts: []protocol.Part{{Type: protocol.PartText, Text: "see"}, {Type: protocol.PartBlob, MediaType: "image/heic", Name: "photo.heic", Data: heic}}}
	if err := harness.CheckInput(noImages, in); err != nil {
		t.Errorf("CheckInput of a named image/heic for a model without image input = %v, want nil", err)
	}
	if _, err := s.Prompt(t.Context(), in); err != nil {
		t.Fatalf("Prompt = %v, want nil: a file is not an image input", err)
	}
	for e, err := range s.Events(t.Context(), 0) {
		if err != nil || e.Kind == "turn.ended" {
			break
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, ".harness/attachments", hex.EncodeToString(sum[:]), "photo.heic")); err != nil || !bytes.Equal(got, heic) {
		t.Errorf("file = %q, %v, want the HEIC bytes", got, err)
	}
	if reqs := fake.Requests(); len(reqs) != 1 {
		t.Errorf("requests = %d, want 1 that carries the tag", len(reqs))
	}
}
