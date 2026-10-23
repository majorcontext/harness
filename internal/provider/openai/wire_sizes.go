package openai

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/majorcontext/harness/internal/provider"
)

// wireSizes are the serialized byte sizes of a request and of its parts.
type wireSizes struct {
	request, instructions, tools, history, maxOutput int
}

// measureBody sizes the parts of a serialized request body of maxOutput tokens.
func measureBody(body []byte, maxOutput int) wireSizes {
	parts := provider.BodyParts(body)
	return wireSizes{request: len(body), instructions: len(parts["instructions"]), tools: len(parts["tools"]), history: len(parts["input"]), maxOutput: maxOutput}
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

// toolsDiff names the first function tool, by position, whose entry differs
// between two tool lists, and the part of it that differs: name, description,
// schema, or order. A longer list names the first tool past the shorter. When
// only the entries that are not functions differ, the part is other.
func toolsDiff(previous, current []apiToolDef) (tool, part string) {
	pf, pOther := splitTools(previous)
	cf, cOther := splitTools(current)
	n := min(len(pf), len(cf))
	for i := range n {
		p, c := pf[i], cf[i]
		switch {
		case p.Name != c.Name:
			if sameNames(pf, cf) {
				return c.Name, "order"
			}
			return c.Name, "name"
		case p.Description != c.Description:
			return c.Name, "description"
		case !rawJSONEqual(p.Parameters, c.Parameters):
			return c.Name, "schema"
		}
	}
	if len(pf) > n {
		return pf[n].Name, "name"
	}
	if len(cf) > n {
		return cf[n].Name, "name"
	}
	for i := range min(len(pOther), len(cOther)) {
		if !reflect.DeepEqual(pOther[i], cOther[i]) {
			return cOther[i].Name, "other"
		}
	}
	return "", "other"
}

// splitTools separates the function tools of tools from the other entries, in order.
func splitTools(tools []apiToolDef) (functions, others []apiToolDef) {
	for _, t := range tools {
		if t.Type == "function" {
			functions = append(functions, t)
		} else {
			others = append(others, t)
		}
	}
	return functions, others
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
