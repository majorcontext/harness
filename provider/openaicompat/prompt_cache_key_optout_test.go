package openaicompat

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/majorcontext/harness/message"
)

// TestPromptCacheKeyDisabledOmitsFieldKeepsUser: a deployment whose upstream
// validates its request schema strictly can suppress prompt_cache_key. The
// suppression is scoped to that ONE field — "user" still carries the session
// key, so the measured Fireworks affinity win survives the opt-out.
func TestPromptCacheKeyDisabledOmitsFieldKeepsUser(t *testing.T) {
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out, err := transcodeRequestOpts(req, testFamily, transcodeOptions{noPromptCacheKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.PromptCacheKey != "" {
		t.Errorf("PromptCacheKey = %q, want empty when suppressed", out.PromptCacheKey)
	}
	if out.User != "sess_abc" {
		t.Errorf("User = %q, want %q — suppression must not touch user", out.User, "sess_abc")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"prompt_cache_key"`) {
		t.Errorf("wire must omit prompt_cache_key when suppressed: %s", raw)
	}
	if !strings.Contains(string(raw), `"user":"sess_abc"`) {
		t.Errorf("wire missing user field: %s", raw)
	}
}

// TestClientSendsPromptCacheKeyByDefault: the zero-value Client sends the
// field. Suppression is opt-in, so a deployment that configures nothing keeps
// the cache-affinity hint.
func TestClientSendsPromptCacheKeyByDefault(t *testing.T) {
	c := &Client{Family: testFamily, APIKey: "k"}
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out, err := transcodeRequestOpts(req, c.Family, c.transcodeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if out.PromptCacheKey != "sess_abc" {
		t.Errorf("PromptCacheKey = %q, want %q", out.PromptCacheKey, "sess_abc")
	}
}

// TestClientNoPromptCacheKeyOptOut: Client.NoPromptCacheKey reaches the
// transcoder through the production seam Stream uses.
func TestClientNoPromptCacheKeyOptOut(t *testing.T) {
	c := &Client{Family: testFamily, APIKey: "k", NoPromptCacheKey: true}
	req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
	req.SessionKey = "sess_abc"
	out, err := transcodeRequestOpts(req, c.Family, c.transcodeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if out.PromptCacheKey != "" {
		t.Errorf("PromptCacheKey = %q, want empty", out.PromptCacheKey)
	}
	if out.User != "sess_abc" {
		t.Errorf("User = %q, want the session key", out.User)
	}
}

// TestStreamHonorsNoPromptCacheKey drives the real Stream path and inspects
// the body the server received, so the option cannot be lost between Stream
// and the transcoder.
func TestStreamHonorsNoPromptCacheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		off  bool
		want bool // want prompt_cache_key present
	}{
		{"default sends", false, true},
		{"opt-out omits", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := make(chan []byte, 1)
			c := testClientCapturing(t, bodies)
			c.NoPromptCacheKey = tc.off
			req := baseRequest(message.Message{Role: message.RoleUser, Parts: message.Parts{&message.Text{Text: "hi"}}})
			req.SessionKey = "sess_abc"
			s, err := c.Stream(t.Context(), req)
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			t.Cleanup(func() { s.Close() })
			body := string(<-bodies)
			if got := strings.Contains(body, `"prompt_cache_key":"sess_abc"`); got != tc.want {
				t.Errorf("prompt_cache_key present = %v, want %v: %s", got, tc.want, body)
			}
			if !strings.Contains(body, `"user":"sess_abc"`) {
				t.Errorf("user field must always ride: %s", body)
			}
		})
	}
}

// testClientCapturing serves a minimal stream and publishes the request body
// it received, so a test can assert on the exact wire bytes.
func testClientCapturing(t *testing.T, bodies chan<- []byte) *Client {
	t.Helper()
	return testClient(t, testFamily, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" + sseDone))
	})
}
