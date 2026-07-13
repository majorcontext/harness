package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeSkill creates a directory named dir under root and writes body to its
// SKILL.md, returning the directory path.
func writeSkill(t *testing.T, root, dir, body string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return d
}

const minimalBody = `---
name: my-skill
description: A minimal skill.
---
# Heading

Body text.
`

func TestLoadValidMinimal(t *testing.T) {
	root := t.TempDir()
	d := writeSkill(t, root, "my-skill", minimalBody)

	s, err := Load(d)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "my-skill" {
		t.Errorf("Name = %q, want my-skill", s.Name)
	}
	if s.Description != "A minimal skill." {
		t.Errorf("Description = %q", s.Description)
	}
	if s.License != "" || s.Compatibility != "" {
		t.Errorf("expected empty optional fields, got license=%q compat=%q", s.License, s.Compatibility)
	}
	if s.Metadata != nil {
		t.Errorf("Metadata = %v, want nil", s.Metadata)
	}
	if s.AllowedTools != nil {
		t.Errorf("AllowedTools = %v, want nil", s.AllowedTools)
	}
	if s.Dir != d {
		t.Errorf("Dir = %q, want %q", s.Dir, d)
	}
	if want := filepath.Join(d, "SKILL.md"); s.Path != want {
		t.Errorf("Path = %q, want %q", s.Path, want)
	}
}

func TestLoadValidFull(t *testing.T) {
	root := t.TempDir()
	body := `---
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
`
	d := writeSkill(t, root, "full-skill", body)

	s, err := Load(d)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "full-skill" {
		t.Errorf("Name = %q", s.Name)
	}
	if s.Description != "A full skill, quoted." {
		t.Errorf("Description = %q", s.Description)
	}
	if s.License != "MIT" {
		t.Errorf("License = %q", s.License)
	}
	if s.Compatibility != "harness >= 1.0" {
		t.Errorf("Compatibility = %q", s.Compatibility)
	}
	wantTools := []string{"bash", "read_file", "write_file"}
	if !reflect.DeepEqual(s.AllowedTools, wantTools) {
		t.Errorf("AllowedTools = %v, want %v", s.AllowedTools, wantTools)
	}
	wantMeta := map[string]string{"author": "alice", "version": "2"}
	if !reflect.DeepEqual(s.Metadata, wantMeta) {
		t.Errorf("Metadata = %v, want %v", s.Metadata, wantMeta)
	}
}

// TestLoadBlockScalarDescription covers YAML block scalars for multi-line
// scalar values (description: |, description: >). These are valid YAML and
// commonly used for long descriptions; the parser must accept them rather
// than erroring on the indented continuation lines. Fixtures are synthetic.
func TestLoadBlockScalarDescription(t *testing.T) {
	cases := []struct {
		name string
		fm   string // frontmatter lines after "name: my-skill\n"
		want string
	}{
		{
			name: "literal block joins with newlines",
			fm:   "description: |\n  First line.\n  Second line.\n",
			want: "First line.\nSecond line.",
		},
		{
			name: "folded block joins with spaces",
			fm:   "description: >\n  First part\n  second part.\n",
			want: "First part second part.",
		},
		{
			name: "literal with strip chomping",
			fm:   "description: |-\n  Only line.\n",
			want: "Only line.",
		},
		{
			name: "block value before another key",
			fm:   "description: |\n  Line one.\n  Line two.\nlicense: MIT\n",
			want: "Line one.\nLine two.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := "---\nname: my-skill\n" + tc.fm + "---\nbody\n"
			d := writeSkill(t, root, "my-skill", body)
			s, err := Load(d)
			if err != nil {
				t.Fatalf("block-scalar description should parse, got: %v", err)
			}
			if s.Description != tc.want {
				t.Fatalf("Description = %q, want %q", s.Description, tc.want)
			}
		})
	}
}

