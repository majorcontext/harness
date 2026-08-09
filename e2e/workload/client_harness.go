//go:build e2e

package workload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HarnessClient talks to the `harness serve` API a single box exposes on
// port 4096 (fronted by the box's own tunnel URL) — the surface
// server/openapi.yaml already specifies. Unlike client_boxes.go, this file
// mirrors an existing, committed spec, so its shapes carry only the fields
// this suite actually reads (a deliberate subset, not a full client).
type HarnessClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewHarnessClient builds a client against one box's tunnel URL and run
// token, as returned by BoxesClient.Spawn/Get.
func NewHarnessClient(baseURL, runToken string) *HarnessClient {
	return &HarnessClient{
		baseURL: baseURL,
		token:   runToken,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}
}

// Health mirrors the openapi.yaml Health schema (server/openapi.yaml:53).
type Health struct {
	Version     string `json:"version"`
	VCSRevision string `json:"vcs_revision"`
	VCSTime     string `json:"vcs_time"`
	SessionSync string `json:"session_sync"`
	StartedAt   string `json:"started_at"`
}

// Health calls GET /health. It is unauthenticated on the wire (see
// openapi.yaml's health operation `security: []`), so an empty token still
// works — useful as the very first "does this box answer on 4096 at all"
// check row 1 needs, before a run token is even in play.
func (c *HarnessClient) Health(ctx context.Context) (*Health, error) {
	var h Health
	if err := c.do(ctx, http.MethodGet, "/health", nil, &h, false); err != nil {
		return nil, fmt.Errorf("GET /health: %w", err)
	}
	return &h, nil
}

// Session mirrors the fields of the openapi.yaml Session schema
// (server/openapi.yaml:100) this suite reads. Additional fields
// (goal, last_turn, usage, ...) round-trip fine through json.Unmarshal
// without a struct field; add them here as later rows need them.
type Session struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Status    string `json:"status"`
	State     string `json:"state"`
	Messages  int    `json:"messages"`
	Seq       int64  `json:"seq"`
	Workdir   string `json:"workdir"`
	Queued    int    `json:"queued"`
}

// CreateSessionRequest is the POST /session body this suite uses.
type CreateSessionRequest struct {
	Workdir       string `json:"workdir,omitempty"`
	ParentSession string `json:"parent_session,omitempty"`
}

// CreateSession calls POST /session.
func (c *HarnessClient) CreateSession(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	var sess Session
	if err := c.do(ctx, http.MethodPost, "/session", req, &sess, true); err != nil {
		return nil, fmt.Errorf("POST /session: %w", err)
	}
	return &sess, nil
}

// GetSession calls GET /session/{id}.
func (c *HarnessClient) GetSession(ctx context.Context, id string) (*Session, error) {
	var sess Session
	if err := c.do(ctx, http.MethodGet, "/session/"+id, nil, &sess, true); err != nil {
		return nil, fmt.Errorf("GET /session/%s: %w", id, err)
	}
	return &sess, nil
}

// EndSession calls DELETE /session/{id}. It treats 404 as success (the
// session is already gone), matching the endpoint's own documented
// idempotence.
func (c *HarnessClient) EndSession(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/session/"+id, nil, nil, true)
	if err != nil && bytes.Contains([]byte(err.Error()), []byte("status 404")) {
		return nil
	}
	return err
}

// PromptAsyncResponse mirrors server/openapi.yaml:641.
type PromptAsyncResponse struct {
	Seq    int64  `json:"seq"`
	Status string `json:"status"` // "started" or "queued"
	Queued int    `json:"queued,omitempty"`
}

// PromptAsync calls POST /session/{id}/prompt_async with a single text part.
func (c *HarnessClient) PromptAsync(ctx context.Context, id, text string) (*PromptAsyncResponse, error) {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": text}},
	}
	var resp PromptAsyncResponse
	if err := c.do(ctx, http.MethodPost, "/session/"+id+"/prompt_async", body, &resp, true); err != nil {
		return nil, fmt.Errorf("POST /session/%s/prompt_async: %w", id, err)
	}
	return &resp, nil
}

// GoalStartRequest is the POST /session/{id}/goal body.
type GoalStartRequest struct {
	Condition string `json:"condition"`
	MaxTurns  int    `json:"max_turns,omitempty"`
}

// StartGoal calls POST /session/{id}/goal, arming (or updating) a goal loop.
func (c *HarnessClient) StartGoal(ctx context.Context, id string, req GoalStartRequest) error {
	if err := c.do(ctx, http.MethodPost, "/session/"+id+"/goal", req, nil, true); err != nil {
		return fmt.Errorf("POST /session/%s/goal: %w", id, err)
	}
	return nil
}

// MessagePart mirrors the fields of the openapi.yaml TextPart schema this
// suite reads; a non-text part (tool_call, tool_result, blob, reasoning)
// simply leaves Text empty, which is all a caller needs to skip it.
type MessagePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TranscriptMessage mirrors the fields of the openapi.yaml Message schema
// this suite reads.
type TranscriptMessage struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
}

// GetMessages calls GET /session/{id}/message and returns the full
// transcript. A MessagePlaceholder entry (see openapi.yaml) round-trips as
// a TranscriptMessage with empty Parts rather than failing the whole
// unmarshal — this suite only reads text out of ordinary messages.
func (c *HarnessClient) GetMessages(ctx context.Context, id string) ([]TranscriptMessage, error) {
	var msgs []TranscriptMessage
	if err := c.do(ctx, http.MethodGet, "/session/"+id+"/message", nil, &msgs, true); err != nil {
		return nil, fmt.Errorf("GET /session/%s/message: %w", id, err)
	}
	return msgs, nil
}

// LastAssistantText concatenates the text parts of the LAST assistant
// message in a transcript — the model's final reply for the most recent
// turn, which every directive in this suite asks for explicitly (e.g. "reply
// with only the exit code").
func LastAssistantText(msgs []TranscriptMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		var text string
		for _, p := range msgs[i].Parts {
			if p.Type == "text" {
				text += p.Text
			}
		}
		return text
	}
	return ""
}

// Event mirrors the fields of the openapi.yaml Event schema
// (server/openapi.yaml:802) this suite reads.
type Event struct {
	Type      string          `json:"type"`
	SessionID string          `json:"session_id"`
	Seq       int64           `json:"seq,omitempty"`
	Data      json.RawMessage `json:"-"`
}

// Events streams GET /event?from=&session=, delivering each parsed Event
// on the returned channel until ctx is cancelled or the stream ends.
func (c *HarnessClient) Events(ctx context.Context, sessionID string, from int64) (<-chan Event, error) {
	path := fmt.Sprintf("/event?from=%d", from)
	if sessionID != "" {
		path += "&session=" + sessionID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open /event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("/event: status %d: %s", resp.StatusCode, body)
	}

	out := make(chan Event)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := newSSEScanner(resp.Body)
		for {
			raw, err := scanner.next()
			if err != nil {
				return
			}
			if len(raw.Data) == 0 {
				continue
			}
			var ev Event
			if json.Unmarshal(raw.Data, &ev) != nil {
				continue
			}
			ev.Data = raw.Data
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *HarnessClient) do(ctx context.Context, method, path string, body, out any, auth bool) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if auth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, out)
}
