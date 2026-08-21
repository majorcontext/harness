// Command sysbp is a THROWAWAY probe (docs/scratch only) that verifies a
// detail the lazy-MCP design depends on: harness places its ONLY ephemeral
// cache_control breakpoint on the last SYSTEM block, never on a tool
// (provider/anthropic/transcode.go). Anthropic documents the cacheable
// prefix as ordered tools -> system -> messages, so a system breakpoint
// should still cache the tools array ahead of it.
//
// This probe confirms that empirically, and then confirms the corollary
// that matters for lazy loading: with the breakpoint on system, changing
// the TOOLS array still invalidates the cache, because tools sit in the
// cached prefix ahead of the breakpoint.
//
// Credentials are read by env var NAME only and never printed.
//
//	go run ./docs/scratch/probe/sysbp -tools /tmp/mcpdump
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func main() {
	dir := flag.String("tools", "/tmp/mcpdump", "directory of <server>.tools.json dumps")
	gap := flag.Duration("gap", 20*time.Second, "delay between turns")
	model := flag.String("model", "anthropic/claude-haiku-4-5-20251001", "model ref")
	nonce := flag.String("nonce", "", "cold-cache run id (defaults to unix nano)")
	flag.Parse()
	if *nonce == "" {
		*nonce = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://bifrost.meetneptune.dev/anthropic"
	}
	key := os.Getenv("BIFROST_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "credential env var BIFROST_API_KEY is empty")
		os.Exit(1)
	}
	fmt.Printf("endpoint: %s/v1/messages\n", base)
	fmt.Printf("credential: from env BIFROST_API_KEY (value never printed)\n")
	fmt.Printf("breakpoint placement: LAST SYSTEM BLOCK only (harness's real shape)\n\n")

	fat, err := load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	cl := &http.Client{Timeout: 5 * time.Minute}
	sys := fmt.Sprintf("You are a probe fixture for run %s. Answer with the single word: ok.", *nonce)

	// Mark tool[0] so this run starts cold.
	cold := make([]tool, len(fat))
	copy(cold, fat)
	cold[0].Description = fmt.Sprintf("[sysbp run %s] %s", *nonce, cold[0].Description)

	fmt.Println("--- turn 1: fat tools, system breakpoint (cold) ---")
	u1, t1 := call(ctx, cl, base, key, *model, sys, cold)
	fmt.Printf("input=%d cache_creation=%d cache_read=%d ttft=%.3fs\n",
		u1.InputTokens, u1.CacheCreationInputTokens, u1.CacheReadInputTokens, t1.Seconds())

	fmt.Printf("\nsleeping %s...\n\n", *gap)
	time.Sleep(*gap)

	fmt.Println("--- turn 2: IDENTICAL tools + system (expect cache read covering the tools array) ---")
	u2, t2 := call(ctx, cl, base, key, *model, sys, cold)
	fmt.Printf("input=%d cache_creation=%d cache_read=%d ttft=%.3fs\n",
		u2.InputTokens, u2.CacheCreationInputTokens, u2.CacheReadInputTokens, t2.Seconds())

	fmt.Printf("\nsleeping %s...\n\n", *gap)
	time.Sleep(*gap)

	fmt.Println("--- turn 3: SAME system, ONE TOOL REMOVED (expect miss: tools precede the breakpoint) ---")
	varied := append(append([]tool{}, cold[:len(cold)/2]...), cold[len(cold)/2+1:]...)
	u3, t3 := call(ctx, cl, base, key, *model, sys, varied)
	fmt.Printf("input=%d cache_creation=%d cache_read=%d ttft=%.3fs\n",
		u3.InputTokens, u3.CacheCreationInputTokens, u3.CacheReadInputTokens, t3.Seconds())
}

func call(ctx context.Context, cl *http.Client, base, key, model, sys string, tools []tool) (usage, time.Duration) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     true,
		// Exactly harness's shape: the ephemeral breakpoint on the LAST
		// system block, and NO cache_control on any tool.
		"system": []map[string]any{{
			"type":          "text",
			"text":          sys,
			"cache_control": map[string]string{"type": "ephemeral"},
		}},
		"tools": tools,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Reply with the single word: ok",
		}},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "do: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		fmt.Fprintf(os.Stderr, "HTTP %d: %s\n", resp.StatusCode, buf.String())
		os.Exit(1)
	}

	var u usage
	var ttft time.Duration
	var set bool
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage usage `json:"usage"`
			} `json:"message"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "message_start" {
			u = ev.Message.Usage
		}
		if ev.Type == "content_block_delta" && !set && ev.Delta.Text != "" {
			ttft = time.Since(start)
			set = true
		}
	}
	return u, ttft
}

func load(dir string) ([]tool, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.tools.json"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no *.tools.json in %s", dir)
	}
	sort.Strings(entries)
	var all []tool
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			return nil, err
		}
		var ts []tool
		if err := json.Unmarshal(b, &ts); err != nil {
			return nil, err
		}
		all = append(all, ts...)
	}
	return all, nil
}
