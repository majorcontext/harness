// Command genctx generates modelmeta's context-window tables from the
// models.dev catalog. Run it through "go generate" in package modelmeta.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const defaultSource = "https://models.dev/api.json"

type table struct {
	varName  string
	provider string
	doc      string
	// key maps a models.dev model ID to the table key, or "" to skip it.
	key func(id string) string
}

var tables = []table{
	{"anthropicContextWindows", "anthropic", "models.dev \"anthropic\" entries, keyed by model ID.", identityKey},
	{"openaiContextWindows", "openai", "models.dev \"openai\" entries, keyed by model ID.", identityKey},
	{"bedrockAnthropicContextWindows", "amazon-bedrock", "models.dev \"amazon-bedrock\" anthropic.* entries, keyed by model ID without region prefix, \"anthropic.\" family segment, or version suffix. Bedrock can report a different window than the first-party route for the same model, and lookup keeps that divergence.", bedrockKey},
	{"bifrostFireworksContextWindows", "fireworks-ai", "models.dev \"fireworks-ai\" entries, keyed by the last path segment.", lastSegmentKey},
	{"bifrostVertexContextWindows", "google-vertex", "models.dev \"google-vertex\" Gemini entries, keyed by model ID.", geminiKey},
}

var bedrockPrefixPattern = regexp.MustCompile(`^(?:[a-z]+\.)?anthropic\.`)
var bedrockVersionSuffixPattern = regexp.MustCompile(`-v\d+(:\d+)?$`)

// isChat reports whether an entry is a text-output model with a chat
// capability flag. Embedding entries carry a positive context but set none.
func isChat(output []string, toolCall, reasoning, temperature bool) bool {
	return len(output) == 1 && output[0] == "text" && (toolCall || reasoning || temperature)
}

func identityKey(id string) string { return id }

func lastSegmentKey(id string) string {
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		return id[i+1:]
	}
	return id
}

func bedrockKey(id string) string {
	loc := bedrockPrefixPattern.FindStringIndex(id)
	if loc == nil {
		return ""
	}
	return bedrockVersionSuffixPattern.ReplaceAllString(id[loc[1]:], "")
}

func geminiKey(id string) string {
	if strings.HasPrefix(id, "gemini-") {
		return id
	}
	return ""
}

type catalog map[string]struct {
	Name   string `json:"name"`
	Models map[string]struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		ReleaseDate      string `json:"release_date"`
		Status           string `json:"status"`
		ReasoningOptions []struct {
			Type   string   `json:"type"`
			Values []string `json:"values"`
		} `json:"reasoning_options"`
		Limit struct {
			Context int `json:"context"`
		} `json:"limit"`
		Modalities struct {
			Input  []string `json:"input"`
			Output []string `json:"output"`
		} `json:"modalities"`
		ToolCall    bool `json:"tool_call"`
		Reasoning   bool `json:"reasoning"`
		Temperature bool `json:"temperature"`
	} `json:"models"`
}

// overrideEntry is one hand-written entry for a model that models.dev lacks
// or reports wrongly. Note is documentation for maintainers only. OffType
// names the thinking type that the off level sends, and only an entry of a
// thinking-disable table may set it.
type overrideEntry struct {
	Context int    `json:"context"`
	OffType string `json:"offType"`
	Note    string `json:"note"`
}

// offTypeDisabled and offTypeBetweenTools are the thinking types that the off level can send. An entry without an offType sends the first.
const (
	offTypeDisabled     = "disabled"
	offTypeBetweenTools = "between_tools"
)

// thinkingOffTable names the overrides of the models that run with thinking off when a request omits the thinking field.
const thinkingOffTable = "thinkingOffByDefault"

// thinkingDisableSuffix ends the name of the override table of a models.dev provider whose models accept a disabled thinking request but that models.dev lists without an off level. The provider name starts the table name.
const thinkingDisableSuffix = "ThinkingCanBeDisabled"

type overrides map[string]map[string]overrideEntry

