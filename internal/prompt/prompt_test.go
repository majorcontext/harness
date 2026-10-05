package prompt_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/prompt"
)

func skillFile(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody\n"
}

func TestBuildWithNoWorkDirReadsNoFile(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{".git/HEAD": "x", "AGENTS.md": "control plane rules\n", ".agents/skills/demo/SKILL.md": skillFile("demo", "Demo.")} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	if got, want := prompt.Build(config.Config{AppendSystemPrompt: []string{"EMBEDDER"}}, ""), []string{"EMBEDDER"}; !slices.Equal(got, want) {
		t.Errorf("Build = %q, want %q", got, want)
	}
}

// readOnly are the read-only file tools of the native and Claude Code backends.
var readOnly = []string{"read_file", "glob", "grep", "ls", "Read", "Glob", "Grep"}

func TestProfiles(t *testing.T) {
	agent := func(fm, body string) string { return "---\n" + fm + "\n---\n\n" + body + "\n" }
	builtins, _ := prompt.Profiles(nil)
	gp, explore, plan := builtins[prompt.GeneralPurpose], builtins["explore"], builtins["plan"]
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []prompt.Profile
	}{
		{name: "general-purpose, explore, and plan are built in",
			want: []prompt.Profile{gp, {Name: "explore", Description: explore.Description, Tools: readOnly, Prompt: explore.Prompt},
				{Name: "plan", Description: plan.Description, Tools: readOnly, Prompt: plan.Prompt}}},
		{name: "a bad file, a subdirectory, and a file that is not markdown are skipped",
			files: map[string]string{".agents/x.md": agent("name: x\ndescription: X.\nhooks: y", "B"), ".agents/y.md": agent("description: Y.", "B"),
				".agents/z.md": "no frontmatter", ".agents/skills/s/SKILL.md": skillFile("s", "S."), ".agents/n.txt": agent("name: n\ndescription: N.", "B")},
			want: []prompt.Profile{gp, explore, plan}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tc.files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, _ := prompt.Profiles([]string{filepath.Join(root, ".agents")})
			if len(got) != len(tc.want) {
				t.Fatalf("Profiles = %+v, want %+v", got, tc.want)
			}
			for _, w := range tc.want {
				if g := got[w.Name]; g.Name != w.Name || g.Description != w.Description || g.Model != w.Model || g.Prompt != w.Prompt || !slices.Equal(g.Tools, w.Tools) {
					t.Errorf("profile %s = %+v, want %+v", w.Name, g, w)
				}
			}
		})
	}
}
