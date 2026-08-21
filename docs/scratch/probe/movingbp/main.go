// Command movingbp is a THROWAWAY probe (docs/scratch only, never
// production) answering the ONE question the reviewer's REJECT hinges on:
// can a MOVING cache breakpoint rescue lazy MCP schema loading?
//
// The earlier cacheprobe pinned the breakpoint to the end of the stable
// catalog and measured only two turns. That showed an appended schema
// billing as fresh input once, and the report then GENERALIZED to "rides
// the cache from the next turn onward" without running a third turn. The
// reviewer ran it: fresh forever, not fresh once. This probe tests the
// obvious repair -- move the breakpoint to the newly loaded schema so the
// schema is INSIDE the cached prefix on every later turn.
//
// SCENARIO E (C1, decisive): tools-only, breakpoint on the last loaded
// schema.
//
//	t1 catalog, BP on last catalog entry (cold)
//	t2 catalog + schema1, BP MOVED to schema1
//	t3 byte-identical to t2 -- DOES read cover catalog+schema1?
//	t4 catalog + schema1 + schema2, BP moved to schema2
//
// SCENARIO F (C2, real-harness cost): same, but with a system block
// carrying its own breakpoint, standing in for harness's transcode.go:207
// system BP (AGENTS.md scale). Tools precede system in Anthropic's prefix
// order, so growing the tools array must re-create the system prefix too.
// This quantifies what ONE mcp_load_tool call really costs.
//
// Credentials are read by env var NAME only and never printed.
//
//	go run ./docs/scratch/probe/movingbp -tools /tmp/mcpdump -gap 20s -nonce e1
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

func main() {
	dir := flag.String("tools", "/tmp/mcpdump", "directory of <server>.tools.json dumps")
	gap := flag.Duration("gap", 20*time.Second, "delay between turns")
	model := flag.String("model", "anthropic/claude-haiku-4-5-20251001", "model ref")
	nonce := flag.String("nonce", "", "cold-cache run id (defaults to unix nano)")
	sysFill := flag.Int("sysfill", 3000, "approx tokens of system filler standing in for AGENTS.md (scenario F)")
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
	fmt.Println("credential: from env BIFROST_API_KEY (value never printed)")
	fmt.Printf("model: %s\nnonce: %s\ngap: %s\n\n", *model, *nonce, *gap)

	fat, err := load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	catalog := thinCatalog(fat)

	// Two full-schema tools to "load", renamed so they cannot collide with
	// their own catalog entries.
	s1 := fat[len(fat)/2]
	s1.Name += "_loaded1"
	s2 := fat[len(fat)/3]
	s2.Name += "_loaded2"

	ctx := context.Background()
	cl := &http.Client{Timeout: 5 * time.Minute}

	fmt.Printf("catalog entries: %d | schema1: %s (%d B) | schema2: %s (%d B)\n\n",
		len(catalog), s1.Name, len(s1.InputSchema), s2.Name, len(s2.InputSchema))

	// ---------------- SCENARIO E ----------------
	fmt.Println("==================== SCENARIO E: moving BP, tools only (C1) ====================")
	sysE := fmt.Sprintf("Probe fixture E run %s. Answer with the single word: ok.", *nonce)
	cat := coldify(catalog, *nonce, "E")

	e1 := withBPAt(cat, len(cat)-1)
	fmt.Println("--- t1: catalog only, BP on last catalog entry (expect COLD: read=0) ---")
	show(call(ctx, cl, base, key, *model, sysE, e1, false, 0))

	sleep(*gap)
	e2 := withBPAt(append(clone(cat), s1), len(cat)) // BP moved onto schema1
	fmt.Println("--- t2: + schema1 appended, BP MOVED to schema1 (expect read=catalog, creation=schema1) ---")
	show(call(ctx, cl, base, key, *model, sysE, e2, false, 0))

	sleep(*gap)
	fmt.Println("--- t3: BYTE-IDENTICAL to t2 -- DECISIVE: does read now cover catalog+schema1? ---")
	show(call(ctx, cl, base, key, *model, sysE, e2, false, 0))

	sleep(*gap)
	e4 := withBPAt(append(append(clone(cat), s1), s2), len(cat)+1) // BP on schema2
	fmt.Println("--- t4: + schema2, BP moved to schema2 (expect read=catalog+schema1, creation=schema2) ---")
	show(call(ctx, cl, base, key, *model, sysE, e4, false, 0))

	// ---------------- SCENARIO F ----------------
	fmt.Println()
	fmt.Println("==================== SCENARIO F: + system block with its own BP (C2) ====================")
	filler := strings.Repeat("This line stands in for AGENTS.md project instructions. ", *sysFill/12)
	sysF := fmt.Sprintf("Probe fixture F run %s. %s Answer with the single word: ok.", *nonce, filler)
	fmt.Printf("system block: %d bytes (~%d tokens) with its own ephemeral BP (harness transcode.go:207 shape)\n",
		len(sysF), len(sysF)/4)
	catF := coldify(catalog, *nonce, "F")

	f1 := withBPAt(catF, len(catF)-1)
	fmt.Println("--- t1: catalog + system BP (cold) ---")
	show(call(ctx, cl, base, key, *model, sysF, f1, true, 0))

	sleep(*gap)
	fmt.Println("--- t2: byte-identical (expect steady state: everything reads) ---")
	show(call(ctx, cl, base, key, *model, sysF, f1, true, 0))

	sleep(*gap)
	f3 := withBPAt(append(clone(catF), s1), len(catF))
	fmt.Println("--- t3: ONE mcp_load_tool -- schema1 appended, BP moved. THE COST OF A LOAD ---")
	show(call(ctx, cl, base, key, *model, sysF, f3, true, 0))

	sleep(*gap)
	fmt.Println("--- t4: byte-identical to t3 -- does everything read back after the load? ---")
	show(call(ctx, cl, base, key, *model, sysF, f3, true, 0))
}

func sleep(d time.Duration) {
	fmt.Printf("sleeping %s...\n", d)
	time.Sleep(d)
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
	// Strip any breakpoint so exactly one BP is placed by withBPAt.
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
		out[0].Description = fmt.Sprintf("[movingbp run %s scenario %s] %s", nonce, id, out[0].Description)
	}
	return out
}

func call(ctx context.Context, cl *http.Client, base, key, model, sys string, tools []tool, sysBP bool, _ int) result {
	sysBlock := map[string]any{"type": "text", "text": sys}
	if sysBP {
		sysBlock["cache_control"] = map[string]string{"type": "ephemeral"}
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     true,
		"system":     []map[string]any{sysBlock},
		"tools":      tools,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Reply with the single word: ok",
		}},
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