func main() {
	src := flag.String("in", defaultSource, "models.dev catalog: URL or file path")
	ovr := flag.String("overrides", "overrides.json", "hand-written overrides file")
	out := flag.String("out", "context_windows_gen.go", "generated Go file")
	infoOut := flag.String("info-out", "model_info_gen.go", "generated Go file of model facts")
	flag.Parse()
	if err := run(*src, *ovr, *out, *infoOut); err != nil {
		fmt.Fprintln(os.Stderr, "genctx:", err)
		os.Exit(1)
	}
}

func run(src, ovrPath, outPath, infoPath string) error {
	cat, err := readSource(src)
	if err != nil {
		return err
	}
	ovrData, err := os.ReadFile(ovrPath)
	if err != nil {
		return err
	}
	code, notes, err := generate(cat, ovrData)
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "genctx: note:", n)
	}
	info, infoNotes, err := generateInfo(cat, ovrData)
	if err != nil {
		return err
	}
	for _, n := range infoNotes {
		fmt.Fprintln(os.Stderr, "genctx: note:", n)
	}
	if err := os.WriteFile(infoPath, info, 0o644); err != nil {
		return err
	}
	return os.WriteFile(outPath, code, 0o644)
}

func readSource(src string) ([]byte, error) {
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		return os.ReadFile(src)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(src)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", src, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// generate renders the Go source for catalogJSON merged with ovrJSON. Equal
// inputs give byte-identical output. The returned notes name overrides that
// shadow a different models.dev value or repeat the same one.
func generate(catalogJSON, ovrJSON []byte) (code []byte, notes []string, err error) {
	var cat catalog
	if err := json.Unmarshal(catalogJSON, &cat); err != nil {
		return nil, nil, fmt.Errorf("decode catalog: %w", err)
	}
	var ovr overrides
	if err := json.Unmarshal(ovrJSON, &ovr); err != nil {
		return nil, nil, fmt.Errorf("decode overrides: %w", err)
	}
	known := map[string]bool{}
	for _, t := range tables {
		known[t.varName] = true
	}
	known[thinkingOffTable] = true
	for _, t := range infoTables {
		known[t.provider+thinkingDisableSuffix] = true
	}
	for name, entries := range ovr {
		if !known[name] {
			return nil, nil, fmt.Errorf("overrides name unknown table %q", name)
		}
		if strings.HasSuffix(name, thinkingDisableSuffix) {
			continue
		}
		for id, e := range entries {
			if e.OffType != "" {
				return nil, nil, fmt.Errorf("%s[%q] sets offType outside a %s table", name, id, thinkingDisableSuffix)
			}
		}
	}

	var buf bytes.Buffer
	buf.WriteString("// Code generated by go generate; DO NOT EDIT.\n\npackage modelmeta\n")
	for _, t := range tables {
		prov, ok := cat[t.provider]
		if !ok {
			return nil, nil, fmt.Errorf("catalog has no provider %q", t.provider)
		}
		vals := map[string]int{}
		for id, m := range prov.Models {
			k := t.key(id)
			// Zero context marks a non-chat model; it must stay unknown to callers.
			if k == "" || m.Limit.Context <= 0 || !isChat(m.Modalities.Output, m.ToolCall, m.Reasoning, m.Temperature) {
				continue
			}
			// Several IDs can share a key; the smallest window compacts earliest.
			if cur, dup := vals[k]; !dup || m.Limit.Context < cur {
				vals[k] = m.Limit.Context
			}
		}
		for k, e := range ovr[t.varName] {
			if e.Context <= 0 {
				return nil, nil, fmt.Errorf("override %s[%q] needs a positive context", t.varName, k)
			}
			switch cur, ok := vals[k]; {
			case ok && cur == e.Context:
				notes = append(notes, fmt.Sprintf("%s[%q] override repeats models.dev value %d", t.varName, k, cur))
			case ok:
				notes = append(notes, fmt.Sprintf("%s[%q] override %d shadows models.dev value %d", t.varName, k, e.Context, cur))
			}
			vals[k] = e.Context
		}
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&buf, "\n// %s holds %s\nvar %s = map[string]int{\n", t.varName, t.doc, t.varName)
		for _, k := range keys {
			fmt.Fprintf(&buf, "\t%q: %d,\n", k, vals[k])
		}
		buf.WriteString("}\n")
	}
	sort.Strings(notes)
	code, err = format.Source(buf.Bytes())
	return code, notes, err
}

