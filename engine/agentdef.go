package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/majorcontext/harness/message"
)

// AgentDef describes one agent type a spawn (the `task` tool, Stage 3, or
// session.create, Stage 4) can name: which tools the child gets, an
// optional model override, and a system-prompt addition. Three are
// compiled in — general-purpose, explore, plan, see builtinAgentDefs and
// the design doc's preset table — and more load from a project's
// .agents/*.md files (see LoadAgentDefs).
type AgentDef struct {
	Name        string
	Description string
	// Tools, if non-nil, restricts the child's tool registry to exactly
	// these names (via restrictTools, session_manager.go) — nil inherits
	// the caller's full tool set unchanged. A definition whose Tools list
	// omits "task" is a leaf: it cannot itself spawn children, regardless
	// of depth (the design doc's "any agent definition can exclude task to
	// make its type a leaf").
	Tools []string
	// Model overrides the child's model. The zero value inherits the
	// caller's.
	Model message.ModelRef
	// SystemAppend is appended as one more Config.System segment: a
	// .agents/*.md file's body verbatim, or a synthesized instruction for
	// a built-in like plan. Empty means no addition — the child's system
	// prompt is exactly its parent's (fx-style: no distinct persona).
	SystemAppend string
	// Source identifies where this definition came from: "builtin", or the
	// absolute path of the .agents/*.md file it was loaded from. Purely
	// diagnostic (session_info, load-error messages).
	Source string
}

// Built-in agent type names, matching the design doc's preset table.
const (
	AgentGeneralPurpose = "general-purpose"
	AgentExplore        = "explore"
	AgentPlan           = "plan"
)

// readOnlyTools is the tool set explore and plan restrict to: read-only
// inspection, deliberately excluding bash (which can write files and run
// arbitrary commands) and write_file/edit_file. This is the design doc's
// illustrative read_file/glob/grep/ls mapped onto the real registered tool
// names — identical here, since glob/grep/ls (searchtools.go) were added
// specifically to give this preset real teeth; session_info is included
// too, since every other preset gets it and self-inspection is harmless.
// "task" is deliberately absent: both presets are leaves.
var readOnlyTools = []string{"read_file", "glob", "grep", "ls", "session_info"}

// builtinAgentDefs are the compiled-in presets. Looked up by ResolveAgentDefs
// and never mutated after package init.
var builtinAgentDefs = map[string]AgentDef{
	AgentGeneralPurpose: {
		Name:        AgentGeneralPurpose,
		Description: "General-purpose agent for complex, multi-step, or open-ended tasks. Has the full tool set, including task itself, so it can spawn its own children (subject to the depth limit).",
		Tools:       nil, // full set, unrestricted
		Source:      "builtin",
	},
	AgentExplore: {
		Name:        AgentExplore,
		Description: "Fast, read-only agent for finding code and answering \"where is X\" questions. Cannot edit files or run commands.",
		Tools:       readOnlyTools,
		Source:      "builtin",
	},
	AgentPlan: {
		Name:        AgentPlan,
		Description: "Read-only agent that investigates a codebase and returns an implementation plan instead of making edits.",
		Tools:       readOnlyTools,
		SystemAppend: "You are in planning mode. Investigate using your read-only tools, then reply with a clear, concrete implementation plan " +
			"as your final answer. You have no tools that can edit a file or run a command that changes state — do not attempt to make any changes.",
		Source: "builtin",
	},
}

// knownToolNames are every tool name a *.md agent definition's tools: list
// may reference — every built-in this package can ever register,
// including the conditionally-registered ones (process, goal, model, mcp,
// read_tool_result: present only when the relevant Config field is set)
// and task (Stage 3, gated on depth rather than Config). Validating
// against this fixed set, rather than one particular session's actually-
// registered tools, is what lets a definition be loaded once per WorkDir
// and reused by every session rooted there (see LoadAgentDefs) — a
// definition referencing "mcp" is not a load-time error just because the
// FIRST session to load it happens to have no MCP servers configured.
//
// A definition cannot reference a tool an embedder registers via
// Config.Tools (a name this package cannot know about statically): that is
// a known limitation of load-time validation, not a spawn-time one.
var knownToolNames = map[string]bool{
	"bash": true, "read_file": true, "write_file": true, "edit_file": true,
	"session_info": true, "glob": true, "grep": true, "ls": true,
	"process": true, "goal": true, "model": true, "mcp": true,
	"read_tool_result": true, "task": true,
}

