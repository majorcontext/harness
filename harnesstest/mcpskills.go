package harnesstest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/skill"
)

// MCPSkill is one scripted skill at skill://<Path>/SKILL.md. Files maps each path under the skill root to its
// content, and the entry lists the size and SHA-256 digest of each. Served replaces the content that
// resources/read returns for a path, and Listed replaces the content that the entry of skills/list describes,
// so a scenario can script content that does not match its entry. Frontmatter replaces the frontmatter of every
// entry. An Unlisted skill is absent from skills/list and skills/get still answers it. A Dynamic skill lists no
// files.
type MCPSkill struct {
	Path        string
	Files       map[string]string
	Served      map[string]string `json:",omitempty"`
	Listed      map[string]string `json:",omitempty"`
	Frontmatter map[string]any    `json:",omitempty"`
	Unlisted    bool              `json:",omitempty"`
	Dynamic     bool              `json:",omitempty"`
}

func (s MCPSkill) root() string { return "skill://" + s.Path }

// entry is the skill entry that describes files.
func (s MCPSkill) entry(files map[string]string) mcp.Skill {
	e := mcp.Skill{URI: s.root() + "/" + skill.Filename, Dynamic: s.Dynamic}
	e.Frontmatter, _ = skill.Frontmatter(files[skill.Filename])
	if s.Frontmatter != nil {
		e.Frontmatter = s.Frontmatter
	}
	if s.Dynamic {
		return e
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		sum := sha256.Sum256([]byte(files[p]))
		e.Resources = append(e.Resources, mcp.SkillResource{URI: s.root() + "/" + p, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(files[p]))})
	}
	return e
}

func (h *mcpHandler) skillMethod(method string, params json.RawMessage) (any, *mcp.RPCError) {
	var p struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(params, &p)
	switch method {
	case "skills/list":
		var out []mcp.Skill
		for _, s := range h.spec.Skills {
			if !s.Unlisted {
				out = append(out, s.entry(cmpFiles(s.Listed, s.Files)))
			}
		}
		return map[string]any{"skills": out}, nil
	case "skills/get":
		for _, s := range h.spec.Skills {
			if s.root()+"/"+skill.Filename == p.URI {
				return map[string]any{"skill": s.entry(s.Files)}, nil
			}
		}
		return nil, &mcp.RPCError{Code: -32602, Message: "No skill is served at " + p.URI}
	}
	if !h.spec.DirectoryRead {
		return nil, &mcp.RPCError{Code: -32601, Message: "method not found: " + method}
	}
	children := map[string]mcp.Resource{}
	for _, s := range h.spec.Skills {
		for f := range s.Files {
			rel, ok := strings.CutPrefix(s.root()+"/"+f, p.URI+"/")
			if !ok {
				continue
			}
			if dir, _, nested := strings.Cut(rel, "/"); nested {
				children[dir] = mcp.Resource{URI: p.URI + "/" + dir, Name: dir, MimeType: "inode/directory"}
			} else {
				children[rel] = mcp.Resource{URI: p.URI + "/" + rel, Name: path.Base(rel)}
			}
		}
	}
	if len(children) == 0 {
		return nil, &mcp.RPCError{Code: -32602, Message: p.URI + " is not a directory resource"}
	}
	out := make([]mcp.Resource, 0, len(children))
	for _, k := range slices.Sorted(maps.Keys(children)) {
		out = append(out, children[k])
	}
	return mcp.ListResourcesResult{Resources: out}, nil
}

func cmpFiles(override, files map[string]string) map[string]string {
	if override == nil {
		return files
	}
	out := maps.Clone(files)
	maps.Copy(out, override)
	return out
}

// skillResources lists every skill file.
func (h *mcpHandler) skillResources() []mcp.Resource {
	var out []mcp.Resource
	for _, s := range h.spec.Skills {
		for _, f := range slices.Sorted(maps.Keys(s.Files)) {
			out = append(out, mcp.Resource{URI: s.root() + "/" + f, Name: path.Base(f)})
		}
	}
	return out
}

// skillFile returns the content that resources/read serves for uri.
func (h *mcpHandler) skillFile(uri string) (string, bool) {
	for _, s := range h.spec.Skills {
		files := cmpFiles(s.Served, s.Files)
		for f, text := range files {
			if s.root()+"/"+f == uri {
				return text, true
			}
		}
	}
	return "", false
}
