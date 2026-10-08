package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type obj = map[string]any

type reply struct {
	Status int
	Body   []byte
}

func post(url string, header map[string]string, body obj) (reply, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return reply{}, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return reply{Status: resp.StatusCode, Body: data}, err
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}

// errorFixture is the recorded form of a failed HTTP response.
func errorFixture(r reply) ([]byte, error) {
	var body json.RawMessage = r.Body
	if !json.Valid(body) {
		return nil, fmt.Errorf("error body is not JSON (status %d)", r.Status)
	}
	return json.MarshalIndent(obj{"status": r.Status, "body": body}, "", "  ")
}

type sseEvent struct{ Name, Data string }

func parseSSE(body []byte) []sseEvent {
	var out []sseEvent
	var cur sseEvent
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			if cur != (sseEvent{}) {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event:"):
			cur.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.Data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if cur != (sseEvent{}) {
		out = append(out, cur)
	}
	return out
}

func okStream(r reply) error {
	if r.Status != http.StatusOK {
		return fmt.Errorf("status %d: %s", r.Status, truncate(string(r.Body), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
