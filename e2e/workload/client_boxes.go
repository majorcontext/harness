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

// BoxesClient talks to the deployed fleet control plane ("the boxes
// service") over its real HTTP API. The wire shapes below are PROVISIONAL —
// see doc.go's package comment — derived from the phase-2.5 acceptance
// table's own naming (POST /v1/boxes, GET, an event stream, hibernate)
// rather than a committed OpenAPI spec, because that spec does not exist in
// this repo yet. Adjust this file, and only this file, once it does.
type BoxesClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewBoxesClient builds a client against env-provided BOXES_URL/BOXES_TOKEN.
// ok is false when either var is unset; callers should t.Skip with the
// missing-var detail rather than treating that as a failure.
func NewBoxesClient() (client *BoxesClient, ok bool, missing string) {
	env := loadBoxesEnv()
	if !env.ready() {
		return nil, false, env.missing()
	}
	return &BoxesClient{
		baseURL: env.URL,
		token:   env.Token,
		hc:      &http.Client{Timeout: 30 * time.Second},
	}, true, ""
}

// Box mirrors the boxes service's box resource as described by the
// acceptance table: an identity, a lifecycle status, and — once running —
// the address and credential a caller uses to reach that box's own
// `harness serve` API (see client_harness.go).
type Box struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	Status    string    `json:"status"` // e.g. "pending", "running", "hibernated", "terminated"
	TunnelURL string    `json:"tunnel_url,omitempty"`
	RunToken  string    `json:"run_token,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// SpawnRequest is the body of POST /v1/boxes.
type SpawnRequest struct {
	Name  string            `json:"name"`
	Image string            `json:"image"`
	Env   map[string]string `json:"env,omitempty"`
}

// Ping performs a minimal reachability probe against the boxes service
// (GET /v1/boxes, expecting any non-5xx/non-connection-error response) so a
// test can distinguish "service unreachable" from "service reachable but
// this box/image is missing" and skip with the right message.
func (c *BoxesClient) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/boxes", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("boxes service unreachable at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("boxes service at %s returned %d: %s", c.baseURL, resp.StatusCode, body)
	}
	return nil
}

// Spawn creates a new box and returns its resource. It does not block for
// the box to become ready — callers poll Get or watch Events.
func (c *BoxesClient) Spawn(ctx context.Context, req SpawnRequest) (*Box, error) {
	var box Box
	if err := c.do(ctx, http.MethodPost, "/v1/boxes", req, &box); err != nil {
		return nil, fmt.Errorf("spawn box %q: %w", req.Name, err)
	}
	return &box, nil
}

// Get polls a box's current state.
func (c *BoxesClient) Get(ctx context.Context, id string) (*Box, error) {
	var box Box
	if err := c.do(ctx, http.MethodGet, "/v1/boxes/"+id, nil, &box); err != nil {
		return nil, fmt.Errorf("get box %q: %w", id, err)
	}
	return &box, nil
}

// Hibernate suspends a box without deleting it.
func (c *BoxesClient) Hibernate(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodPost, "/v1/boxes/"+id+"/hibernate", nil, nil); err != nil {
		return fmt.Errorf("hibernate box %q: %w", id, err)
	}
	return nil
}

// Delete terminates a box and releases its compute. It does NOT delete the
// box's underlying volume (see docs/design/fleet-model.md's cattle/pets
// split) — that is a separate, explicit retire operation this client does
// not perform, so a test's cleanup only ever costs compute, never history.
func (c *BoxesClient) Delete(ctx context.Context, id string) error {
	if err := c.do(ctx, http.MethodDelete, "/v1/boxes/"+id, nil, nil); err != nil {
		return fmt.Errorf("delete box %q: %w", id, err)
	}
	return nil
}

// BoxEvent is one lifecycle event off a box's SSE event stream (spawn
// progress, status transitions, and the like). Field names are provisional,
// same caveat as Box.
type BoxEvent struct {
	Type   string          `json:"type"`
	BoxID  string          `json:"box_id"`
	Status string          `json:"status,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Events streams a box's lifecycle events until ctx is cancelled or the
// stream ends, delivering each parsed BoxEvent on the returned channel. The
// channel is closed when the stream ends (for any reason); the caller
// should select on ctx.Done() alongside it.
func (c *BoxesClient) Events(ctx context.Context, id string) (<-chan BoxEvent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/boxes/"+id+"/events", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open event stream for box %q: %w", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("event stream for box %q: status %d: %s", id, resp.StatusCode, body)
	}

	out := make(chan BoxEvent)
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
			var ev BoxEvent
			if json.Unmarshal(raw.Data, &ev) != nil {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *BoxesClient) do(ctx context.Context, method, path string, body, out any) error {
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
	req.Header.Set("Authorization", "Bearer "+c.token)
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
