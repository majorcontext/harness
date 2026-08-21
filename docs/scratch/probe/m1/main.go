// Command m1 is a THROWAWAY probe (docs/scratch only, never production)
// replicating harness's REAL request shape end to end, to settle the last
// open question gating lazy MCP tool exposure: how does the moving-
// breakpoint design behave with a GROWING MESSAGE HISTORY carrying the
// second breakpoint (provider/anthropic/transcode.go:263), at real
// AGENTS.md system scale?
//
// Every earlier probe held the message history fixed at one short user
// turn. That is not the shape a real session has. Harness places TWO
// ephemeral breakpoints per request:
//
//	:207  last SYSTEM block          (AGENTS.md lives here)
//	:263  last content block of the LAST MESSAGE (moves forward each turn)
//
// and the wire prefix orders tools -> system -> messages. So a growing
// history sits BEHIND both the tools array and the system block, and a
// tools-array change should invalidate all three.
//
// Two arms, each with its own cold nonce:
//
//	UNBATCHED: 2 separate load events, 1 schema each, on different turns
//	BATCHED:   1 load event carrying 4 schemas
//
// Both run >=5 turns with a realistic user+assistant exchange appended
// every turn. Credentials are read by env var NAME only, never printed.
//
//	go run ./docs/scratch/probe/m1 -tools /tmp/mcpdump -agents AGENTS.md -arm unbatched -nonce m1u-1
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
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type result struct {
	U    usage
	TTFT time.Duration
	Err  error
}

const bp = `{"type":"ephemeral"}`

// msg is one canonical message; the :263-shape breakpoint is placed on the
// last content block of the LAST message only.
type msg struct {
	Role string
	Text string
}

func main() {
	dir := flag.String("tools", "/tmp/mcpdump", "directory of <server>.tools.json dumps")
	agents := flag.String("agents", "AGENTS.md", "path to the real AGENTS.md used as the system block")
	arm := flag.String("arm", "unbatched", "unbatched | batched")
	gap := flag.Duration("gap", 15*time.Second, "delay between turns")
	model := flag.String("model", "anthropic/claude-haiku-4-5-20251001", "model ref")
	nonce := flag.String("nonce", "", "cold-cache run id (defaults to unix nano)")
	turns := flag.Int("turns", 6, "number of turns")
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

	sysBytes, err := os.ReadFile(*agents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read agents: %v\n", err)
		os.Exit(1)
	}
	fat, err := load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load tools: %v\n", err)
		os.Exit(1)
	}
	catalog := thinCatalog(fat)

	fmt.Printf("endpoint: %s/v1/messages\n", base)
	fmt.Println("credential: from env BIFROST_API_KEY (value never printed)")
	fmt.Printf("model: %s | arm: %s | nonce: %s | gap: %s | turns: %d\n", *model, *arm, *nonce, *gap, *turns)
	fmt.Printf("system block: REAL %s, %d bytes (~%d tokens est) with its own BP (:207 shape)\n",
		*agents, len(sysBytes), len(sysBytes)/4)
	fmt.Printf("tools: thin catalog %d entries, moving BP on last loaded schema\n", len(catalog))
	fmt.Println("messages: growing history, :263-shape BP on last content block of last message")
	fmt.Println()

	// Four distinct schemas available to load.
	var schemas []tool
	for i, idx := range []int{len(fat) / 2, len(fat) / 3, len(fat) / 4, len(fat) / 5} {
		s := fat[idx]
		s.Name = fmt.Sprintf("%s_m1load%d", s.Name, i+1)
		s.CacheControl = nil
		schemas = append(schemas, s)
	}
	for i, s := range schemas {
		fmt.Printf("  schema%d: %s (%d B schema)\n", i+1, s.Name, len(s.InputSchema))
	}
	fmt.Println()

	// Load plan: which schemas become resident BEFORE each turn (1-based).
	// unbatched: +1 at turn 3, +1 at turn 5.   batched: +4 at turn 3.
	loadAt := map[int]int{}
	switch *arm {
	case "unbatched":
		loadAt[3] = 1
		loadAt[5] = 2
	case "batched":
		loadAt[3] = 4
	default:
		fmt.Fprintf(os.Stderr, "unknown arm %q\n", *arm)
		os.Exit(1)
	}

	sys := fmt.Sprintf("[m1 run %s arm %s]\n\n%s", *nonce, *arm, string(sysBytes))
	cat := coldify(catalog, *nonce, *arm)

	ctx := context.Background()
	cl := &http.Client{Timeout: 10 * time.Minute}

	var history []msg
	resident := 0

	for t := 1; t <= *turns; t++ {
		if n, ok := loadAt[t]; ok {
			resident = n
			fmt.Printf("### LOAD EVENT before turn %d: resident schemas now %d\n", t, resident)
		}
		// Tools = catalog + resident schemas, BP on the LAST resident
		// schema (or last catalog entry when none are resident).
		tools := clone(cat)
		tools = append(tools, schemas[:resident]...)
		tools = withBPAt(tools, len(tools)-1)

		// Grow the history BEFORE the request, as a real session would:
		// each turn adds a user message (and the prior assistant reply).
		history = append(history, msg{"user", turnText(t)})

		fmt.Printf("--- turn %d: tools=%d (BP idx %d) history=%d msgs (~%d tok est) resident=%d ---\n",
			t, len(tools), len(tools)-1, len(history), historyTokens(history), resident)
		r := call(ctx, cl, base, key, *model, sys, tools, history)
		show(r)
		if r.Err != nil {
			return
		}
		// Append the assistant reply so the next turn's history has grown
		// on both sides, exactly like a real transcript.
		history = append(history, msg{"assistant", assistantText(t)})

		if t < *turns {
			fmt.Printf("sleeping %s...\n\n", *gap)
			time.Sleep(*gap)
		}
	}
}

