package prompt_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/config"
	"github.com/majorcontext/harness/internal/prompt"
)

const skillHeader = "Available skills. Each skill below is a capability you can use, but only its name and description are shown here. " +
	"To activate a skill you MUST first read its SKILL.md file with the read_file tool before relying on it; do not assume its contents from the description alone."

func skillFile(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody\n"
}

var builds = []struct {
	name  string
	files map[string]string
	// serve is the work dir below the tree; "-" runs with no work dir from the tree.
	serve string
	cfg   config.Config
	// want follows the base prompt; <root> is the tree.
	want []string
}{
	{name: "no work dir reads no file, not even in the process directory", serve: "-",
		files: map[string]string{".git/HEAD": "x", "AGENTS.md": "control plane rules\n", ".agents/skills/demo/SKILL.md": skillFile("demo", "Demo.")},
		cfg:   config.Config{AppendSystemPrompt: []string{"EMBEDDER"}},
		want:  []string{"EMBEDDER"}},
	{name: "append_system_prompt comes before the files",
		files: map[string]string{"AGENTS.md": "be brief\n", ".agents/skills/demo/SKILL.md": skillFile("demo", "Demo.")},
		cfg:   config.Config{AppendSystemPrompt: []string{"USER-LAYER", "PROJECT-LAYER"}},
		want: []string{"USER-LAYER", "PROJECT-LAYER", "Project instructions from AGENTS.md:\n\nbe brief\n",
			skillHeader + "\ndemo — Demo. (path: <root>/.agents/skills/demo/SKILL.md)"}},
	{name: "the chain runs from the git root to the work dir and AGENTS.md wins over AGENT.md",
		files: map[string]string{
			"outer/AGENTS.md": "above the repository", "outer/repo/.git": "gitdir: elsewhere",
			"outer/repo/AGENTS.md": "# Root\nroot rule", "outer/repo/AGENT.md": "ignored",
			"outer/repo/sub/AGENT.md": "# Sub\nsub rule", "outer/repo/sub/deep/.keep": "",
		},
		serve: "outer/repo/sub/deep",
		want: []string{"Project instructions, root to working directory. The deepest file wins on conflict.\n\n" +
			"From ../../AGENTS.md:\n\n# Root\nroot rule\n\nFrom ../AGENT.md:\n\n# Sub\nsub rule"}},
	{name: "outside a repository only the work dir file is read",
		files: map[string]string{"AGENTS.md": "above", "work/AGENTS.md": "mine\n"}, serve: "work",
		want: []string{"Project instructions from AGENTS.md:\n\nmine\n"}},
	{name: "an empty or invalid file is skipped",
		files: map[string]string{".git/HEAD": "x", "AGENTS.md": "root rule", "a/AGENTS.md": " \n", "a/b/AGENTS.md": "\xff"}, serve: "a/b",
		want: []string{"Project instructions from ../../AGENTS.md:\n\nroot rule"}},
	{name: "a file over instructions_max_bytes is cut with a marker",
		files: map[string]string{"AGENTS.md": strings.Repeat("plain text line\n", 10)},
		cfg:   config.Config{InstructionsMaxBytes: 50},
		want: []string{"Project instructions from AGENTS.md:\n\nplain text line\nplain text line\nplain text line\npl\n" +
			"[... truncated: <root>/AGENTS.md is 160 bytes. The first 50 bytes are above. 110 bytes are not shown. Read the full file with the read_file tool. ...]"}},
	{name: "a negative instructions_max_bytes keeps the whole file",
		files: map[string]string{"AGENTS.md": strings.Repeat("x", 65<<10)},
		cfg:   config.Config{InstructionsMaxBytes: -1},
		want:  []string{"Project instructions from AGENTS.md:\n\n" + strings.Repeat("x", 65<<10)}},
	{name: "instructions false reads no AGENTS.md",
		files: map[string]string{"AGENTS.md": "rules"}, cfg: config.Config{Instructions: new(false)}},
	{name: "instructions_path replaces discovery",
		files: map[string]string{"AGENTS.md": "rules", "other.md": "OTHER\n"}, cfg: config.Config{InstructionsPath: "other.md"},
		want: []string{"Project instructions from other.md:\n\nOTHER\n"}},
	{name: "skills are sorted by name and a malformed skill is skipped",
		files: map[string]string{
			".agents/skills/zeta/SKILL.md": skillFile("zeta", "Last."), ".agents/skills/alpha/SKILL.md": skillFile("alpha", "First."),
			".agents/skills/Bad/SKILL.md": skillFile("Bad", "Upper case."),
		},
		want: []string{skillHeader + "\nalpha — First. (path: <root>/.agents/skills/alpha/SKILL.md)\nzeta — Last. (path: <root>/.agents/skills/zeta/SKILL.md)"}},
	{name: "skills_dirs replaces the default dir",
		files: map[string]string{".agents/skills/ignored/SKILL.md": skillFile("ignored", "No."), "custom/picked/SKILL.md": skillFile("picked", "Yes.")},
		cfg:   config.Config{SkillsDirs: []string{"custom"}},
		want:  []string{skillHeader + "\npicked — Yes. (path: <root>/custom/picked/SKILL.md)"}},
	{name: "an empty skills_dirs lists no skill",
		files: map[string]string{".agents/skills/ignored/SKILL.md": skillFile("ignored", "No.")}, cfg: config.Config{SkillsDirs: []string{}}},
}

