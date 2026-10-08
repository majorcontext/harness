package gates

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

func str(ev map[string]any, keys ...string) string {
	var cur any = ev
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

func firstBlockType(ev map[string]any) string {
	msg, _ := ev["message"].(map[string]any)
	switch c := msg["content"].(type) {
	case string:
		return "string"
	case []any:
		if len(c) > 0 {
			if b, ok := c[0].(map[string]any); ok {
				return str(b, "type")
			}
		}
	}
	return "empty"
}

// AnthropicWire is the Anthropic Messages event stream.
var AnthropicWire = Wire{
	Name: "anthropic",
	Kind: func(ev map[string]any) string {
		switch t := str(ev, "type"); t {
		case "content_block_start":
			return t + ":" + str(ev, "content_block", "type")
		case "content_block_delta":
			return t + ":" + str(ev, "delta", "type")
		default:
			return t
		}
	},
	FreeForm: []string{"input"},
}

// ChatWire is the OpenAI chat-completions chunk stream.
var ChatWire = Wire{
	Name: "chat",
	Kind: func(ev map[string]any) string {
		if _, ok := ev["error"]; ok {
			return "error"
		}
		return "chunk"
	},
}

// ResponsesWire is the OpenAI Responses event stream.
var ResponsesWire = Wire{
	Name: "responses",
	Kind: func(ev map[string]any) string {
		switch t := str(ev, "type"); t {
		case "response.output_item.added", "response.output_item.done":
			return t + ":" + str(ev, "item", "type")
		default:
			return t
		}
	},
	FreeForm: []string{"parameters", "metadata"},
}

// ClaudeCodeWire is the stream-json output of the Claude Code CLI.
var ClaudeCodeWire = Wire{
	Name: "claudecode",
	Kind: func(ev map[string]any) string {
		kind := str(ev, "type")
		if sub := str(ev, "subtype"); sub != "" {
			kind += "/" + sub
		}
		if kind == "assistant" || kind == "user" {
			kind += ":" + firstBlockType(ev)
		}
		return kind
	},
	FreeForm: []string{"input", "tool_use_result", "entries"},
	MapPaths: []string{"modelUsage", "rate_limit_info.unifiedWindows", "subagent_stats.by_type"},
}

// ParserRead is one field that a parser reads from the events of a kind. Kind
// is the event kind without its sub-kind, or "*" for any kind.
type ParserRead struct {
	Wire, Kind string
	Read
}

func readsOf(wire string, kinds []string, prefix string, reads []Read) []ParserRead {
	var out []ParserRead
	for _, k := range kinds {
		for _, r := range reads {
			out = append(out, ParserRead{wire, k, Read{join(prefix, r.Path), r.Type}})
		}
	}
	return out
}

func caseParserReads(fsys fs.FS, wire, dir, file, fn string, extra map[string][]string, prefixes map[string]string) ([]ParserRead, error) {
	cases, err := CaseReads(fsys, dir, file, fn)
	if err != nil {
		return nil, err
	}
	var out []ParserRead
	for _, label := range Labels(cases) {
		kinds := []string{label}
		if over, ok := extra[label]; ok {
			kinds = over
		}
		for _, sr := range cases[label] {
			out = append(out, readsOf(wire, kinds, prefixes[sr.Var], sr.Reads)...)
		}
	}
	return out, nil
}

// ParserReads lists every field that the harness parsers decode from a wire,
// derived from the Go source of the parsers.
func ParserReads(fsys fs.FS) ([]ParserRead, error) {
	var out []ParserRead
	add := func(rs []ParserRead, err error) error {
		out = append(out, rs...)
		return err
	}
	httpError := map[string][]string{"": {errorBodyKind}}
	if err := add(caseParserReads(fsys, "anthropic", "provider/anthropic", "anthropic.go", "handle", map[string][]string{"error": {errorBodyKind}}, nil)); err != nil {
		return nil, err
	}
	if err := add(caseParserReads(fsys, "anthropic", "provider/anthropic", "anthropic.go", "apiError", httpError, nil)); err != nil {
		return nil, err
	}
	if err := add(caseParserReads(fsys, "responses", "provider/openai", "openai.go", "handle", nil, map[string]string{"head": "item"})); err != nil {
		return nil, err
	}
	if err := add(caseParserReads(fsys, "responses", "provider/openai", "openai.go", "apiError", httpError, nil)); err != nil {
		return nil, err
	}
	if err := add(caseParserReads(fsys, "chat", "provider/openaicompat", "openaicompat.go", "apiError", httpError, nil)); err != nil {
		return nil, err
	}
	chunk, err := TypeReads(fsys, "provider/openaicompat", "wireChunk")
	if err != nil {
		return nil, err
	}
	out = append(out, readsOf("chat", []string{"chunk"}, "", chunk)...)
	cc, err := claudeCodeReads(fsys)
	return append(out, cc...), err
}

func claudeCodeReads(fsys fs.FS) ([]ParserRead, error) {
	const dir = "internal/backend/claudecode"
	var out []ParserRead
	for _, t := range []struct{ name, prefix string }{
		{"envelope", ""}, {"wireMessage", "message"}, {"block", "message.content[]"},
	} {
		reads, err := TypeReads(fsys, dir, t.name)
		if err != nil {
			return nil, err
		}
		out = append(out, readsOf("claudecode", []string{"*"}, t.prefix, reads)...)
	}
	return out, nil
}

func baseKind(kind string) string { return strings.SplitN(kind, ":", 2)[0] }

// ParserDifferences lists each field that a parser reads and the recording
// lacks, or has with a type the parser cannot decode.
func ParserDifferences(wire string, real Shape, reads []ParserRead, allowed *Allowed) []string {
	var out []string
	seenKind := map[string]bool{}
	for _, r := range reads {
		if r.Wire != wire {
			continue
		}
		var kinds []string
		for _, k := range slices.Sorted(maps.Keys(real)) {
			if r.Kind == "*" || slices.Contains(strings.Split(r.Kind, "|"), baseKind(k)) {
				kinds = append(kinds, k)
			}
		}
		if len(kinds) == 0 {
			if !seenKind[r.Kind] && !allowed.Allows("parser", wire, r.Kind, "*") {
				out = append(out, fmt.Sprintf("%s: parser reads kind %q; no recording holds it", wire, r.Kind))
			}
			seenKind[r.Kind] = true
			continue
		}
		types := map[string]bool{}
		for _, k := range kinds {
			maps.Copy(types, real[k][r.Path])
		}
		switch {
		case len(types) == 0:
			if !allowed.Allows("parser", wire, r.Kind, r.Path) {
				out = append(out, fmt.Sprintf("%s: parser reads %s.%s; the recordings lack it", wire, r.Kind, r.Path))
			}
		case r.Type != "" && !decodable(types, r.Type):
			if !allowed.Allows("parser", wire, r.Kind, r.Path) {
				out = append(out, fmt.Sprintf("%s: parser reads %s.%s as %s; the recordings have %s", wire, r.Kind, r.Path, r.Type, strings.Join(slices.Sorted(maps.Keys(types)), "|")))
			}
		}
	}
	return out
}

// decodable reports whether a Go type that needs want can decode every type
// seen. A null decodes into any Go type.
func decodable(seen map[string]bool, want string) bool {
	for typ := range seen {
		if typ != "null" && typ != want {
			return false
		}
	}
	return true
}
