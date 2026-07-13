// Package skill parses and validates Agent Skills as defined by the Agent
// Skills specification (agentskills.io/specification). A skill is a directory
// containing a SKILL.md file: YAML frontmatter followed by a Markdown body.
//
// This package covers parsing and validation only — there is no engine
// integration here. It is built for the specification's progressive-disclosure
// contract:
//
//   - Stage 1 (cheap metadata scan): Load reads only the frontmatter and
//     validates it. The Markdown body is not retained.
//   - Stage 2 (full load): Skill.Instructions reads and returns the body on
//     demand.
//
// # Frontmatter parser
//
// Skills are specified with YAML frontmatter, but this package deliberately
// avoids a YAML dependency (zero new dependencies) and hand-rolls a minimal,
// line-based parser covering exactly the constructs the spec uses:
//
//   - Scalar string values, either plain (my-skill) or double/single quoted
//     ("value" / 'value'). Quotes are stripped; no escape processing beyond
//     the outer quote pair.
//   - A single one-level-deep "metadata:" mapping block whose entries are
//     indented "key: value" scalar pairs.
//   - Literal (|) and folded (>) block scalars for a multi-line scalar value,
//     with an optional -/+ chomping indicator. The following more-indented
//     lines are dedented and joined (newlines for |, spaces for >), ending at
//     the next non-indented key. Explicit indentation indicators (e.g. |2) are
//     out of scope, and + (keep) is accepted for validity but normalized to
//     clip (trailing blank lines are always dropped).
//   - Unknown top-level keys are rejected with an error (spec-first
//     strictness: only fields named by the specification are permitted).
//
// Deliberately unsupported YAML constructs (any use is an error or is treated
// as a plain string, never interpreted): flow collections ([a, b] / {k: v}),
// anchors/aliases, tags, nested mappings deeper than metadata's one level, and
// sequence (- item) syntax. allowed-tools is a single space-separated scalar
// string per the spec, not a YAML sequence.
package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Filename is the fixed name of the skill definition file within a skill
// directory.
const Filename = "SKILL.md"

// Field length limits from the specification.
const (
	maxNameLen          = 64
	maxDescriptionLen   = 1024
	maxCompatibilityLen = 500
)

// Skill is the validated metadata for a single Agent Skill. Load populates
// every field except the Markdown body, which is fetched separately via
// Instructions (stage-2 progressive disclosure).
type Skill struct {
	// Name is the skill identifier: 1-64 chars of lowercase a-z, 0-9 and
	// hyphens, not starting or ending with a hyphen, with no consecutive
	// hyphens, and matching the parent directory name.
	Name string
	// Description is the required 1-1024 char human-readable summary.
	Description string
	// License is the optional SPDX-style license string.
	License string
	// Compatibility is the optional 1-500 char compatibility constraint.
	Compatibility string
	// Metadata is the optional one-level-deep map of string keys to string
	// values. It is nil when the frontmatter has no metadata block.
	Metadata map[string]string
	// AllowedTools is the optional (experimental) list of tool names, parsed
	// by splitting the space-separated "allowed-tools" scalar. It is nil when
	// the field is absent.
	AllowedTools []string
	// Dir is the skill directory that was loaded.
	Dir string
	// Path is the full path to the SKILL.md file within Dir.
	Path string
}

// Load parses and validates the SKILL.md in dir. Every specification violation
// is returned as an error naming the SKILL.md file and the exact violation.
// The Markdown body is not read or retained; use Instructions for that.
func Load(dir string) (*Skill, error) {
	path := filepath.Join(dir, Filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, _, err := splitFrontmatter(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	fields, meta, err := parseFrontmatter(fm)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	s := &Skill{
		Name:          fields["name"],
		Description:   fields["description"],
		License:       fields["license"],
		Compatibility: fields["compatibility"],
		Metadata:      meta,
		Dir:           dir,
		Path:          path,
	}
	if raw, ok := fields["allowed-tools"]; ok {
		s.AllowedTools = strings.Fields(raw)
	}

	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// validate enforces the specification's field rules. The caller wraps returned
// errors with the SKILL.md path.
func (s *Skill) validate() error {
	if s.Name == "" {
		return errors.New("missing required field: name")
	}
	if err := validateName(s.Name); err != nil {
		return err
	}
	if want := filepath.Base(s.Dir); s.Name != want {
		return fmt.Errorf("name %q must match parent directory name %q", s.Name, want)
	}

	if s.Description == "" {
		return errors.New("missing or empty required field: description")
	}
	if n := utf8.RuneCountInString(s.Description); n > maxDescriptionLen {
		return fmt.Errorf("description is %d chars, exceeds maximum of %d", n, maxDescriptionLen)
	}

	if s.Compatibility != "" {
		if n := utf8.RuneCountInString(s.Compatibility); n > maxCompatibilityLen {
			return fmt.Errorf("compatibility is %d chars, exceeds maximum of %d", n, maxCompatibilityLen)
		}
	}
	return nil
}

// validateName enforces the name character and shape rules.
func validateName(name string) error {
	if n := utf8.RuneCountInString(name); n < 1 || n > maxNameLen {
		return fmt.Errorf("name %q must be 1-%d characters", name, maxNameLen)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("name %q must not start or end with a hyphen", name)
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("name %q must not contain consecutive hyphens", name)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			return fmt.Errorf("name %q contains invalid character %q; only lowercase a-z, 0-9 and hyphens are allowed", name, r)
		}
	}
	return nil
}

// Instructions reads and returns the Markdown body of the skill (stage-2
// progressive disclosure). The frontmatter is stripped; the body is returned
// verbatim.
func (s *Skill) Instructions() (string, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return "", err
	}
	_, body, err := splitFrontmatter(string(data))
	if err != nil {
		return "", fmt.Errorf("%s: %w", s.Path, err)
	}
	return body, nil
}

// Discover loads every immediate subdirectory of root that contains a
// SKILL.md. Directories without a SKILL.md are skipped. A missing root
// returns an empty slice and no error. Any invalid skill fails the whole
// Discover with the error from Load (which names the offending SKILL.md).
// Results are sorted by name.
func Discover(root string) ([]*Skill, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var skills []*Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, Filename)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		s, err := Load(dir)
		if err != nil {
			return nil, err
		}
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}
