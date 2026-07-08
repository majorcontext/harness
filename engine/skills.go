// Agent Skills injection: skills discovered under the configured directories
// are advertised in the system prompt so the model knows they exist without
// the user naming them.
//
// This implements the agentskills.io progressive-disclosure model. Stage 1
// (this file) injects only cheap metadata — one line per skill (name,
// description, and the absolute path to its SKILL.md) plus a header telling
// the model that to *activate* a skill it MUST first read that SKILL.md with
// the read_file tool. Stage 2 (the actual instructions) is deferred to that
// read: the body is never front-loaded into the prompt.
//
// Discovery touches disk, so — exactly like project instructions (see
// instructions.go) — it happens lazily on the first Prompt of a session
// (never at NewSession, the startup budget rule) and is cached for the
// session's life. A discovery error (a malformed SKILL.md, or a duplicate
// skill name across dirs) fails that first Prompt loudly, mirroring the
// present-but-unusable AGENTS.md contract: a project that ships skills must
// not run silently without them.
//
// Skills are never written to the session log — the log stores only canonical
// messages — so a resumed session rediscovers them on its first Prompt.

package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/majorcontext/harness/skill"
)

// defaultSkillsSubdir is the directory (relative to WorkDir) scanned for
// skills when Config.SkillsDirs is nil and it exists.
var defaultSkillsSubdir = filepath.Join(".agents", "skills")

// skillsHeader introduces the stage-1 skill catalog and states the
// progressive-disclosure contract: the model must read a skill's SKILL.md
// with the read_file tool before relying on it.
const skillsHeader = "Available skills. Each skill below is a capability you can use, " +
	"but only its name and description are shown here. To activate a skill you " +
	"MUST first read its SKILL.md file with the read_file tool before relying " +
	"on it; do not assume its contents from the description alone."

// skillsDirs returns the effective skill directories for the session,
// resolving the nil-means-default behaviour: a nil SkillsDirs uses
// <WorkDir>/.agents/skills when that directory exists (an explicit empty
// slice disables discovery entirely). Caller holds s.mu.
func (s *Session) skillsDirs() []string {
	if s.cfg.SkillsDirs != nil {
		return s.cfg.SkillsDirs
	}
	def := filepath.Join(s.cfg.WorkDir, defaultSkillsSubdir)
	if isDir(def) {
		return []string{def}
	}
	return nil
}

// ensureSkills discovers and caches the skill segment on first call,
// returning any discovery error (a malformed SKILL.md or a duplicate name).
// Like ensureInstructions it runs at most once per session; the cached error,
// if any, is returned again on later calls.
func (s *Session) ensureSkills() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skillsLoaded {
		return s.skillsErr
	}
	s.skillsLoaded = true
	seg, err := s.buildSkillsSegment()
	if err != nil {
		s.skillsErr = err
		return err
	}
	s.skillsSeg = seg
	return nil
}

// buildSkillsSegment discovers skills across every configured dir, merges
// them (sorted by name), and formats the stage-1 catalog segment. A duplicate
// skill name across two dirs is an error naming both dirs. Returns "" when no
// skills are found or discovery is disabled. Caller holds s.mu.
func (s *Session) buildSkillsSegment() (string, error) {
	dirs := s.skillsDirs()
	if len(dirs) == 0 {
		return "", nil
	}

	type located struct {
		sk  *skill.Skill
		dir string
	}
	byName := make(map[string]located)
	var names []string
	for _, dir := range dirs {
		found, err := skill.Discover(dir)
		if err != nil {
			return "", err
		}
		for _, sk := range found {
			if prev, ok := byName[sk.Name]; ok {
				return "", fmt.Errorf("engine: duplicate skill %q found in %s and %s", sk.Name, prev.dir, dir)
			}
			byName[sk.Name] = located{sk: sk, dir: dir}
			names = append(names, sk.Name)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(skillsHeader)
	for _, name := range names {
		sk := byName[name].sk
		absPath, err := filepath.Abs(sk.Path)
		if err != nil {
			absPath = sk.Path
		}
		fmt.Fprintf(&b, "\n%s — %s (path: %s)", sk.Name, sk.Description, absPath)
		// Keep the structured catalog for the session_info tool.
		s.skills = append(s.skills, skillInfo{Name: sk.Name, Path: absPath})
	}
	return b.String(), nil
}

// skillsSegment returns the cached skill segment (possibly empty).
func (s *Session) skillsSegment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.skillsSeg
}
