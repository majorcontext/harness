package harnesstest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/skill"
)

// MCPSkill is one scripted skill at skill://<Path>/SKILL.md. Files maps each path under the skill root to its
// content, and the entry lists the size and SHA-256 digest of each. Served replaces the content that
// resources/read returns for a path, and Listed replaces the content that the entry of skills/list describes,
// so a scenario can script content that does not match its entry. Frontmatter replaces the frontmatter of every
// entry. Blobs are binary files, served as blobs. An Unlisted skill is absent from skills/list and skills/get still answers it. A Dynamic skill lists no
// files.
type MCPSkill struct {
	Path        string
	Files       map[string]string
	Blobs       map[string][]byte `json:",omitempty"`
	Served      map[string]string `json:",omitempty"`
	Listed      map[string]string `json:",omitempty"`
	Frontmatter map[string]any    `json:",omitempty"`
	Unlisted    bool              `json:",omitempty"`
	Dynamic     bool              `json:",omitempty"`
}

func (s MCPSkill) root() string { return "skill://" + s.Path }

func (s MCPSkill) entry(files map[string]string) mcp.Skill {
	e := mcp.Skill{URI: s.root() + "/" + skill.Filename, Dynamic: s.Dynamic}
	if head, _, err := skill.SplitFrontmatter(files[skill.Filename]); err == nil {
		_ = yaml.Unmarshal([]byte(head+"\n"), &e.Frontmatter)
	}
	if s.Frontmatter != nil {
		e.Frontmatter = s.Frontmatter
	}
	if s.Dynamic {
		return e
	}
	all := map[string][]byte{}
	for p, text := range files {
		all[p] = []byte(text)
	}
	maps.Copy(all, s.Blobs)
	for _, p := range slices.Sorted(maps.Keys(all)) {
		sum := sha256.Sum256(all[p])
		e.Resources = append(e.Resources, mcp.SkillResource{URI: s.root() + "/" + p, Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(all[p]))})
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

func (h *mcpHandler) skillResources() []mcp.Resource {
	var out []mcp.Resource
	for _, s := range h.spec.Skills {
		for _, f := range slices.Sorted(maps.Keys(s.Files)) {
			out = append(out, mcp.Resource{URI: s.root() + "/" + f, Name: path.Base(f)})
		}
		for _, f := range slices.Sorted(maps.Keys(s.Blobs)) {
			out = append(out, mcp.Resource{URI: s.root() + "/" + f, Name: path.Base(f)})
		}
	}
	return out
}

func (h *mcpHandler) skillFile(uri string) (mcp.ResourceContents, bool) {
	for _, s := range h.spec.Skills {
		for f, text := range cmpFiles(s.Served, s.Files) {
			if s.root()+"/"+f == uri {
				return mcp.ResourceContents{URI: uri, Text: text}, true
			}
		}
		for f, b := range s.Blobs {
			if s.root()+"/"+f == uri {
				blob := base64.StdEncoding.EncodeToString(b)
				return mcp.ResourceContents{URI: uri, MimeType: "image/png", Blob: &blob}, true
			}
		}
	}
	return mcp.ResourceContents{}, false
}
