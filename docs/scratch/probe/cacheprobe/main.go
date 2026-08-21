// Command cacheprobe is a THROWAWAY probe (docs/scratch only, never
// production) that measures Anthropic prompt-cache behaviour when the
// TOOLS array at the front of the request prefix is stable versus varied.
//
// It answers: does lazy MCP tool exposure (which varies the tool prefix
// between requests) forfeit the prompt cache that a stable fat prefix
// would otherwise get?
//
// Each scenario runs two turns with an identical message history, spaced
// apart in time, and reports for each turn:
//
//	input_tokens, cache_creation_input_tokens, cache_read_input_tokens,
//	output_tokens, and time-to-first-token (TTFT) from the streaming
//	response.
//
// Credentials are read from the environment by NAME only and are never
// printed.
//
// Usage:
//
//	go run ./docs/scratch/probe/cacheprobe -tools /tmp/mcpdump -gap 30s
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

type turnResult struct {
	Usage usage
	TTFT  time.Duration
	Total time.Duration
	Err   error
}

const cacheBreakpoint = `{"type":"ephemeral"}`

func main() {
	dir := flag.String("tools", "/tmp/mcpdump", "directory of <server>.tools.json dumps from the toolsize probe")
	gap := flag.Duration("gap", 30*time.Second, "delay between turn 1 and turn 2")
	model := flag.String("model", "anthropic/claude-haiku-4-5-20251001", "model ref to send")
	nonce := flag.String("nonce", "", "unique run id injected into the FIRST tool's description so every scenario starts with a COLD tools-prefix cache; defaults to the current unix nano")
	flag.Parse()

	if *nonce == "" {
		*nonce = fmt.Sprintf("%d", time.Now().UnixNano())
	}

	base := os.Getenv("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://bifrost.meetneptune.dev/anthropic"
	}
	keyEnv := "BIFROST_API_KEY"
	key := os.Getenv(keyEnv)
	if key == "" {
		fmt.Fprintf(os.Stderr, "credential env var %s is empty\n", keyEnv)
		os.Exit(1)
	}
	fmt.Printf("endpoint: %s/v1/messages\n", base)
	fmt.Printf("credential: from env %s (value never printed)\n", keyEnv)
	fmt.Printf("model: %s\n", *model)
	fmt.Printf("turn gap: %s\n\n", *gap)

	fat, err := loadFat(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load tools: %v\n", err)
		os.Exit(1)
	}
	thin := thinCatalog(fat)

	fmt.Printf("fat tool set:  %d tools\n", len(fat))
	fmt.Printf("thin tool set: %d tools (catalog entries + search/select pair)\n\n", len(thin))

	ctx := context.Background()
	cl := &http.Client{Timeout: 5 * time.Minute}

	// Every scenario gets a COLD tools-prefix cache: the run nonce plus
	// the scenario label is injected into the FIRST tool's description,
	// so no scenario can inherit a cache entry warmed by another
	// scenario, by an earlier run of this probe, or by any other client.
	// Tools serialize at the very front of the prefix, so a change to
	// tool[0] invalidates the entire tools cache by construction.
	//
	// bpIdx is the index the ephemeral cache_control breakpoint sits on.
	// Passing -1 means "last tool".
	run := func(label, scenarioID string, t1, t2 []tool, bp1, bp2 int) {
		fmt.Printf("==================== SCENARIO %s ====================\n", label)
		sys := "You are a probe fixture. Answer with the single word: ok."
		c1 := withBreakpointAt(coldify(t1, *nonce, scenarioID), bp1)
		c2 := withBreakpointAt(coldify(t2, *nonce, scenarioID), bp2)
		fmt.Printf("turn 1 tools: %d (breakpoint idx %d) | turn 2 tools: %d (breakpoint idx %d)\n",
			len(c1), bpIndex(c1, bp1), len(c2), bpIndex(c2, bp2))
		r1 := call(ctx, cl, base, key, *model, sys, c1)
		report("turn 1", r1)
		if r1.Err != nil {
			fmt.Println()
			return
		}
		fmt.Printf("sleeping %s...\n", *gap)
		time.Sleep(*gap)
		r2 := call(ctx, cl, base, key, *model, sys, c2)
		report("turn 2", r2)
		fmt.Println()
	}

	run("A stable-fat", "A", fat, fat, -1, -1)

	// VARIED: drop one tool from the MIDDLE of the array, the realistic
	// shape of a lazy loader swapping which schema is resident.
	run("B varied (middle tool removed on turn 2)", "B", fat, dropIndex(fat, len(fat)/2), -1, -1)

	run("C stable-thin", "C", thin, thin, -1, -1)

	// D: append-only growth, the cache-friendly lazy-loader shape. The
	// thin catalog is byte-stable and the breakpoint is pinned to the END
	// OF THE CATALOG (index len(thin)-1) on BOTH turns; turn 2 appends one
	// freshly "loaded" full-schema tool AFTER that breakpoint. This tests
	// whether the stable catalog prefix still cache-reads when the suffix
	// behind it changes -- the single most important number for the
	// design.
	loaded := fat[len(fat)/2]
	loaded.Name = loaded.Name + "_loaded" // avoid a duplicate-name 400
	appended := append(append([]tool{}, thin...), loaded)
	run("D append-only (breakpoint pinned to end of stable catalog)", "D",
		thin, appended, len(thin)-1, len(thin)-1)
}

