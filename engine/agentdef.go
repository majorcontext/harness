package engine

import (
	"fmt"
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

// ResolveAgentDefs returns every agent type available to a session rooted
// at workDir: the compiled-in builtinAgentDefs plus whatever
// LoadAgentDefs(workDir/.agents) discovers, merged. It is the single entry
// point Stage 3's `task` tool (and Stage 4's session.create) resolve an
// agent name against.
func ResolveAgentDefs(workDir string) (map[string]AgentDef, error) {
	defs := make(map[string]AgentDef, len(builtinAgentDefs))
	for name, def := range builtinAgentDefs {
		defs[name] = def
	}
	custom, err := LoadAgentDefs(agentDefsDir(workDir))
	if err != nil {
		return nil, err
	}
	for name, def := range custom {
		defs[name] = def
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
		s.agentDefs, s.agentDefsErr = ResolveAgentDefs(s.cfg.WorkDir)
	}
	return s.agentDefs, s.agentDefsErr
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
// Every *.md file must parse and every tools: entry must name a real tool
// (knownToolNames) — a definition that fails either check fails discovery
// for the WHOLE directory, naming the offending file, per the design doc's
// "unknown tool names in a definition are an error surfaced at load, not
// spawn." A custom definition's name may not collide with a built-in
// (general-purpose, explore, plan) or with another custom definition in
// the same directory — both are load errors too.
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
			return AgentDef{}, fmt.Errorf("unknown frontmatter key %q", key)
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