// ResolveAgentDefs returns every agent type available to a session: the
// compiled-in builtinAgentDefs plus whatever LoadAgentDefs discovers
// across every directory in dirs, merged. It is the single entry point
// Stage 3's `task` tool (and Stage 4's session.create) resolve an agent
// name against.
//
// A custom definition's name may not collide with a built-in, nor with
// ANOTHER custom definition — whether that collision is within one dir
// (LoadAgentDefs' own check) or across two different dirs in dirs (this
// function's own check, mirroring buildSkillsSegment's identical
// duplicate-across-dirs handling for Agent Skills — skills.go). Neither
// case has an obvious "which one wins" answer, unlike a single
// unparseable file (see LoadAgentDefs' own "frontmatter leniency" doc
// comment) — both stay hard load errors.
func ResolveAgentDefs(dirs []string) (map[string]AgentDef, error) {
	defs := make(map[string]AgentDef, len(builtinAgentDefs))
	source := make(map[string]string, len(builtinAgentDefs))
	for name, def := range builtinAgentDefs {
		defs[name] = def
		source[name] = "builtin"
	}
	// Deduped on filepath.Clean(dir) before ever calling LoadAgentDefs —
	// a live review finding: without this, the SAME directory appearing
	// twice in dirs (Config.AgentDefsDirs built up from more than one
	// source, or simply a caller-supplied duplicate) got loaded twice,
	// and every single name it defined then collided with ITSELF on the
	// second pass — the cross-dir duplicate-name check just below exists
	// to catch a genuine conflict between two DIFFERENT directories, not
	// a directory tripping over its own earlier pass, and a false
	// positive here is a hard load error that kills every custom agent
	// type for the whole session, not a harmless no-op. Clean, not a raw
	// string compare, so the common trivial variants (a trailing
	// slash, a redundant "./") still dedupe; deliberately NOT
	// symlink/absolute-path resolution (filepath.Abs or EvalSymlinks) —
	// dirs may legitimately not exist yet (LoadAgentDefs' own "missing
	// dir is not an error" contract), and erroring or doing I/O here
	// just to normalize a path this function does not otherwise need
	// resolved would trade one edge case for a worse one.
	seen := make(map[string]bool, len(dirs))
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		custom, err := LoadAgentDefs(dir)
		if err != nil {
			return nil, err
		}
		for name, def := range custom {
			if prevSource, dup := source[name]; dup {
				return nil, fmt.Errorf("engine: agent definition %s: name %q already defined in %s", def.Source, name, prevSource)
			}
			defs[name] = def
			source[name] = def.Source
		}
	}
	return defs, nil
}

// AgentDefs returns s's available agent definitions — built-ins plus its
// WorkDir's .agents/*.md — discovering them exactly once and caching the
// result (or a load failure) for the session's life. Mirrors
// ensureInstructions/ensureSkills' load-once-cache-error pattern
// (instructions.go, skills.go), but triggered lazily from the FIRST call
// here (the `task` tool's own first invocation, or a wire-level
// session.create with a parent — see task_tool.go and
// server/session_tree.go's handleSpawnChild) rather than from Prompt: a
// malformed .agents/*.md should only ever break SPAWNING a child, never
// every prompt in a session that never uses `task` at all.
//
// This is also what makes the design doc's "unknown tool names in a
// definition are an error surfaced at load, not spawn" true in practice —
// "load" here means the session's first task-shaped call, not
// construction: caching here is what stops a definition being re-read and
// re-parsed from disk on every single spawn, which is what an earlier,
// uncached version of this call site did (a live review finding: a parent
// fanning out many children re-parsed the same .agents/*.md files on every
// one).
func (s *Session) AgentDefs() (map[string]AgentDef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.agentDefsLoaded {
		s.agentDefsLoaded = true
		s.agentDefs, s.agentDefsErr = ResolveAgentDefs(s.agentDefsDirs())
	}
	return s.agentDefs, s.agentDefsErr
}

// agentDefsDirs returns the effective agent-definition directories for
// the session, resolving Config.AgentDefsDirs' nil/empty/multi-dir
// contract — see that field's own doc comment. Mirrors skillsDirs'
// identical nil-means-default resolution exactly, one directory
// (agentDefsDir(WorkDir)) instead of skillsDirs' conditional-on-existence
// one: unlike skills, a MISSING .agents dir here is not itself
// special-cased — LoadAgentDefs already treats os.IsNotExist as "no
// custom definitions" (nil, nil), so there is no need to pre-check
// isDir before including it, unlike skillsDirs' defaultSkillsSubdir
// check (which exists to avoid scanning a directory Agent Skills has no
// convention for auto-creating). Caller holds s.mu (AgentDefs' own
// caller already does).
func (s *Session) agentDefsDirs() []string {
	if s.cfg.AgentDefsDirs != nil {
		return s.cfg.AgentDefsDirs
	}
	return []string{agentDefsDir(s.cfg.WorkDir)}
}

