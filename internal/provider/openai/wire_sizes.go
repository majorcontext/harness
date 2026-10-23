package openai

import (
	"encoding/json"
	"slices"

	"github.com/majorcontext/harness/internal/provider"
)

// wireSizes are the serialized byte sizes of a request and of its parts.
type wireSizes struct {
	request, instructions, tools, history, maxOutput int
}

// measureRequest sizes the parts of w. requestBytes is the size of the body or frame that carries it.
func measureRequest(w *apiRequest, requestBytes int) wireSizes {
	z := wireSizes{request: requestBytes, instructions: len(w.Instructions), history: inputBytes(w.Input), maxOutput: w.MaxOutputTokens}
	if len(w.Tools) > 0 {
		tools, _ := json.Marshal(w.Tools)
		z.tools = len(tools)
	}
	return z
}

// inputBytes is the size of the JSON array that holds items.
func inputBytes(items []json.RawMessage) int {
	n := 2
	for _, it := range items {
		n += len(it) + 1
	}
	return max(n-1, 2)
}

// metadata returns the request facts of z.
func (z wireSizes) metadata() *provider.RequestMetadata {
	return &provider.RequestMetadata{RequestBytes: z.request, InstructionsBytes: z.instructions,
		ToolsBytes: z.tools, HistoryBytes: z.history, MaxOutputTokens: z.maxOutput}
}

// toolsDiff names the first tool, by position, whose entry differs between two
// tool lists, and the part of it that differs: name, description, schema,
// order, or other. A list that is longer names the first tool past the shorter.
func toolsDiff(previous, current []apiToolDef) (tool, part string) {
	n := min(len(previous), len(current))
	for i := range n {
		p, c := previous[i], current[i]
		switch {
		case p.Type != c.Type || c.Type != "function":
			return c.Name, "other"
		case p.Name != c.Name:
			if sameNames(previous, current) {
				return c.Name, "order"
			}
			return c.Name, "name"
		case p.Description != c.Description:
			return c.Name, "description"
		case !rawJSONEqual(p.Parameters, c.Parameters):
			return c.Name, "schema"
		}
	}
	if len(previous) > n {
		return previous[n].Name, "name"
	}
	if len(current) > n {
		return current[n].Name, "name"
	}
	return "", "other"
}

func sameNames(a, b []apiToolDef) bool {
	names := func(tools []apiToolDef) []string {
		out := make([]string, len(tools))
		for i, t := range tools {
			out[i] = t.Name
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}