// turnText is a realistic ~500-1000 token user message.
func turnText(t int) string {
	body := strings.Repeat(
		fmt.Sprintf("Turn %d: please review the session-affinity notes and the prompt-cache routing hint, then summarize the tradeoff between a stable fat tool prefix and a thin catalog for this box. ", t),
		18)
	return body + fmt.Sprintf("(request %d) Reply with the single word: ok", t)
}

func assistantText(t int) string {
	return strings.Repeat(
		fmt.Sprintf("Reply %d: the stable prefix caches while the varying one does not; the thin catalog trades resident schema bytes for prefix stability. ", t),
		12)
}

func historyTokens(h []msg) int {
	n := 0
	for _, m := range h {
		n += len(m.Text)
	}
	return n / 4
}

func show(r result) {
	if r.Err != nil {
		fmt.Printf("ERROR: %v\n\n", r.Err)
		return
	}
	fmt.Printf("input=%d cache_creation=%d cache_read=%d output=%d ttft=%.3fs\n\n",
		r.U.InputTokens, r.U.CacheCreationInputTokens, r.U.CacheReadInputTokens,
		r.U.OutputTokens, r.TTFT.Seconds())
}

func clone(ts []tool) []tool {
	out := make([]tool, len(ts))
	copy(out, ts)
	for i := range out {
		out[i].CacheControl = nil
	}
	return out
}

func withBPAt(ts []tool, idx int) []tool {
	out := clone(ts)
	if idx >= 0 && idx < len(out) {
		out[idx].CacheControl = json.RawMessage(bp)
	}
	return out
}

func coldify(ts []tool, nonce, id string) []tool {
	out := clone(ts)
	if len(out) > 0 {
		out[0].Description = fmt.Sprintf("[m1 run %s arm %s] %s", nonce, id, out[0].Description)
	}
	return out
}

func call(ctx context.Context, cl *http.Client, base, key, model, sys string, tools []tool, history []msg) result {
	// System block with its own ephemeral BP: harness transcode.go:207.
	sysBlock := map[string]any{
		"type":          "text",
		"text":          sys,
		"cache_control": map[string]string{"type": "ephemeral"},
	}

	// Messages, with the :263-shape BP on the last content block of the
	// LAST message only -- it moves forward every turn, as the transcoder
	// does.
	msgs := make([]map[string]any, 0, len(history))
	for i, m := range history {
		block := map[string]any{"type": "text", "text": m.Text}
		if i == len(history)-1 {
			block["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		msgs = append(msgs, map[string]any{
			"role":    m.Role,
			"content": []map[string]any{block},
		})
	}

	body := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     true,
		"system":     []map[string]any{sysBlock},
		"tools":      tools,
		"messages":   msgs,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return result{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return result{Err: err}
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return result{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return result{Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, redact(buf.String()))}
	}

	var res result
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
			res.U = ev.Message.Usage
		}
		if ev.Type == "content_block_delta" && !set && ev.Delta.Text != "" {
			res.TTFT = time.Since(start)
			set = true
		}
	}
	if err := sc.Err(); err != nil {
		return result{Err: err}
	}
	return res
}

func redact(s string) string {
	for _, pfx := range []string{"sk-", "mgt_", "ogt_", "Bearer "} {
		for {
			i := strings.Index(s, pfx)
			if i < 0 {
				break
			}
			j := i + len(pfx)
			for j < len(s) && (s[j] == '-' || s[j] == '_' ||
				(s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') ||
				(s[j] >= '0' && s[j] <= '9')) {
				j++
			}
			s = s[:i] + "<redacted>" + s[j:]
		}
	}
	return s
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

func thinCatalog(fat []tool) []tool {
	out := make([]tool, 0, len(fat)+2)
	for _, t := range fat {
		out = append(out, tool{
			Name:        t.Name,
			Description: oneLine(t.Description),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
	}
	out = append(out,
		tool{
			Name:        "mcp_search_tools",
			Description: "Search the catalog of available MCP tools by keyword.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
		},
		tool{
			Name:        "mcp_load_tool",
			Description: "Load the full input schema for one or more MCP tools by name.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"}}},"required":["names"]}`),
		},
	)
	return out
}

func oneLine(d string) string {
	d = strings.TrimSpace(d)
	if i := strings.IndexAny(d, "\n"); i >= 0 {
		d = d[:i]
	}
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i+1]
	}
	if len(d) > 160 {
		d = d[:157] + "..."
	}
	return strings.TrimSpace(d)
}
