package e2e

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
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
	csv := []byte("a,b\n1,2\n")
	file := func(name string) []protocol.Part {
		return []protocol.Part{{Type: protocol.PartText, Text: "read it"}, {Type: protocol.PartBlob, MediaType: "text/csv", Name: name, Data: csv}}
	}
	onHosts(t, func(t *testing.T, h host) {
		dir := runtimeWorkdir(t, nil)
		tag := func(name, rel string) string {
			return `<harness-attachment name="` + name + `" path="` + filepath.Join(dir, ".harness/attachments", rel) + `" media_type="text/csv" bytes="8"/>`
		}
		d, _ := startOn(t, h, dir, nil,
			harnesstest.Step{Name: "csv", Match: anyPartContains(tag("report.csv", "in_csv/report.csv")), Reply: harnesstest.Reply{Text: "read"}},
			harnesstest.Step{Name: "same_name", Match: func(r harnesstest.Request) bool {
				return anyPartContains(tag("data.csv", "in_dup/data.csv"))(r) && anyPartContains(tag("data.csv", "in_dup/data-2.csv"))(r)
			}, Reply: harnesstest.Reply{Text: "read"}},
			harnesstest.Step{Name: "hostile", Match: anyPartContains(tag("../../etc/passwd", "in_evil/etcpasswd")), Reply: harnesstest.Reply{Text: "read"}})
		id := d.Create(t)
		for _, row := range []struct{ input, name, rel string }{{"in_csv", "report.csv", "in_csv/report.csv"}, {"in_evil", "../../etc/passwd", "in_evil/etcpasswd"}} {
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", protocol.Input{ID: row.input, Parts: file(row.name)}, nil)
			d.WaitIdle(t, id)
			got, err := os.ReadFile(filepath.Join(dir, ".harness/attachments", row.rel))
			if err != nil || !bytes.Equal(got, csv) {
				t.Errorf("attachment %s = %q, %v, want the CSV bytes inside the attachments directory", row.rel, got, err)
			}
		}
		second := []byte("c,d\n3,4\n")
		d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", protocol.Input{ID: "in_dup", Parts: append(file("data.csv"), protocol.Part{Type: protocol.PartBlob, MediaType: "text/csv", Name: "data.csv", Data: second})}, nil)
		d.WaitIdle(t, id)
		for rel, want := range map[string][]byte{"in_dup/data.csv": csv, "in_dup/data-2.csv": second} {
			if got, err := os.ReadFile(filepath.Join(dir, ".harness/attachments", rel)); err != nil || !bytes.Equal(got, want) {
				t.Errorf("attachment %s = %q, %v, want its own bytes %q", rel, got, err, want)
			}
		}
		res := d.call(t, http.MethodPost, "/sessions/"+id+"/inputs", protocol.Input{ID: "in_noname", Parts: file("")})
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