// agentDefsDir is where LoadAgentDefs looks for *.md agent definitions —
// the top level of workDir's .agents directory. Its skills/ subdirectory
// (Agent Skills, see skills.go's defaultSkillsSubdir) is never descended
// into or treated as a definition: LoadAgentDefs only reads files directly
// in this directory, never recursing.
func agentDefsDir(workDir string) string {
	return filepath.Join(workDir, ".agents")
}

// LoadAgentDefs discovers custom agent definitions from dir: loose
// top-level *.md files only (a subdirectory — .agents/skills/ in
// particular — is never descended into). A missing dir is not an error:
// (nil, nil), mirroring skill.Discover's convention for a project with no
// custom definitions at all.
//
// errUnknownFrontmatterKey is the ONE parseAgentDef error class
// LoadAgentDefs treats leniently — skip this one file, log a warning,
// keep loading the rest of the directory. A design-owner decision on a
// live review finding ("frontmatter leniency" scope): an earlier
// revision of this package skipped-and-warned on EVERY parseAgentDef
// error alike (bad frontmatter delimiters, a missing required field, an
// unknown tool name, an invalid model string), reasoning that one
// contributor's single mistake in one file should never break every
// OTHER custom agent type in the same directory. A later review
// disagreed for two specific classes: an unknown tool name and an
// invalid model string are SEMANTIC authoring mistakes the design doc
// explicitly requires to be "an error surfaced at load, not spawn" — a
// silently-skipped file means the agent simply does not exist, and a
// later `task` call naming it fails with a generic "unknown agent %q"
// that gives no hint the definition was ever written, let alone why it
// was rejected. An unknown FRONTMATTER KEY (a stray typo'd line, like
// `desc:` instead of `description:`) is judged a lower-stakes,
// genuinely cosmetic mistake worth the same one-file-only blast radius
// the original leniency fix targeted — every OTHER parseAgentDef error
// (structural frontmatter problems, missing required fields, duplicate
// keys, unknown tool names, invalid models) is a hard load error for the
// WHOLE directory once again, matching the design doc's original,
// pre-leniency-fix behavior for those classes.
var errUnknownFrontmatterKey = errors.New("unknown frontmatter key")

// LoadAgentDefs discovers custom agent definitions from dir: loose
// top-level *.md files only (a subdirectory — .agents/skills/ in
// particular — is never descended into). A missing dir is not an error:
// (nil, nil), mirroring skill.Discover's convention for a project with no
// custom definitions at all.
//
// Exactly ONE class of per-file error is lenient: an unknown frontmatter
// KEY (errUnknownFrontmatterKey — see its own doc comment for the full
// design-owner reasoning) skips just that one file with a logged
// warning, not a load error for the whole directory. Every OTHER
// per-file error — a bad frontmatter delimiter, a malformed "key: value"
// line, a missing required field, a duplicate key, an unknown TOOL name,
// an invalid MODEL string, or a bare os.ReadFile failure — fails the
// WHOLE directory's load (`return nil, err`), exactly as the design
// doc's "unknown tool names in a definition are an error surfaced at
// load, not spawn" already requires for those two specifically, and as
// this whole function did for every error class before the (now
// narrowed) leniency fix existed at all. Because AgentDefs (this
// package's sole caller) caches a load failure for the session's whole
// life, this DOES mean one contributor's unknown-tool typo in agent-b.md
// costs every OTHER custom agent type too, not just agent-b — a
// deliberate trade-off: silently letting agent-b simply not exist would
// bury a real authoring mistake behind a generic "unknown agent %q" at
// spawn time instead of surfacing it, loudly, at load.
//
// This same hard-fail treatment already covered cross-file conflicts — a
// custom definition's name colliding with a built-in (general-purpose,
// explore, plan), or with ANOTHER custom definition in the same
// directory. Neither has an obvious "which one wins" answer the way an
// unknown-key typo does — there is nothing to silently prefer between two
// genuinely different definitions both claiming the same name.
func LoadAgentDefs(dir string) (map[string]AgentDef, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("engine: reading agent definitions in %s: %w", dir, err)
	}
	defs := make(map[string]AgentDef)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("engine: reading agent definition %s: %w", path, err)
		}
		def, err := parseAgentDef(string(data), path)
		if err != nil {
			if errors.Is(err, errUnknownFrontmatterKey) {
				slog.Warn("engine: skipping agent definition with an unknown frontmatter key", "path", path, "error", err)
				continue
			}
			return nil, fmt.Errorf("engine: agent definition %s: %w", path, err)
		}
		if _, ok := builtinAgentDefs[def.Name]; ok {
			return nil, fmt.Errorf("engine: agent definition %s: name %q collides with a built-in agent type", path, def.Name)
		}
		if existing, dup := defs[def.Name]; dup {
			return nil, fmt.Errorf("engine: agent definition %s: name %q already defined in %s", path, def.Name, existing.Source)
		}
		defs[def.Name] = def
	}
	return defs, nil
}