// infoTables lists the tables of model facts. A table holds the chat models
// of one models.dev provider. offTypes is true when modelmeta reads the
// offType of the thinking-disable table of the provider.
var infoTables = []struct {
	varName, provider string
	offTypes          bool
}{
	{"anthropicInfo", "anthropic", true},
	{"openaiInfo", "openai", false},
}

// latestSuffix marks the undated alias of a model in a models.dev name.
const latestSuffix = " (latest)"

// effortLevels are the effort levels of harness, in display order.
var effortLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// budgetLevels are the levels that a "budget_tokens" option takes. Harness
// maps a level to a budget, and a budget has no level above high.
var budgetLevels = effortLevels[:5]

// efforts returns the effort levels of harness that a model supports. A
// models.dev "none" value and a "toggle" option both mean that the model
// runs without reasoning, which is "off". A "budget_tokens" option takes every
// level up to high, because harness maps a level to a budget. A level that
// harness lacks, such as ultra, is dropped.
func efforts(opts []struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}) []string {
	have := map[string]bool{}
	for _, o := range opts {
		switch o.Type {
		case "toggle":
			have["off"] = true
		case "budget_tokens":
			for _, l := range budgetLevels {
				have[l] = true
			}
		case "effort":
			for _, v := range o.Values {
				if v == "none" {
					v = "off"
				}
				have[v] = true
			}
		}
	}
	var out []string
	for _, l := range effortLevels {
		if have[l] {
			out = append(out, l)
		}
	}
	return out
}

// controls reports whether a model takes a level as a wire effort field
// ("effort") and whether it takes a thinking budget ("budget_tokens").
func controls(opts []struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}) (field, budget bool) {
	for _, o := range opts {
		switch o.Type {
		case "effort":
			field = true
		case "budget_tokens":
			budget = true
		}
	}
	return field, budget
}

// effortLiteral is the Go literal of the efforts of a model. A model that
// models.dev says has no reasoning has an empty list. A model with reasoning
// and no usable option takes every level, as harness maps a level for it. A
// model in the thinking-disable overrides of its provider also lists off.
func effortLiteral(reasoning bool, opts []struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
}, canDisable bool) string {
	if !reasoning {
		return "[]string{}"
	}
	if e := efforts(opts); len(e) > 0 {
		if canDisable && e[0] != "off" {
			e = append([]string{"off"}, e...)
		}
		return fmt.Sprintf("%#v", e)
	}
	return fmt.Sprintf("%#v", budgetLevels)
}

// noImageLiteral is the struct field that marks a model whose models.dev
// input modalities are known and lack images. A model without the list of
// input modalities stays unmarked, because its input is unknown.
func noImageLiteral(input []string) string {
	if len(input) == 0 || slices.Contains(input, "image") {
		return ""
	}
	return ", noImageInput: true"
}

// writeOffTypes writes anthropicThinkingOffType, the model ids whose off level sends a thinking type other than "disabled". The map is empty when no entry sets one. An unknown offType, or an offType in a table that modelmeta does not read, is an error.
func writeOffTypes(buf *bytes.Buffer, ovr overrides) error {
	for _, t := range infoTables {
		table := t.provider + thinkingDisableSuffix
		for _, id := range slices.Sorted(maps.Keys(ovr[table])) {
			switch typ := ovr[table][id].OffType; {
			case typ == "":
			case !t.offTypes:
				return fmt.Errorf("%s[%q] sets offType, which modelmeta does not read for this provider", table, id)
			case typ != offTypeDisabled && typ != offTypeBetweenTools:
				return fmt.Errorf("%s[%q] has unknown offType %q", table, id, typ)
			}
		}
	}
	for _, t := range infoTables {
		if !t.offTypes {
			continue
		}
		table := ovr[t.provider+thinkingDisableSuffix]
		fmt.Fprintf(buf, "\n// %sThinkingOffType holds the thinking type that the off level sends in place of %q for a model of the %s provider.\nvar %sThinkingOffType = map[string]string{\n", t.provider, offTypeDisabled, t.provider, t.provider)
		for _, id := range slices.Sorted(maps.Keys(table)) {
			if typ := table[id].OffType; typ != "" && typ != offTypeDisabled {
				fmt.Fprintf(buf, "\t%q: %q,\n", id, typ)
			}
		}
		buf.WriteString("}\n")
	}
	return nil
}

