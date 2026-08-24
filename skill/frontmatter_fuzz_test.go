package skill

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseFrontmatter fuzzes the hand-rolled SKILL.md frontmatter parser
// (splitFrontmatter + parseFrontmatter in frontmatter.go) with arbitrary
// bytes. The parser is hand-rolled precisely because it avoids a YAML
// dependency (see the package doc), and it has already needed one production
// fix for a rejected-but-valid construct (block scalars, 55429b0) — direct
// evidence the hand-rolled grammar is worth fuzzing.
//
// Beyond splitFrontmatter/parseFrontmatter, successfully parsed frontmatter
// is also run through the same Skill-construction and validate() steps Load
// uses, so the fuzz target also exercises the field-level contract documented
// on the Skill struct and enforced by validate(). The directory-match check
// inside validate() is about filesystem placement, not about the bytes of
// SKILL.md, so a synthetic directory whose base name always equals the parsed
// name is used to isolate the content invariants from that unrelated check.
func FuzzParseFrontmatter(f *testing.F) {
	// Happy path, lifted from the existing minimal fixture (skill_test.go).
	f.Add([]byte(minimalBody))
	// Happy path with every optional field populated, including a metadata
	// block (skill_test.go's TestLoadValidFull fixture).
	f.Add([]byte(`---
name: full-skill
description: "A full skill, quoted."
license: MIT
compatibility: harness >= 1.0
allowed-tools: bash read_file  write_file
metadata:
  author: alice
  version: "2"
---
# Full

Everything.
`))
	// Block scalars: literal, folded, strip chomping, keep chomping, and a
	// block value immediately followed by another key. Values mirror
	// TestLoadBlockScalarDescription's cases plus a "+" (keep) case that test
	// doesn't cover.
	f.Add([]byte("---\nname: my-skill\ndescription: |\n  First line.\n  Second line.\n---\nbody\n"))
	f.Add([]byte("---\nname: my-skill\ndescription: >\n  First part\n  second part.\n---\nbody\n"))
	f.Add([]byte("---\nname: my-skill\ndescription: |-\n  Only line.\n---\nbody\n"))
	f.Add([]byte("---\nname: my-skill\ndescription: |+\n  Only line.\n\n---\nbody\n"))
	f.Add([]byte("---\nname: my-skill\ndescription: |\n  Line one.\n  Line two.\nlicense: MIT\n---\nbody\n"))
	// Missing frontmatter: no leading "---" delimiter.
	f.Add([]byte("# No frontmatter here\n\nJust markdown.\n"))
	// Unterminated frontmatter: opening delimiter, no closing one.
	f.Add([]byte("---\nname: my-skill\ndescription: ok\nbody without closing delimiter\n"))
	// Empty input.
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		doc := string(data)

		fm, _, err := splitFrontmatter(doc)
		if err != nil {
			return // malformed delimiters: a valid terminal outcome.
		}
		fields, meta, err := parseFrontmatter(fm)
		if err != nil {
			return // malformed frontmatter body: a valid terminal outcome.
		}
		checkFrontmatterInvariants(t, fields, meta)

		s := &Skill{
			Name:          fields["name"],
			Description:   fields["description"],
			License:       fields["license"],
			Compatibility: fields["compatibility"],
			Metadata:      meta,
			Dir:           filepath.Join("fuzz-root", fields["name"]),
		}
		if raw, ok := fields["allowed-tools"]; ok {
			s.AllowedTools = strings.Fields(raw)
		}
		if err := s.validate(); err != nil {
			return // spec violation: a valid terminal outcome.
		}
		checkSkillInvariants(t, s)
	})
}

// checkFrontmatterInvariants asserts the structural guarantees
// parseFrontmatter's doc comments make about any successfully parsed result,
// independent of the field-level rules validate() enforces later.
func checkFrontmatterInvariants(t *testing.T, fields, meta map[string]string) {
	t.Helper()
	for k, v := range fields {
		// "Unknown top-level keys are rejected with an error" (package doc):
		// every surviving key must be one the spec defines, and "metadata" is
		// always consumed into meta rather than left in fields.
		if k == "metadata" {
			t.Fatalf("fields contains \"metadata\" key, which must be routed to the metadata map: %v", fields)
		}
		if !knownKeys[k] {
			t.Fatalf("fields contains unknown key %q that should have been rejected: %v", k, fields)
		}
		// parseBlockScalar's doc: "trailing blank lines are dropped". A plain
		// scalar is a single physical line and so cannot contain '\n' at all.
		// Either way, no field value should end in a newline.
		if strings.HasSuffix(v, "\n") {
			t.Fatalf("field %q = %q has a trailing newline; block scalars must drop trailing blank lines", k, v)
		}
	}
	// parseMetadataBlock's doc/error path: "metadata block is empty" is
	// rejected, so a non-nil meta map is never empty.
	if meta != nil && len(meta) == 0 {
		t.Fatalf("meta is non-nil but empty; an empty metadata block should have been rejected")
	}
	for k, v := range meta {
		if strings.HasSuffix(v, "\n") {
			t.Fatalf("metadata %q = %q has a trailing newline", k, v)
		}
	}
}

// checkSkillInvariants asserts the field-level contract the Skill struct's
// doc comments and validate() document for anything validate() accepts:
// required fields are non-empty and within their documented bounds.
func checkSkillInvariants(t *testing.T, s *Skill) {
	t.Helper()
	// Skill.Name doc: "1-64 chars of lowercase a-z, 0-9 and hyphens, not
	// starting or ending with a hyphen, with no consecutive hyphens".
	if s.Name == "" {
		t.Fatal("validate() accepted an empty Name")
	}
	if err := validateName(s.Name); err != nil {
		t.Fatalf("validate() accepted Name %q that violates its own character/shape rules: %v", s.Name, err)
	}
	if got := filepath.Base(s.Dir); got != s.Name {
		t.Fatalf("validate() accepted Name %q not matching directory base %q", s.Name, got)
	}
	// Skill.Description doc: "the required 1-1024 char human-readable
	// summary".
	if s.Description == "" {
		t.Fatal("validate() accepted an empty Description")
	}
	if n := utf8.RuneCountInString(s.Description); n > maxDescriptionLen {
		t.Fatalf("validate() accepted an over-length Description: %d runes (max %d)", n, maxDescriptionLen)
	}
	// Skill.Compatibility doc: "optional 500 char compatibility constraint".
	if s.Compatibility != "" {
		if n := utf8.RuneCountInString(s.Compatibility); n > maxCompatibilityLen {
			t.Fatalf("validate() accepted an over-length Compatibility: %d runes (max %d)", n, maxCompatibilityLen)
		}
	}
}