// agentDefKnownKeys are the only frontmatter keys an agent definition may
// use — Claude Code-compatible: name, description, tools, model.
var agentDefKnownKeys = map[string]bool{"name": true, "description": true, "tools": true, "model": true}

// parseAgentDef parses one *.md agent definition: a "---"-fenced
// frontmatter block of flat "key: value" lines (name, description, tools,
// model — no nested structure, no block scalars; this is a deliberately
// narrower subset than skill/frontmatter.go's, matched to this format's
// simpler four-key shape) followed by a Markdown body, which becomes
// SystemAppend verbatim (trimmed of surrounding blank lines).
func parseAgentDef(doc, path string) (AgentDef, error) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	lines := strings.Split(doc, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t") != "---" {
		return AgentDef{}, fmt.Errorf("must begin with a '---' frontmatter delimiter line")
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t") == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 {
		return AgentDef{}, fmt.Errorf("unterminated frontmatter: no closing '---' delimiter")
	}

	fields := make(map[string]string)
	// unknownKeyErr is recorded, not returned immediately, the moment an
	// unknown key is seen — see its own use below (after every other
	// check in this function has had a chance to run) for why: an
	// earlier version of this loop returned this error the INSTANT it
	// hit an unknown key, which meant a file with BOTH a stray unknown
	// key AND a genuine semantic mistake (an unknown tool name, an
	// invalid model string — validated below, only once the full fields
	// map is assembled) reported only the lenient one, whichever line
	// happened to come first in the file — silently hiding the hard
	// error LoadAgentDefs actually needed to fail the whole directory
	// load for. A live review caught this: leniency is for unknown KEYS
	// only, and must never suppress the REPORT of a co-occurring semantic
	// mistake, even if it does still win when it is the ONLY problem the
	// file has (see the final check at the bottom of this function).
	var unknownKeyErr error
	for _, line := range lines[1:closeIdx] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return AgentDef{}, fmt.Errorf("malformed frontmatter line (expected 'key: value'): %q", trimmed)
		}
		key = strings.TrimSpace(key)
		value = unquoteAgentDefValue(strings.TrimSpace(value))
		if !agentDefKnownKeys[key] {
			if unknownKeyErr == nil {
				unknownKeyErr = fmt.Errorf("%w: %q", errUnknownFrontmatterKey, key)
			}
			continue
		}
		if _, dup := fields[key]; dup {
			return AgentDef{}, fmt.Errorf("duplicate frontmatter key %q", key)
		}
		fields[key] = value
	}
	body := strings.TrimSpace(strings.Join(lines[closeIdx+1:], "\n"))

	name := fields["name"]
	if name == "" {
		return AgentDef{}, fmt.Errorf("frontmatter missing required 'name'")
	}
	description := fields["description"]
	if description == "" {
		return AgentDef{}, fmt.Errorf("frontmatter missing required 'description'")
	}

	def := AgentDef{
		Name:         name,
		Description:  description,
		SystemAppend: body,
		Source:       path,
	}
	if raw, ok := fields["tools"]; ok {
		var names []string
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !knownToolNames[part] {
				return AgentDef{}, fmt.Errorf("unknown tool %q in tools:", part)
			}
			names = append(names, part)
		}
		if len(names) == 0 {
			return AgentDef{}, fmt.Errorf("tools: present but empty")
		}
		def.Tools = names
	}
	if raw, ok := fields["model"]; ok {
		ref, err := message.ParseModelRef(raw)
		if err != nil {
			return AgentDef{}, fmt.Errorf("invalid model %q: %w", raw, err)
		}
		def.Model = ref
	}
	// Checked LAST, only once every hard-error class above (missing
	// name/description, unknown tool, invalid model) has had its chance
	// to fire — see unknownKeyErr's own comment above for why: this is
	// what actually makes leniency lose to a co-occurring semantic
	// mistake instead of silently masking it, while still winning (as
	// before) when it is the file's only problem.
	if unknownKeyErr != nil {
		return AgentDef{}, unknownKeyErr
	}
	return def, nil
}

// unquoteAgentDefValue strips one matching pair of surrounding double or
// single quotes. No escape processing — the minimal handling this
// format's scalar values need, mirroring skill/frontmatter.go's unquote.
func unquoteAgentDefValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