func TestBuild(t *testing.T) {
	for _, tc := range builds {
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
			wd, want := filepath.Join(root, tc.serve), []string{}
			if tc.serve == "-" {
				t.Chdir(root)
				wd = ""
			} else {
				want = append(want, prompt.Base(wd))
			}
			for _, w := range tc.want {
				want = append(want, strings.ReplaceAll(w, "<root>", root))
			}
			if got := prompt.Build(tc.cfg, wd); !slices.Equal(got, want) {
				t.Errorf("Build =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// readOnly are the read-only file tools of the native and Claude Code backends.
var readOnly = []string{"read_file", "glob", "grep", "ls", "Read", "Glob", "Grep"}

func TestProfiles(t *testing.T) {
	agent := func(fm, body string) string { return "---\n" + fm + "\n---\n\n" + body + "\n" }
	builtins := prompt.Profiles("")
	gp, explore, plan := builtins[prompt.GeneralPurpose], builtins["explore"], builtins["plan"]
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []prompt.Profile
	}{
		{name: "general-purpose, explore, and plan are built in",
			want: []prompt.Profile{gp, {Name: "explore", Description: explore.Description, Tools: readOnly, Prompt: explore.Prompt},
				{Name: "plan", Description: plan.Description, Tools: readOnly, Prompt: plan.Prompt}}},
		{name: "a Claude Code agent file is a profile",
			files: map[string]string{".agents/reader.md": agent("name: reader\ndescription: Reads.\ntools: ls, grep\nmodel: inherit\ncolor: blue", "Only read.")},
			want:  []prompt.Profile{gp, explore, plan, {Name: "reader", Description: "Reads.", Tools: []string{"ls", "grep"}, Prompt: "Only read."}}},
		{name: "a file with no tools allows every tool and names its model",
			files: map[string]string{".agents/fast.md": agent("name: fast\ndescription: Quick.\nmodel: test/small", "Hurry.")},
			want:  []prompt.Profile{{Name: "fast", Description: "Quick.", Model: "test/small", Prompt: "Hurry."}, gp, explore, plan}},
		{name: "a file can replace general-purpose",
			files: map[string]string{".agents/gp.md": agent("name: general-purpose\ndescription: Mine.", "Custom.")},
			want:  []prompt.Profile{{Name: "general-purpose", Description: "Mine.", Prompt: "Custom."}, explore, plan}},
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
			got := prompt.Profiles(root)
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
