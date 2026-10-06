package e2e

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/protocol"
)

func blobKeyOf(t *testing.T, d *runtimeDriver, id string) string {
	t.Helper()
	var page protocol.MessagePage
	d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id+"/messages", nil, &page)
	for _, m := range page.Messages {
		for _, p := range m.Parts {
			if p.Type == protocol.MessagePartBlob {
				return p.Key
			}
		}
	}
	t.Fatalf("no blob part of the messages carries a key: %+v", page.Messages)
	return ""
}

func getBlob(t *testing.T, d *runtimeDriver, id, key string) (*http.Response, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	resp := d.send(t, ctx, http.MethodGet, "/sessions/"+id+"/blobs/"+key, nil, nil)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func TestContractQueuedInputs(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("queued_inputs_list_their_parts_and_provenance", func(t *testing.T) {
			t.Parallel()
			d, fake := startOn(t, h, runtimeWorkdir(t, nil), nil,
				harnesstest.Step{Name: "call", Match: harnesstest.LastUserText("start"), Reply: harnesstest.Reply{Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_1", Name: "bash", Input: map[string]any{"command": "echo tool"}},
				}}},
				replyText("done"))
			id := d.Create(t)
			d.Submit(t, id, "start")
			if !fake.AwaitRequests(1, waitBound) {
				t.Fatal("the turn sent no model request")
			}
			png := rowAttachments()[0].data
			first := provInput("q1", "later", "slack", "slack:C1:1", "Ann")
			first["delivery"] = protocol.DeliveryQueue
			first["parts"] = append(first["parts"].([]protocol.Part), protocol.Part{Type: protocol.PartBlob, MediaType: "image/png", Data: png})
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", first, nil)
			second := provInput("q2", "last", "", "", "")
			second["delivery"] = protocol.DeliveryQueue
			d.expect(t, http.StatusCreated, http.MethodPost, "/sessions/"+id+"/inputs", second, nil)
			status, raw := d.do(t, http.MethodGet, "/sessions/"+id+"/inputs", nil)
			var got []protocol.QueuedInput
			d.expect(t, http.StatusOK, http.MethodGet, "/sessions/"+id+"/inputs", nil, &got)
			sum := sha256.Sum256(png)
			want := []protocol.QueuedInput{
				{ID: "q1", Delivery: "queue", Source: "slack", SourceID: "slack:C1:1", SourceLabel: "Ann", Parts: []protocol.MessagePart{
					{Type: "text", Text: "later"},
					{Type: "blob", MediaType: "image/png", Bytes: len(png), Key: "attachment-" + hex.EncodeToString(sum[:])}}},
				{ID: "q2", Delivery: "queue", Source: "user", Parts: []protocol.MessagePart{{Type: "text", Text: "last"}}},
			}
			if status != http.StatusOK || len(got) != 2 || !queuedEqual(got, want) {
				t.Errorf("GET inputs = %d %s, want %+v", status, raw, want)
			}
			if bytes.Contains(raw, []byte(`"data"`)) {
				t.Errorf("GET inputs carries attachment data: %s", raw)
			}
			fake.Release("call")
			d.WaitIdle(t, id)
		})
	})
}

func TestContractBlobs(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("a_history_attachment_reads_back_by_its_key", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("seen"))
			id := d.Create(t)
			d.Attach(t, id, "look", rowAttachments()[:1])
			d.WaitIdle(t, id)
			key := blobKeyOf(t, d, id)
			resp, data := getBlob(t, d, id, key)
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(data, rowAttachments()[0].data) {
				t.Errorf("GET blob = %d %s %d bytes, want 200 image/png with the attachment", resp.StatusCode, resp.Header.Get("Content-Type"), len(data))
			}
			resp, data = getBlob(t, d, id, "attachment-0")
			if resp.StatusCode != http.StatusNotFound || !bytes.Contains(data, []byte(protocol.CodeBlobNotFound)) {
				t.Errorf("GET blob of an unknown key = %d %s, want 404 blob_not_found", resp.StatusCode, data)
			}
		})
		t.Run("a_backend_state_blob_is_not_served", func(t *testing.T) {
			fake := harnesstest.New(t)
			run1 := mirrorFixtures(t, "run1.stdout.jsonl")
			lane := claudeLane{mode: "mirror", mirror: true, env: map[string]string{"FAKE_CLAUDE_MIRROR_FIXTURE": run1}}
			d := lane.newDriver(t, h, fake.URL()).(*claudeDriver).laneHost.(*runtimeDriver)
			id := d.Create(t)
			d.Submit(t, id, "reply with ok")
			d.WaitIdle(t, id)
			var state string
			for _, ev := range d.events(t, id) {
				if ev.Kind == "backend.state" {
					state = cmp.Or(decodeEvent[struct {
						Chunk string `json:"chunk"`
					}](t, ev).Chunk, state)
				}
			}
			if state == "" {
				t.Fatal("the session wrote no backend.state")
			}
			resp, data := getBlob(t, d, id, state)
			if resp.StatusCode != http.StatusNotFound || !bytes.Contains(data, []byte(protocol.CodeBlobNotFound)) {
				t.Errorf("GET blob of the backend state = %d %s, want 404 blob_not_found", resp.StatusCode, data)
			}
		})
	})
}

func queuedEqual(a, b []protocol.QueuedInput) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.ID != y.ID || x.Delivery != y.Delivery || x.Source != y.Source || x.SourceID != y.SourceID || x.SourceLabel != y.SourceLabel || len(x.Parts) != len(y.Parts) {
			return false
		}
		for j := range x.Parts {
			if x.Parts[j].Type != y.Parts[j].Type || x.Parts[j].Text != y.Parts[j].Text || x.Parts[j].MediaType != y.Parts[j].MediaType ||
				x.Parts[j].Bytes != y.Parts[j].Bytes || x.Parts[j].Key != y.Parts[j].Key {
				return false
			}
		}
	}
	return true
}