// generateInfo renders the Go source of the model-fact tables for
// catalogJSON. A model is listed when it supports tool calls and is not
// deprecated. An entry without a name is skipped.
func generateInfo(catalogJSON, ovrJSON []byte) ([]byte, []string, error) {
	var ovr overrides
	if err := json.Unmarshal(ovrJSON, &ovr); err != nil {
		return nil, nil, fmt.Errorf("decode overrides: %w", err)
	}
	var cat catalog
	if err := json.Unmarshal(catalogJSON, &cat); err != nil {
		return nil, nil, fmt.Errorf("decode catalog: %w", err)
	}
	var notes []string
	var buf bytes.Buffer
	buf.WriteString("// Code generated by go generate; DO NOT EDIT.\n\npackage modelmeta\n")
	buf.WriteString("\n// providerNames holds the models.dev display names of the providers, keyed by provider.\nvar providerNames = map[string]string{\n")
	for _, t := range infoTables {
		prov, ok := cat[t.provider]
		if !ok {
			return nil, nil, fmt.Errorf("catalog has no provider %q", t.provider)
		}
		if prov.Name != "" {
			fmt.Fprintf(&buf, "\t%q: %q,\n", t.provider, prov.Name)
		}
	}
	buf.WriteString("}\n")
	buf.WriteString("\n// thinkingOffByDefault holds the Anthropic models that run with thinking off when a request omits the thinking field.\nvar thinkingOffByDefault = map[string]bool{\n")
	for _, id := range slices.Sorted(maps.Keys(ovr[thinkingOffTable])) {
		fmt.Fprintf(&buf, "\t%q: true,\n", id)
	}
	buf.WriteString("}\n")
	if err := writeOffTypes(&buf, ovr); err != nil {
		return nil, nil, err
	}
	for _, t := range infoTables {
		prov := cat[t.provider]
		keys := make([]string, 0, len(prov.Models))
		for id, m := range prov.Models {
			name := strings.TrimSuffix(strings.TrimSpace(m.Name), latestSuffix)
			if name == "" || m.Limit.Context <= 0 || !isChat(m.Modalities.Output, m.ToolCall, m.Reasoning, m.Temperature) {
				continue
			}
			keys = append(keys, id)
		}
		sort.Strings(keys)
		fmt.Fprintf(&buf, "\n// %s holds the facts of the models.dev %q chat models, keyed by model ID.\nvar %s = map[string]modelInfo{\n", t.varName, t.provider, t.varName)
		for _, id := range keys {
			m := prov.Models[id]
			name := strings.TrimSuffix(strings.TrimSpace(m.Name), latestSuffix)
			disableTable := t.provider + thinkingDisableSuffix
			_, canDisable := ovr[disableTable][id]
			if canDisable && slices.Contains(efforts(m.ReasoningOptions), "off") {
				notes = append(notes, fmt.Sprintf("%s[%q] override repeats models.dev, which lists off", disableTable, id))
			}
			listed := m.ToolCall && m.Status != "deprecated"
			field, budget := controls(m.ReasoningOptions)
			var flags string
			if m.Reasoning && field {
				flags += " effortField: true,"
			}
			if m.Reasoning && budget {
				flags += " budgetTokens: true,"
			}
			fmt.Fprintf(&buf, "\t%q: {name: %q, description: %q, released: %q, efforts: %s,%s listed: %t%s},\n",
				id, name, strings.TrimSpace(m.Description), m.ReleaseDate, effortLiteral(m.Reasoning, m.ReasoningOptions, canDisable), flags, listed, noImageLiteral(m.Modalities.Input))
		}
		buf.WriteString("}\n")
	}
	sort.Strings(notes)
	code, err := format.Source(buf.Bytes())
	return code, notes, err
}
