package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/majorcontext/harness"
	"github.com/majorcontext/harness/protocol"
)

type waitingInputs map[string][]protocol.QueuedInput

func (q waitingInputs) Queued(_ context.Context, session string) ([]protocol.QueuedInput, error) {
	return q[session], nil
}

func TestContractReadHandler(t *testing.T) {
	skipShort(t)
	onHosts(t, func(t *testing.T, h host) {
		t.Run("the_read_handler_serves_the_read_routes_and_refuses_the_rest", func(t *testing.T) {
			t.Parallel()
			d, _ := startOn(t, h, runtimeWorkdir(t, nil), nil, replyText("seen"))
			id := d.Create(t)
			d.Attach(t, id, "look", rowAttachments()[:1])
			d.WaitIdle(t, id)
			key := blobKeyOf(t, d, id)
			queue := waitingInputs{"waiting": {{ID: "q1", Delivery: protocol.DeliveryQueue, Source: "user", Parts: []protocol.MessagePart{{Type: "text", Text: "later"}}}}}
			srv := httptest.NewServer(harness.ReadHandler(harness.NewDiskStore(d.store), queue))
			t.Cleanup(srv.Close)
			client := wireClientReadOnly(t, srv.Client())
			session := "/sessions/" + id
			rows := []struct {
				method, path string
				accept       string
				want         int
			}{
				{http.MethodGet, "/sessions", "", http.StatusOK},
				{http.MethodGet, session, "", http.StatusOK},
				{http.MethodGet, session + "/inputs", "", http.StatusOK},
				{http.MethodGet, "/sessions/waiting/inputs", "", http.StatusOK},
				{http.MethodGet, session + "/messages", "", http.StatusOK},
				{http.MethodGet, session + "/events", "", http.StatusOK},
				{http.MethodGet, session + "/events", "text/event-stream", http.StatusOK},
				{http.MethodGet, session + "/blobs/" + key, "", http.StatusOK},
				{http.MethodGet, session + "/blobs/missing", "", http.StatusNotFound},
				{http.MethodGet, "/sessions/nope", "", http.StatusNotFound},
				{http.MethodGet, "/sessions/waiting", "", http.StatusNotFound},
				{http.MethodPost, "/sessions", "", http.StatusMethodNotAllowed},
				{http.MethodPatch, session, "", http.StatusMethodNotAllowed},
				{http.MethodDelete, session, "", http.StatusMethodNotAllowed},
				{http.MethodPost, session + "/inputs", "", http.StatusMethodNotAllowed},
				{http.MethodDelete, session + "/inputs/q1", "", http.StatusMethodNotAllowed},
				{http.MethodPost, session + "/interrupt", "", http.StatusMethodNotAllowed},
				{http.MethodPost, session + "/compact", "", http.StatusMethodNotAllowed},
				{http.MethodPost, session + "/requests/r1", "", http.StatusMethodNotAllowed},
				{http.MethodPut, session + "/goal", "", http.StatusMethodNotAllowed},
				{http.MethodDelete, session + "/goal", "", http.StatusMethodNotAllowed},
				{http.MethodPost, "/processes/p/start", "", http.StatusMethodNotAllowed},
				{http.MethodPost, "/processes/p/stop", "", http.StatusMethodNotAllowed},
				{http.MethodPost, "/processes/p/restart", "", http.StatusMethodNotAllowed},
				{http.MethodGet, "/models", "", http.StatusNotFound},
				{http.MethodGet, "/health", "", http.StatusNotFound},
				{http.MethodGet, "/nowhere", "", http.StatusNotFound},
			}
			for _, row := range rows {
				req, err := http.NewRequestWithContext(t.Context(), row.method, srv.URL+row.path, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				if row.accept != "" {
					req.Header.Set("Accept", row.accept)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("%s %s: %v", row.method, row.path, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != row.want {
					t.Errorf("%s %s = %d, want %d", row.method, row.path, resp.StatusCode, row.want)
				}
			}
		})
	})
}