func TestLoadNameRules(t *testing.T) {
	cases := []struct {
		name string // dir name (and, unless mismatch, frontmatter name)
		fm   string // frontmatter name value, if different from dir
		want string // substring the error must contain
	}{
		{name: "Uppercase-Name", fm: "Uppercase-Name", want: "name"},
		{name: "leadhyphen", fm: "-leading", want: "name"},
		{name: "trailhyphen", fm: "trailing-", want: "name"},
		{name: "double", fm: "double--hyphen", want: "name"},
		{name: strings.Repeat("a", 65), fm: strings.Repeat("a", 65), want: "name"},
		{name: "actual-dir", fm: "different-name", want: "match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := "---\nname: " + tc.fm + "\ndescription: ok\n---\nbody\n"
			d := writeSkill(t, root, tc.name, body)
			_, err := Load(d)
			if err == nil {
				t.Fatalf("expected error for name %q", tc.fm)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "SKILL.md") {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestLoadDescriptionRules(t *testing.T) {
	cases := []struct {
		name string
		fm   string // full frontmatter block minus delimiters
	}{
		{"missing", "name: my-skill\n"},
		{"empty", "name: my-skill\ndescription: \"\"\n"},
		{"oversize", "name: my-skill\ndescription: " + strings.Repeat("x", 1025) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			body := "---\n" + tc.fm + "---\nbody\n"
			d := writeSkill(t, root, "my-skill", body)
			_, err := Load(d)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), "description") {
				t.Errorf("error %q does not mention description", err)
			}
			if !strings.Contains(err.Error(), "SKILL.md") {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestLoadMultibyteLengthLimits(t *testing.T) {
	root := t.TempDir()
	// Description of exactly 1024 multi-byte runes must be valid (character
	// count, not byte count).
	desc1024 := strings.Repeat("é", 1024)
	d := writeSkill(t, root, "my-skill", "---\nname: my-skill\ndescription: "+desc1024+"\n---\nbody\n")
	if _, err := Load(d); err != nil {
		t.Fatalf("1024-rune description should be valid: %v", err)
	}

	desc1025 := strings.Repeat("é", 1025)
	d = writeSkill(t, root, "my-skill", "---\nname: my-skill\ndescription: "+desc1025+"\n---\nbody\n")
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("1025-rune description should be invalid, got err=%v", err)
	}

	compat500 := strings.Repeat("é", 500)
	d = writeSkill(t, root, "my-skill", "---\nname: my-skill\ndescription: ok\ncompatibility: "+compat500+"\n---\nbody\n")
	if _, err := Load(d); err != nil {
		t.Fatalf("500-rune compatibility should be valid: %v", err)
	}

	compat501 := strings.Repeat("é", 501)
	d = writeSkill(t, root, "my-skill", "---\nname: my-skill\ndescription: ok\ncompatibility: "+compat501+"\n---\nbody\n")
	if _, err := Load(d); err == nil || !strings.Contains(err.Error(), "compatibility") {
		t.Fatalf("501-rune compatibility should be invalid, got err=%v", err)
	}
}

func TestLoadDuplicateMetadataBlock(t *testing.T) {
	root := t.TempDir()
	body := "---\nname: my-skill\ndescription: ok\nmetadata:\n  a: 1\nmetadata:\n  b: 2\n---\nbody\n"
	d := writeSkill(t, root, "my-skill", body)
	_, err := Load(d)
	if err == nil {
		t.Fatalf("expected error for duplicate metadata block")
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error %q should name a duplicate metadata key", err)
	}
}

func TestLoadCompatibilityOversize(t *testing.T) {
	root := t.TempDir()
	body := "---\nname: my-skill\ndescription: ok\ncompatibility: " + strings.Repeat("y", 501) + "\n---\nbody\n"
	d := writeSkill(t, root, "my-skill", body)
	_, err := Load(d)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "compatibility") {
		t.Errorf("error %q does not mention compatibility", err)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	root := t.TempDir()
	body := "---\nname: my-skill\ndescription: ok\nbogus: value\n---\nbody\n"
	d := writeSkill(t, root, "my-skill", body)
	_, err := Load(d)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the unknown key", err)
	}
}

func TestLoadMissingFrontmatter(t *testing.T) {
	root := t.TempDir()
	body := "# No frontmatter here\n\nJust markdown.\n"
	d := writeSkill(t, root, "my-skill", body)
	_, err := Load(d)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("error %q does not mention frontmatter", err)
	}
}

func TestLoadUnterminatedFrontmatter(t *testing.T) {
	root := t.TempDir()
	body := "---\nname: my-skill\ndescription: ok\nbody without closing delimiter\n"
	d := writeSkill(t, root, "my-skill", body)
	_, err := Load(d)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("error %q does not mention frontmatter", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "no-skill")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(d); err == nil {
		t.Fatalf("expected error for missing SKILL.md")
	}
}

func TestInstructions(t *testing.T) {
	root := t.TempDir()
	d := writeSkill(t, root, "my-skill", minimalBody)
	s, err := Load(d)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	body, err := s.Instructions()
	if err != nil {
		t.Fatalf("Instructions: %v", err)
	}
	want := "# Heading\n\nBody text.\n"
	if body != want {
		t.Errorf("Instructions() = %q, want %q", body, want)
	}
}

func TestDiscoverSorted(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		body := "---\nname: " + name + "\ndescription: desc\n---\nbody\n"
		writeSkill(t, root, name, body)
	}
	// A plain directory without SKILL.md must be skipped.
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray file at root must be ignored.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var got []string
	for _, s := range skills {
		got = append(got, s.Name)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Discover names = %v, want %v", got, want)
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	skills, err := Discover(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected empty, got %v", skills)
	}
}

func TestDiscoverInvalidSkillFails(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "---\nname: good\ndescription: ok\n---\nbody\n")
	badDir := writeSkill(t, root, "bad", "---\nname: mismatch\ndescription: ok\n---\nbody\n")

	_, err := Discover(root)
	if err == nil {
		t.Fatalf("expected Discover to fail on invalid skill")
	}
	if !strings.Contains(err.Error(), filepath.Join(badDir, "SKILL.md")) {
		t.Errorf("error %q does not contain bad skill path %q", err, badDir)
	}
}
