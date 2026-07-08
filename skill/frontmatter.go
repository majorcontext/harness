package skill

import (
	"errors"
	"fmt"
	"strings"
)

// delimiter is the frontmatter fence line.
const delimiter = "---"

// knownKeys are the only top-level frontmatter keys the specification defines.
// Any other key is rejected (spec-first strictness).
var knownKeys = map[string]bool{
	"name":          true,
	"description":   true,
	"license":       true,
	"compatibility": true,
	"metadata":      true,
	"allowed-tools": true,
}

// splitFrontmatter separates the YAML frontmatter block from the Markdown
// body. The document must begin with a "---" line and the frontmatter must be
// closed by a matching "---" line; the body is everything after it. Errors
// name the malformed-delimiter condition.
func splitFrontmatter(doc string) (frontmatter, body string, err error) {
	// Normalize CRLF so line-based parsing is uniform.
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	lines := strings.Split(doc, "\n")

	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != delimiter {
		return "", "", errors.New("missing frontmatter: file must begin with a '---' delimiter line")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == delimiter {
			fm := strings.Join(lines[1:i], "\n")
			body := strings.Join(lines[i+1:], "\n")
			return fm, body, nil
		}
	}
	return "", "", errors.New("unterminated frontmatter: no closing '---' delimiter found")
}

// parseFrontmatter parses the frontmatter block into top-level scalar fields
// and an optional one-level-deep metadata map. See the package doc for the
// supported subset of YAML.
func parseFrontmatter(fm string) (fields map[string]string, meta map[string]string, err error) {
	fields = make(map[string]string)
	lines := strings.Split(fm, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Top-level entries must not be indented.
		if line != strings.TrimLeft(line, " \t") {
			return nil, nil, fmt.Errorf("unexpected indented line in frontmatter: %q", trimmed)
		}

		key, value, ok := splitKeyValue(trimmed)
		if !ok {
			return nil, nil, fmt.Errorf("malformed frontmatter line (expected 'key: value'): %q", trimmed)
		}
		if !knownKeys[key] {
			return nil, nil, fmt.Errorf("unknown frontmatter key: %q", key)
		}

		if key == "metadata" {
			if meta != nil {
				return nil, nil, errors.New("duplicate frontmatter key: \"metadata\"")
			}
			if value != "" {
				return nil, nil, errors.New("metadata must be a block mapping, not an inline value")
			}
			block, next, perr := parseMetadataBlock(lines, i+1)
			if perr != nil {
				return nil, nil, perr
			}
			meta = block
			i = next - 1
			continue
		}

		if _, dup := fields[key]; dup {
			return nil, nil, fmt.Errorf("duplicate frontmatter key: %q", key)
		}
		fields[key] = unquote(value)
	}
	return fields, meta, nil
}

// parseMetadataBlock reads the indented "key: value" entries following a
// "metadata:" line, starting at line index start. It returns the parsed map,
// the index of the first line that is not part of the block, and any error.
func parseMetadataBlock(lines []string, start int) (map[string]string, int, error) {
	meta := make(map[string]string)
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		// The block ends at the first non-indented line.
		if line == strings.TrimLeft(line, " \t") {
			break
		}
		entry := strings.TrimSpace(line)
		if strings.HasPrefix(entry, "#") {
			continue
		}
		key, value, ok := splitKeyValue(entry)
		if !ok {
			return nil, 0, fmt.Errorf("malformed metadata entry (expected 'key: value'): %q", entry)
		}
		if value == "" {
			return nil, 0, fmt.Errorf("metadata entry %q must have a string value; nested mappings are not supported", key)
		}
		if _, dup := meta[key]; dup {
			return nil, 0, fmt.Errorf("duplicate metadata key: %q", key)
		}
		meta[key] = unquote(value)
	}
	if len(meta) == 0 {
		return nil, i, errors.New("metadata block is empty")
	}
	return meta, i, nil
}

// splitKeyValue splits a "key: value" line at the first colon. The value may
// be empty (e.g. a block-opening "metadata:"). It reports whether a colon was
// found.
func splitKeyValue(line string) (key, value string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

// unquote strips a single matching pair of surrounding double or single
// quotes. No escape processing is performed; this is the minimal handling the
// spec's scalar values need.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