// coldify returns a copy of ts whose FIRST tool description carries the
// run nonce and scenario id, guaranteeing a cold tools-prefix cache for
// that scenario. Both turns of a scenario get the same marker, so turn 2
// can still hit the cache turn 1 created.
func coldify(ts []tool, nonce, scenarioID string) []tool {
	out := make([]tool, len(ts))
	copy(out, ts)
	if len(out) > 0 {
		out[0].Description = fmt.Sprintf("[probe run %s scenario %s] %s", nonce, scenarioID, out[0].Description)
	}
	return out
}

// withBreakpointAt returns a copy of the tool slice with an ephemeral
// cache_control breakpoint on the tool at idx (or the last tool if idx is
// negative or out of range).
func withBreakpointAt(ts []tool, idx int) []tool {
	out := make([]tool, len(ts))
	copy(out, ts)
	i := bpIndex(out, idx)
	if i >= 0 {
		out[i].CacheControl = json.RawMessage(cacheBreakpoint)
	}
	return out
}

func bpIndex(ts []tool, idx int) int {
	if len(ts) == 0 {
		return -1
	}
	if idx < 0 || idx >= len(ts) {
		return len(ts) - 1
	}
	return idx
}

func dropIndex(ts []tool, i int) []tool {
	out := make([]tool, 0, len(ts)-1)
	out = append(out, ts[:i]...)
	out = append(out, ts[i+1:]...)
	return out
}

func report(label string, r turnResult) {
	if r.Err != nil {
		fmt.Printf("%s: ERROR: %v\n", label, r.Err)
		return
	}
	fmt.Printf("%s: input=%d cache_creation=%d cache_read=%d output=%d ttft=%.3fs total=%.3fs\n",
		label, r.Usage.InputTokens, r.Usage.CacheCreationInputTokens,
		r.Usage.CacheReadInputTokens, r.Usage.OutputTokens,
		r.TTFT.Seconds(), r.Total.Seconds())
}

func call(ctx context.Context, cl *http.Client, base, key, model, sys string, tools []tool) turnResult {
	body := map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     true,
		"system": []map[string]any{{
			"type": "text",
			"text": sys,
		}},
		"tools": tools,
		"messages": []map[string]any{{
			"role":    "user",
			"content": "Reply with the single word: ok",
		}},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return turnResult{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return turnResult{Err: err}
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	start := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return turnResult{Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return turnResult{Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, redact(buf.String()))}
	}

	var res turnResult
	var ttftSet bool
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage usage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			res.Usage = ev.Message.Usage
		case "content_block_delta":
			if !ttftSet && ev.Delta.Text != "" {
				res.TTFT = time.Since(start)
				ttftSet = true
			}
		case "message_delta":
			if len(ev.Usage) > 0 {
				var u usage
				if json.Unmarshal(ev.Usage, &u) == nil && u.OutputTokens > 0 {
					res.Usage.OutputTokens = u.OutputTokens
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return turnResult{Err: err}
	}
	res.Total = time.Since(start)
	return res
}

// redact scrubs anything that looks like a bearer token or api key out of
// an error body before it is printed.
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

func loadFat(dir string) ([]tool, error) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.tools.json"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no *.tools.json in %s (run the toolsize probe with -dump first)", dir)
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
			return nil, fmt.Errorf("%s: %w", e, err)
		}
		all = append(all, ts...)
	}
	return all, nil
}

func thinCatalog(fat []tool) []tool {
	// A thin catalog entry is a name + one-line description with a
	// minimal schema; the search/select pair carries real schemas.
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
			Description: "Search the catalog of available MCP tools by keyword. Returns matching tool names with their one-line descriptions. Use this when you need a capability that is not among the tools currently loaded.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Keywords describing the capability you need."},"limit":{"type":"integer","description":"Maximum number of matches to return (default 10)."}},"required":["query"]}`),
		},
		tool{
			Name:        "mcp_load_tool",
			Description: "Load the full input schema for one or more MCP tools by name, making them callable for the remainder of the session. Call this after mcp_search_tools identifies the tool you need.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Exact tool names to load, as reported by mcp_search_tools."}},"required":["names"]}`),
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
