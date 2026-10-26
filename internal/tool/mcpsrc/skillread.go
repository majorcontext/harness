package mcpsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/majorcontext/harness/internal/mcp"
	"github.com/majorcontext/harness/internal/turn"
	"github.com/majorcontext/harness/protocol"
)

const dirMimeType = "inode/directory"

// skillRead reads uri when it is a file or a directory of a skill that the session loaded or that server lists.
// handled is false for any other URI, which reads as an ordinary resource. A file of a skill that the session
// loaded from another server is refused, so a skill never makes a read from a server other than its own.
func (s *Source) skillRead(ctx context.Context, c *mcp.Client, server, uri string) (res protocol.ToolResult, handled bool, err error) {
	byServer := s.held.containing(turn.SessionID(ctx), uri)
	e, isHeld := byServer[server]
	if !isHeld {
		for _, other := range slices.Sorted(maps.Keys(byServer)) {
			if _, ok := ownSkill(s.view().live[server], uri); !ok {
				return protocol.ToolResult{}, true, fmt.Errorf("%s is a file of the skill %s that the server %q serves; read it from %q",
					uri, byServer[other].URI, other, other)
			}
		}
		var ok bool
		if e, ok = ownSkill(s.view().live[server], uri); !ok {
			return protocol.ToolResult{}, false, nil
		}
	}
	root, _ := skillRoot(e.URI)
	if dir, ok := directory(e, root, uri); ok {
		return s.listDirectory(ctx, c, server, e, uri, dir)
	}
	listed := e.Dynamic || slices.ContainsFunc(e.Resources, func(r mcp.SkillResource) bool { return r.URI == uri })
	switch {
	case !listed && !isHeld:
		return protocol.ToolResult{}, false, nil
	case !listed:
		err = fmt.Errorf("%w: %s is not in the entry of the skill %s that this session loaded", errVerify, uri, e.URI)
	default:
		var b []byte
		if b, err = readBytes(ctx, c, server, uri); err == nil {
			err = verify(e, uri, b)
		}
		if err == nil {
			return protocol.ToolResult{Text: string(b)}, true, nil
		}
	}
	return protocol.ToolResult{}, true, fmt.Errorf("%w. Load the skill again with %s to read its current entry", err, loadName)
}

// ownSkill returns the listed skill of sv that holds uri, the one with the longest root.
func ownSkill(sv *server, uri string) (mcp.Skill, bool) {
	var best mcp.Skill
	if sv == nil {
		return best, false
	}
	for _, e := range sv.skills {
		if root, _ := skillRoot(e.URI); within(root, uri) && len(e.URI) > len(best.URI) {
			best = e
		}
	}
	return best, best.URI != ""
}

// directory returns the files and subdirectories directly under uri, from the entry, when uri is the root of the
// skill or a directory in it. A dynamic entry knows only its root.
func directory(e mcp.Skill, root, uri string) ([]mcp.Resource, bool) {
	if e.Dynamic {
		return nil, uri == root
	}
	children := map[string]mcp.Resource{}
	for _, r := range e.Resources {
		rel, ok := strings.CutPrefix(r.URI, uri+"/")
		if !ok {
			continue
		}
		if dir, _, nested := strings.Cut(rel, "/"); nested {
			children[dir] = mcp.Resource{URI: uri + "/" + dir, Name: dir, MimeType: dirMimeType}
		} else {
			children[rel] = mcp.Resource{URI: r.URI, Name: path.Base(r.URI)}
		}
	}
	if len(children) == 0 {
		return nil, false
	}
	out := make([]mcp.Resource, 0, len(children))
	for _, k := range slices.Sorted(maps.Keys(children)) {
		out = append(out, children[k])
	}
	return out, true
}

// listDirectory answers a read of a skill directory: from the entry, or for a dynamic skill from
// resources/directory/read when the server declared it.
func (s *Source) listDirectory(ctx context.Context, c *mcp.Client, server string, e mcp.Skill, uri string, children []mcp.Resource) (protocol.ToolResult, bool, error) {
	if e.Dynamic {
		if _, dirRead := c.ServerCapabilities().SkillsSupport(); !dirRead {
			return protocol.ToolResult{}, true, fmt.Errorf("%s is the directory of a skill, and the server %q does not list directories", uri, server)
		}
		seen := map[string]bool{}
		for cursor := ""; ; {
			page, err := c.ReadDirectory(ctx, uri, cursor)
			if err != nil {
				return protocol.ToolResult{}, true, hide(server, err)
			}
			children = append(children, page.Resources...)
			if page.NextCursor == "" || seen[page.NextCursor] || len(children) > maxSkillFiles {
				break
			}
			seen[page.NextCursor], cursor = true, page.NextCursor
		}
	}
	b, err := json.Marshal(struct {
		Directory string         `json:"directory"`
		Resources []mcp.Resource `json:"resources"`
	}{uri, children})
	return protocol.ToolResult{Text: string(b)}, true, err
}
