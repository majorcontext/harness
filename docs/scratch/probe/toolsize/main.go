// Command toolsize is a THROWAWAY probe (docs/scratch only, never
// production) that connects to the MCP servers listed in the
// HARNESS_MCP_SERVERS environment variable, runs tools/list against each,
// and reports the byte/estimated-token cost of the "fat" tool prefix
// harness currently ships on every request, alongside a "thin catalog"
// alternative.
//
// It never prints credential values: only header NAMES are reported.
//
// Usage:
//
//	go run ./docs/scratch/probe/toolsize            # summary table
//	go run ./docs/scratch/probe/toolsize -dump out  # also write raw tools/list JSON per server
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/majorcontext/harness/mcp"
)

type serverSpec struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Command []string          `json:"command"`
	Headers map[string]string `json:"headers"`
}

// anthropicTool is the shape the anthropic adapter puts on the wire for
// each tool. We measure the marshalled size of exactly this, because that
// (not the raw MCP envelope) is what occupies the request prefix.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type stat struct {
	Server      string
	Tools       int
	NameBytes   int
	DescBytes   int
	SchemaBytes int
	WireBytes   int // marshalled anthropic tools array for this server
}

func main() {
	dump := flag.String("dump", "", "directory to write raw tools/list JSON per server")
	flag.Parse()

	raw := os.Getenv("HARNESS_MCP_SERVERS")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "HARNESS_MCP_SERVERS is empty")
		os.Exit(1)
	}
	var specs []serverSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		fmt.Fprintf(os.Stderr, "parse HARNESS_MCP_SERVERS: %v\n", err)
		os.Exit(1)
	}

	if *dump != "" {
		if err := os.MkdirAll(*dump, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
			os.Exit(1)
		}
	}

	ctx := context.Background()
	var stats []stat
	var allWire []anthropicTool

	for _, s := range specs {
		if s.URL == "" {
			fmt.Printf("SKIP %s: no url (stdio server, not probed)\n", s.Name)
			continue
		}
		hdrNames := make([]string, 0, len(s.Headers))
		for k := range s.Headers {
			hdrNames = append(hdrNames, k)
		}
		sort.Strings(hdrNames)
		fmt.Printf("CONNECT %s url=%s headers=%v (values redacted)\n", s.Name, s.URL, hdrNames)

		st, tools, err := probe(ctx, s)
		if err != nil {
			fmt.Printf("ERROR %s: %v\n", s.Name, err)
			continue
		}
		stats = append(stats, st)
		allWire = append(allWire, tools...)

		if *dump != "" {
			b, _ := json.MarshalIndent(tools, "", "  ")
			path := filepath.Join(*dump, s.Name+".tools.json")
			if err := os.WriteFile(path, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
			}
		}
	}

	if len(stats) == 0 {
		fmt.Fprintln(os.Stderr, "no servers probed successfully")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("=== FAT PREFIX (what harness ships today: name+description+full input schema) ===")
	fmt.Printf("%-22s %6s %10s %10s %12s %12s %10s\n",
		"server", "tools", "nameB", "descB", "schemaB", "wireB", "~tokens")
	totals := stat{Server: "TOTAL"}
	for _, s := range stats {
		fmt.Printf("%-22s %6d %10d %10d %12d %12d %10d\n",
			s.Server, s.Tools, s.NameBytes, s.DescBytes, s.SchemaBytes, s.WireBytes, s.WireBytes/4)
		totals.Tools += s.Tools
		totals.NameBytes += s.NameBytes
		totals.DescBytes += s.DescBytes
		totals.SchemaBytes += s.SchemaBytes
		totals.WireBytes += s.WireBytes
	}
	fmt.Printf("%-22s %6d %10d %10d %12d %12d %10d\n",
		totals.Server, totals.Tools, totals.NameBytes, totals.DescBytes,
		totals.SchemaBytes, totals.WireBytes, totals.WireBytes/4)

	// Whole-array marshal: the actual bytes of the tools array as one JSON
	// document, which is what the provider request carries.
	arr, err := json.Marshal(allWire)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal combined: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\ncombined tools array marshalled: %d bytes (~%d tokens at 4 B/token)\n", len(arr), len(arr)/4)

	// Thin catalog: per tool, name + first sentence/line of description
	// only, no schema. Plus a hypothetical search/select tool pair.
	thin := make([]map[string]string, 0, len(allWire))
	var thinDescBytes int
	for _, t := range allWire {
		one := oneLine(t.Description)
		thinDescBytes += len(one)
		thin = append(thin, map[string]string{"name": t.Name, "description": one})
	}
	thinArr, _ := json.Marshal(thin)
	pair := hypotheticalPair()
	pairArr, _ := json.Marshal(pair)

	fmt.Println()
	fmt.Println("=== THIN CATALOG ALTERNATIVE (name + one-line description, no schemas) ===")
	fmt.Printf("thin catalog entries:            %d\n", len(thin))
	fmt.Printf("thin one-line description bytes: %d\n", thinDescBytes)
	fmt.Printf("thin catalog marshalled:         %d bytes (~%d tokens)\n", len(thinArr), len(thinArr)/4)
	fmt.Printf("search/select tool pair:         %d bytes (~%d tokens)\n", len(pairArr), len(pairArr)/4)
	total := len(thinArr) + len(pairArr)
	fmt.Printf("thin TOTAL (catalog + pair):     %d bytes (~%d tokens)\n", total, total/4)
	fmt.Printf("\nDELTA fat->thin: %d bytes saved (~%d tokens), %.1f%% reduction\n",
		len(arr)-total, (len(arr)-total)/4, 100*float64(len(arr)-total)/float64(len(arr)))
}

