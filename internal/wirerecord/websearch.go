package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/majorcontext/harness/internal/codexauth"
)

const (
	searchPrompt  = "Search the web for the current Go release and cite the source in one sentence."
	codexResponse = "https://chatgpt.com/backend-api/codex/responses"
	codexSearch   = "gpt-5.6-sol"
)

// recordAnthropicSearch records one Messages stream that runs the server tool web_search.
func recordAnthropicSearch(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	tool := obj{"type": "web_search_20260209", "name": "web_search", "max_uses": 1, "allowed_callers": []string{"direct"}}
	rep, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 1024, "tools": []obj{tool},
		"messages": []obj{{"role": "user", "content": searchPrompt}}})
	if err != nil || okStream(rep) != nil {
		return fmt.Errorf("search: %v %v", err, okStream(rep))
	}
	body := string(rep.Body)
	for _, want := range []string{`"server_tool_use"`, `"web_search_tool_result"`, `"citations_delta"`, `"web_search_requests"`} {
		if !strings.Contains(body, want) {
			return fmt.Errorf("the search stream lacks %s", want)
		}
	}
	return r.save("anthropic.websearch.sse", rep.Body)
}

// recordAnthropicSearchError records a Messages stream whose second search fails, because max_uses is 1.
func recordAnthropicSearchError(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	tool := obj{"type": "web_search_20260209", "name": "web_search", "max_uses": 1, "allowed_callers": []string{"direct"}}
	prompt := "Run two separate web searches, one after the other: first for the current Go release, then for the current Rust release. Then answer in one sentence."
	rep, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 1024, "tools": []obj{tool},
		"messages": []obj{{"role": "user", "content": prompt}}})
	if err != nil || okStream(rep) != nil {
		return fmt.Errorf("search error: %v %v", err, okStream(rep))
	}
	if !strings.Contains(string(rep.Body), `"web_search_tool_result_error"`) {
		return fmt.Errorf("the stream holds no failed search")
	}
	return r.save("anthropic.websearch-error.sse", rep.Body)
}

// recordAnthropicSearchPause records a Messages stream that ends with pause_turn, because the model chains more searches than one call runs.
func recordAnthropicSearchPause(r *recorder, _ string) error {
	key, err := requireEnv("ANTHROPIC_API_KEY")
	if err != nil {
		return err
	}
	tool := obj{"type": "web_search_20260209", "name": "web_search", "max_uses": 30, "allowed_callers": []string{"direct"}}
	prompt := "Do a chain of exactly 14 web searches, strictly one at a time. Each search must use a word from the top result title of the previous search as its query (start with: Go release). Never run two searches in the same step. After 14, say the last title."
	rep, err := anthropicCall(key, obj{"model": anthropicModel, "max_tokens": 2048, "tools": []obj{tool},
		"messages": []obj{{"role": "user", "content": prompt}}})
	if err != nil || okStream(rep) != nil {
		return fmt.Errorf("search pause: %v %v", err, okStream(rep))
	}
	if !strings.Contains(string(rep.Body), `"stop_reason":"pause_turn"`) {
		return fmt.Errorf("the stream did not end with pause_turn")
	}
	return r.save("anthropic.websearch-pause.sse", rep.Body)
}

// recordCodexSearch records one Responses stream of the ChatGPT Codex lane that runs the web_search tool, with the login of `harness login codex`.
func recordCodexSearch(r *recorder, _ string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := codexauth.NewSource(codexauth.SourceOptions{Path: filepath.Join(home, ".harness", "auth", "codex.json")})
	hc := &http.Client{Transport: codexauth.NewTransport(src, nil, codexResponse), Timeout: 3 * time.Minute}
	raw, err := json.Marshal(obj{"model": codexSearch, "instructions": "You are a helpful assistant.", "stream": true, "store": false,
		"include": []string{"reasoning.encrypted_content"}, "tools": []obj{{"type": "web_search"}}, "tool_choice": "auto", "parallel_tool_calls": true,
		"input": []obj{userMessage(searchPrompt)}})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponse, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return err
	}
	rep := reply{Status: resp.StatusCode, Body: buf.Bytes()}
	if err := okStream(rep); err != nil {
		return err
	}
	for _, want := range []string{`"web_search_call"`, `"response.web_search_call.searching"`, `"url_citation"`, `"response.completed"`} {
		if !strings.Contains(string(rep.Body), want) {
			return fmt.Errorf("the search stream lacks %s", want)
		}
	}
	return r.save("responses.websearch.sse", rep.Body)
}

// recordClaudeCodeSearch records a CLI turn that runs the built-in WebSearch tool, as the claudecode backend runs it.
func recordClaudeCodeSearch(r *recorder, work string) error {
	dir := filepath.Join(work, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := runCLI(dir, []string{searchPrompt}, cliArgs(claudeModel, "WebSearch", "--include-partial-messages", "--allowedTools", "WebSearch"), false)
	if err != nil {
		return fmt.Errorf("search run: %w", err)
	}
	if !bytes.Contains(raw, []byte(`"name":"WebSearch"`)) {
		return fmt.Errorf("the CLI did not call WebSearch")
	}
	return r.saveJSONL("claudecode.websearch.jsonl", raw)
}
