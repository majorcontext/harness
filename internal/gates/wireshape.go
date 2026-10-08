package gates

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
)

// Shape maps an event kind to the field paths of its events and the JSON
// types seen at each path. A path joins keys with ".", marks array elements
// with "[]", and names a key of a keyed map "*".
type Shape map[string]map[string]map[string]bool

func (s Shape) add(kind, path, typ string) {
	if s[kind] == nil {
		s[kind] = map[string]map[string]bool{}
	}
	if s[kind][path] == nil {
		s[kind][path] = map[string]bool{}
	}
	s[kind][path][typ] = true
}

// Wire describes how to read the events of one wire format.
type Wire struct {
	Name string
	// Kind names the kind of an event. A sub-kind follows a ":" so a reader
	// can match the whole family by the text before it.
	Kind func(ev map[string]any) string
	// FreeForm names the keys whose value has no fixed shape: a tool input or a
	// payload of another wire. The walk records their type and stops.
	FreeForm []string
	// MapPaths names the paths whose keys are data, such as a model name.
	MapPaths []string
}

const errorBodyKind = "error-body"

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64, json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	default:
		return "object"
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (w Wire) freeForm(path string) bool {
	last := path[strings.LastIndexByte(path, '.')+1:]
	return slices.Contains(w.FreeForm, strings.TrimSuffix(last, "[]"))
}

func (w Wire) walk(s Shape, kind, path string, v any) {
	if path != "" {
		s.add(kind, path, jsonType(v))
		if w.freeForm(path) {
			if elems, ok := v.([]any); ok {
				for _, e := range elems {
					s.add(kind, path+"[]", jsonType(e))
				}
			}
			return
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if slices.Contains(w.MapPaths, path) {
				k = "*"
			}
			w.walk(s, kind, join(path, k), child)
		}
	case []any:
		for _, child := range x {
			w.walk(s, kind, path+"[]", child)
		}
	}
}

// Observe adds one event to the shape under its kind.
func (w Wire) Observe(s Shape, ev map[string]any) {
	kind := w.Kind(ev)
	if _, ok := s[kind]; !ok {
		s[kind] = map[string]map[string]bool{}
	}
	w.walk(s, kind, "", ev)
}

// ObserveError adds one HTTP error body to the shape.
func (w Wire) ObserveError(s Shape, body map[string]any) {
	s[errorBodyKind] = map[string]map[string]bool{}
	w.walk(s, errorBodyKind, "", body)
}

// DecodeSSE returns the JSON payload of each data line of an event stream. It
// skips the [DONE] sentinel of the chat-completions wire.
func DecodeSSE(data []byte) ([]map[string]any, error) {
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "" || payload == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf("event %q: %w", payload, err)
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// DecodeJSONL returns the frames of a JSON Lines stream.
func DecodeJSONL(data []byte) ([]map[string]any, error) {
	var out []map[string]any
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("frame %q: %w", line, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// RecordedShape reads the recordings of one wire from fsys and merges their
// events into one Shape. A file named *.error.json holds an HTTP error.
func RecordedShape(fsys fs.FS, w Wire, files []string) (Shape, error) {
	s := Shape{}
	for _, name := range files {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		switch {
		case strings.HasSuffix(name, ".error.json"):
			var rec struct {
				Body map[string]any `json:"body"`
			}
			if err := json.Unmarshal(data, &rec); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			w.ObserveError(s, rec.Body)
		case strings.HasSuffix(name, ".sse"), strings.HasSuffix(name, ".jsonl"):
			decode := DecodeSSE
			if strings.HasSuffix(name, ".jsonl") {
				decode = DecodeJSONL
			}
			events, err := decode(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for _, ev := range events {
				w.Observe(s, ev)
			}
		default:
			return nil, fmt.Errorf("%s: unknown recording type", name)
		}
	}
	return s, nil
}

// Allowance is a reviewed exception to the wire gate.
type Allowance struct{ Role, Wire, Kind, Path string }

// Allowed holds the reviewed exceptions, each with its reason, and which of
// them a check used.
type Allowed struct {
	Reasons map[Allowance]string
	used    map[Allowance]bool
}

// Allows reports whether an exception covers the path of the kind. An
// exception path of "*" covers the whole kind, and one that ends in "+" covers
// every path that starts with the text before it.
func (a *Allowed) Allows(role, wire, kind, path string) bool {
	for key := range a.Reasons {
		if key.Role != role || key.Wire != wire || key.Kind != kind {
			continue
		}
		prefix, isPrefix := strings.CutSuffix(key.Path, "+")
		if key.Path == "*" || key.Path == path || (isPrefix && strings.HasPrefix(path, prefix)) {
			a.used[key] = true
			return true
		}
	}
	return false
}

// Unused lists the exceptions that no check needed.
func (a *Allowed) Unused() []string {
	var out []string
	for key := range a.Reasons {
		if !a.used[key] {
			out = append(out, fmt.Sprintf("%s %s %s %s", key.Role, key.Wire, key.Kind, key.Path))
		}
	}
	slices.Sort(out)
	return out
}

// ParseAllowed reads lines of "role wire kind path reason". A line with no
// reason is an error.
func ParseAllowed(data []byte) (*Allowed, error) {
	out := &Allowed{Reasons: map[Allowance]string{}, used: map[Allowance]bool{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 || (f[0] != "fake" && f[0] != "parser") {
			return nil, fmt.Errorf("allowed line needs role (fake or parser), wire, kind, path, and a reason: %q", line)
		}
		out.Reasons[Allowance{f[0], f[1], f[2], f[3]}] = strings.Join(f[4:], " ")
	}
	return out, nil
}

// FakeDifferences lists what a fake emits that the real wire does not: a kind
// the recording lacks, a field the kind lacks, or a type the field never has.
func FakeDifferences(wire string, real, fake Shape, allowed *Allowed) []string {
	var out []string
	for _, kind := range slices.Sorted(maps.Keys(fake)) {
		realPaths, ok := real[kind]
		if !ok {
			if !allowed.Allows("fake", wire, kind, "*") {
				out = append(out, fmt.Sprintf("%s: fake emits kind %q that the real wire never sent", wire, kind))
			}
			continue
		}
		for _, path := range slices.Sorted(maps.Keys(fake[kind])) {
			if allowed.Allows("fake", wire, kind, path) {
				continue
			}
			realTypes, ok := realPaths[path]
			if !ok {
				out = append(out, fmt.Sprintf("%s: fake invents field %s.%s", wire, kind, path))
				continue
			}
			for _, typ := range slices.Sorted(maps.Keys(fake[kind][path])) {
				if !realTypes[typ] {
					out = append(out, fmt.Sprintf("%s: fake gives %s.%s type %s; real has %s", wire, kind, path, typ, strings.Join(slices.Sorted(maps.Keys(realTypes)), "|")))
				}
			}
		}
	}
	return out
}