func probe(ctx context.Context, s serverSpec) (stat, []anthropicTool, error) {
	c, err := mcp.NewClient(&mcp.HTTPTransport{
		Endpoint: s.URL,
		Headers:  s.Headers,
	}, mcp.Options{RequestTimeout: 60 * time.Second})
	if err != nil {
		return stat{}, nil, fmt.Errorf("new client: %w", err)
	}
	defer c.Close()

	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := c.Initialize(ictx); err != nil {
		return stat{}, nil, fmt.Errorf("initialize: %w", err)
	}

	lctx, lcancel := context.WithTimeout(ctx, 120*time.Second)
	defer lcancel()
	tools, err := c.ListAllTools(lctx)
	if err != nil {
		return stat{}, nil, fmt.Errorf("tools/list: %w", err)
	}

	st := stat{Server: s.Name, Tools: len(tools)}
	wire := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		// Harness namespaces MCP tools as mcp__<server>__<tool>; measure
		// the name it actually puts on the wire.
		name := "mcp__" + s.Name + "__" + t.Name
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		st.NameBytes += len(name)
		st.DescBytes += len(t.Description)
		st.SchemaBytes += len(schema)
		wire = append(wire, anthropicTool{Name: name, Description: t.Description, InputSchema: schema})
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return stat{}, nil, fmt.Errorf("marshal: %w", err)
	}
	st.WireBytes = len(b)
	return st, wire, nil
}

// oneLine reduces a description to a single summary line: the first
// sentence, capped at 160 chars, which is the shape a thin catalog entry
// would carry.
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

func hypotheticalPair() []anthropicTool {
	return []anthropicTool{
		{
			Name:        "mcp_search_tools",
			Description: "Search the catalog of available MCP tools by keyword. Returns matching tool names with their one-line descriptions. Use this when you need a capability that is not among the tools currently loaded.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Keywords describing the capability you need."},"limit":{"type":"integer","description":"Maximum number of matches to return (default 10)."}},"required":["query"]}`),
		},
		{
			Name:        "mcp_load_tool",
			Description: "Load the full input schema for one or more MCP tools by name, making them callable for the remainder of the session. Call this after mcp_search_tools identifies the tool you need.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"names":{"type":"array","items":{"type":"string"},"description":"Exact tool names to load, as reported by mcp_search_tools."}},"required":["names"]}`),
		},
	}
}
