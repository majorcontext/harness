package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// SkillsExtension is the identifier of the MCP Skills extension.
const SkillsExtension = "io.modelcontextprotocol/skills"

const (
	methodSkillsList        = "skills/list"
	methodSkillsGet         = "skills/get"
	methodResourcesDirRead  = "resources/directory/read"
	maxListAllSkillsPages   = 100
	dynamicSkillResources   = "dynamic"
	directoryReadSettingKey = "directoryRead"
)

// SkillResource is one file of a skill with the SHA-256 digest and the size of its raw bytes.
type SkillResource struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Skill is the entry of one skill: the URI of its SKILL.md, its frontmatter,
// and its files. Dynamic is true when the server publishes no digests.
type Skill struct {
	URI         string
	Frontmatter map[string]any
	Resources   []SkillResource
	Dynamic     bool
}

// UnmarshalJSON reads a skill entry. An entry whose resources is neither an
// array nor "dynamic" is an error.
func (s *Skill) UnmarshalJSON(b []byte) error {
	var wire struct {
		URI         string          `json:"uri"`
		Frontmatter map[string]any  `json:"frontmatter"`
		Resources   json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*s = Skill{URI: wire.URI, Frontmatter: wire.Frontmatter}
	raw := bytes.TrimSpace(wire.Resources)
	if bytes.Equal(raw, []byte(`"`+dynamicSkillResources+`"`)) {
		s.Dynamic = true
		return nil
	}
	if len(raw) == 0 || raw[0] != '[' {
		return fmt.Errorf("skill %s: resources is neither an array nor %q", wire.URI, dynamicSkillResources)
	}
	return json.Unmarshal(raw, &s.Resources)
}

// MarshalJSON writes the wire form of the entry.
func (s Skill) MarshalJSON() ([]byte, error) {
	var resources any = s.Resources
	if s.Dynamic {
		resources = dynamicSkillResources
	} else if s.Resources == nil {
		resources = []SkillResource{}
	}
	return json.Marshal(struct {
		URI         string         `json:"uri"`
		Frontmatter map[string]any `json:"frontmatter"`
		Resources   any            `json:"resources"`
	}{s.URI, s.Frontmatter, resources})
}

// ListSkillsResult is one page of skills/list.
type ListSkillsResult struct {
	Skills     []json.RawMessage `json:"skills"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// SkillsSupport reports whether the server declared the Skills extension, and
// whether it serves resources/directory/read.
func (c ServerCapabilities) SkillsSupport() (skills, directoryRead bool) {
	raw, ok := c.Extensions[SkillsExtension]
	if !ok {
		return false, false
	}
	var settings map[string]any
	_ = json.Unmarshal(raw, &settings)
	read, _ := settings[directoryReadSettingKey].(bool)
	return true, read
}

// ListAllSkills reads every page of skills/list. An entry that does not
// decode is left out and its error joins the returned one, so the caller
// keeps the valid entries.
func (c *Client) ListAllSkills(ctx context.Context) ([]Skill, error) {
	var out []Skill
	var bad []error
	seen := map[string]bool{}
	cursor := ""
	for range maxListAllSkillsPages {
		var page ListSkillsResult
		if err := c.request(ctx, methodSkillsList, listToolsParams{Cursor: cursor}, &page); err != nil {
			return out, fmt.Errorf("mcp: skills/list: %w", err)
		}
		for _, raw := range page.Skills {
			var s Skill
			if err := json.Unmarshal(raw, &s); err != nil {
				bad = append(bad, err)
				continue
			}
			out = append(out, s)
		}
		if page.NextCursor == "" {
			return out, errors.Join(bad...)
		}
		if seen[page.NextCursor] {
			return out, fmt.Errorf("mcp: skills/list: server returned non-advancing cursor %q", page.NextCursor)
		}
		seen[page.NextCursor], cursor = true, page.NextCursor
	}
	return out, fmt.Errorf("mcp: skills/list: exceeded %d pages without terminating", maxListAllSkillsPages)
}

// GetSkill reads the entry of the skill whose SKILL.md is uri.
func (c *Client) GetSkill(ctx context.Context, uri string) (Skill, error) {
	var result struct {
		Skill Skill `json:"skill"`
	}
	if err := c.request(ctx, methodSkillsGet, readResourceParams{URI: uri}, &result); err != nil {
		return Skill{}, fmt.Errorf("mcp: skills/get %s: %w", uri, err)
	}
	return result.Skill, nil
}

// ReadDirectory requests one page of the direct children of a directory resource.
func (c *Client) ReadDirectory(ctx context.Context, uri, cursor string) (*ListResourcesResult, error) {
	var result ListResourcesResult
	params := struct {
		URI    string `json:"uri"`
		Cursor string `json:"cursor,omitempty"`
	}{uri, cursor}
	if err := c.request(ctx, methodResourcesDirRead, params, &result); err != nil {
		return nil, fmt.Errorf("mcp: resources/directory/read %s: %w", uri, err)
	}
	return &result, nil
}
