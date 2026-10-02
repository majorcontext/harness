package e2e

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const (
	skillHeader = "Available skills. Each skill below is a capability you can use, but only its name and description are shown here. " +
		"To activate a skill you MUST first read its SKILL.md file with the read_file tool before relying on it; do not assume its contents from the description alone."
	batchingPrefix = "If you intend to call multiple tools"
)

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// promptRow serves one workdir and pins the system segments that follow the
// base prompt, with the workdir masked and the tool-batching segment reduced
// to a marker.
type promptRow struct {
	name  string
	files map[string]string
	serve string
	cfg   map[string]any
	model func(workdir string) []harnesstest.Step
	tail  []string
	after func(t *testing.T, workdir string, results []transcriptPart)
}

func (r promptRow) run(t *testing.T) {
	base := runtimeWorkdir(t, r.files)
	cwd := filepath.Join(base, r.serve)
	steps := []harnesstest.Step{replyText("ok")}
	if r.model != nil {
		steps = r.model(base)
	}
	d, _ := startRuntime(t, cwd, r.cfg, steps...)
	id := runTurn(t, d, "go")

	segs := requestSystem(t, d, id)
	if !strings.HasSuffix(segs[0], "Working directory: "+cwd) {
		t.Errorf("base segment does not end with the working directory %s: ...%q", cwd, segs[0][max(0, len(segs[0])-120):])
	}
	got := make([]string, 0, len(segs))
	for _, seg := range segs[1:] {
		if strings.HasPrefix(seg, batchingPrefix) {
			seg = "<batching>"
		}
		got = append(got, strings.ReplaceAll(seg, base, "<workdir>"))
	}
	if !slices.Equal(got, r.tail) {
		t.Errorf("system segments after the base prompt differ:\n got %q\nwant %q", got, r.tail)
	}
	if r.after != nil {
		r.after(t, base, toolResults(d.Messages(t, id)))
	}
}

func runPromptRows(t *testing.T, rows []promptRow) {
	t.Helper()
	skipShort(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			row.run(t)
		})
	}
}

func TestContractRuntimeSkills(t *testing.T) {
	skillPath := func(wd, name string) string { return filepath.Join(wd, ".agents", "skills", name, "SKILL.md") }
	runPromptRows(t, []promptRow{
		{
			name: "skills_listed_sorted_and_read_through_read_file",
			files: map[string]string{
				".agents/skills/demo-skill/SKILL.md": skillFile("demo-skill", "Demonstrates discovery.", "The skill body."),
				".agents/skills/alpha/SKILL.md":      skillFile("alpha", "Sorts first.", "Alpha body."),
			},
			model: func(wd string) []harnesstest.Step {
				return toolChain(ftRead(ftArgs("path", skillPath(wd, "demo-skill"))))
			},
			tail: []string{"<batching>", skillHeader +
				"\nalpha — Sorts first. (path: <workdir>/.agents/skills/alpha/SKILL.md)" +
				"\ndemo-skill — Demonstrates discovery. (path: <workdir>/.agents/skills/demo-skill/SKILL.md)"},
			after: func(t *testing.T, _ string, results []transcriptPart) {
				if len(results) != 1 || !strings.Contains(results[0].Content, "The skill body.") || results[0].IsError {
					t.Errorf("read_file of the SKILL.md path = %+v, want the skill body", results)
				}
			},
		},
		{
			name: "skills_dirs_config_replaces_the_default_dir",
			files: map[string]string{
				".agents/skills/ignored/SKILL.md": skillFile("ignored", "Not scanned.", "x"),
				"custom/picked/SKILL.md":          skillFile("picked", "Scanned.", "x"),
			},
			cfg:  map[string]any{"skills_dirs": []string{"custom"}},
			tail: []string{"<batching>", skillHeader + "\npicked — Scanned. (path: <workdir>/custom/picked/SKILL.md)"},
		},
		{
			name: "skills_from_several_dirs_are_listed_sorted_by_name",
			files: map[string]string{
				"zdir/zeta/SKILL.md":  skillFile("zeta", "Last.", "x"),
				"adir/alpha/SKILL.md": skillFile("alpha", "First.", "x"),
			},
			cfg: map[string]any{"skills_dirs": []string{"zdir", "adir"}},
			tail: []string{"<batching>", skillHeader +
				"\nalpha — First. (path: <workdir>/adir/alpha/SKILL.md)\nzeta — Last. (path: <workdir>/zdir/zeta/SKILL.md)"},
		},
		{
			name:  "skills_dirs_empty_list_disables_discovery",
			files: map[string]string{".agents/skills/ignored/SKILL.md": skillFile("ignored", "Not scanned.", "x")},
			cfg:   map[string]any{"skills_dirs": []string{}},
			tail:  []string{"<batching>"},
		},
	})
}

func TestContractRuntimeInstructions(t *testing.T) {
	const sections = "# Alpha\nalpha body line one\nalpha body line two\n# Beta\nbeta body\n# Gamma\ngamma body\n"
	runPromptRows(t, []promptRow{
		{
			name:  "instructions_single_file_at_workdir",
			files: map[string]string{"AGENTS.md": "# Rules\nBe brief.\n"},
			tail:  []string{"<batching>", "Project instructions from AGENTS.md:\n\n# Rules\nBe brief.\n"},
		},
		{
			name: "instructions_chain_runs_root_to_workdir_and_stops_at_the_git_root",
			files: map[string]string{
				"outer/AGENTS.md":           "above the repository",
				"outer/repo/.git":           "gitdir: elsewhere",
				"outer/repo/AGENTS.md":      "# Root\nroot rule",
				"outer/repo/AGENT.md":       "ignored: AGENTS.md wins",
				"outer/repo/sub/AGENT.md":   "# Sub\nsub rule",
				"outer/repo/sub/deep/.keep": "",
			},
			serve: "outer/repo/sub/deep",
			tail: []string{"<batching>", "Project instructions, root to working directory. The deepest file wins on conflict.\n\n" +
				"From ../../AGENTS.md:\n\n# Root\nroot rule\n\nFrom ../AGENT.md:\n\n# Sub\nsub rule"},
		},
		{
			name:  "instructions_oversize_with_headings_become_an_outline_read_by_range",
			files: map[string]string{"AGENTS.md": sections},
			cfg:   map[string]any{"instructions_max_bytes": 50},
			model: func(wd string) []harnesstest.Step {
				return toolChain(ftRead(ftArgs("path", filepath.Join(wd, "AGENTS.md"), "offset", 4, "limit", 2)))
			},
			tail: []string{"<batching>", "Project instructions from AGENTS.md:\n\n# Alpha\nalpha body line one\nalpha body line two\n\n" +
				"[instructions outline] 2 of the 3 sections of <workdir>/AGENTS.md are not in this prompt. You MUST read a section with the read_file tool before you rely on it:\n" +
				"  Beta — read_file(path=<workdir>/AGENTS.md, offset=4, limit=2) — beta body\n" +
				"  Gamma — read_file(path=<workdir>/AGENTS.md, offset=6, limit=2) — gamma body"},
			after: func(t *testing.T, _ string, results []transcriptPart) {
				if len(results) != 1 || results[0].IsError || results[0].Content != "4→# Beta\n5→beta body\n[truncated: showing lines 4-5 of 7]" {
					t.Errorf("read_file of the outline range = %+v, want exactly lines 4-5, the Beta section", results)
				}
			},
		},
		{
			name:  "instructions_mode_full_keeps_the_truncation_marker",
			files: map[string]string{"AGENTS.md": sections},
			cfg:   map[string]any{"instructions_max_bytes": 50, "instructions_mode": "full"},
			tail: []string{"<batching>", "Project instructions from AGENTS.md:\n\n# Alpha\nalpha body line one\nalpha body line two\n# \n" +
				"[... truncated: <workdir>/AGENTS.md is 84 bytes. The first 50 bytes are above. 34 bytes are not shown. Read the full file with the read_file tool. ...]"},
		},
		{
			name:  "instructions_oversize_without_headings_keep_the_truncation_marker",
			files: map[string]string{"AGENTS.md": strings.Repeat("plain text line\n", 10)},
			cfg:   map[string]any{"instructions_max_bytes": 50},
			tail: []string{"<batching>", "Project instructions from AGENTS.md:\n\nplain text line\nplain text line\nplain text line\npl\n" +
				"[... truncated: <workdir>/AGENTS.md is 160 bytes. The first 50 bytes are above. 110 bytes are not shown. Read the full file with the read_file tool. ...]"},
		},
		{
			name:  "instructions_false_in_config_injects_nothing",
			files: map[string]string{"AGENTS.md": sections},
			cfg:   map[string]any{"instructions": false},
			tail:  []string{"<batching>"},
		},
		{
			name:  "instructions_path_in_config_replaces_discovery",
			files: map[string]string{"AGENTS.md": sections, "other.md": "OTHER\n"},
			cfg:   map[string]any{"instructions_path": "other.md"},
			tail:  []string{"<batching>", "Project instructions from other.md:\n\nOTHER\n"},
		},
	})
}

func TestContractRuntimeSystemOrder(t *testing.T) {
	runPromptRows(t, []promptRow{{
		name: "system_segments_order_append_layers_then_instructions_then_skills",
		files: map[string]string{
			"AGENTS.md":                        "be brief\n",
			".harness.json":                    `{"append_system_prompt": ["PROJECT-LAYER"]}`,
			".agents/skills/demo/SKILL.md":     skillFile("demo", "Demo.", "x"),
			".agents/skills/zeta-two/SKILL.md": skillFile("zeta-two", "Zeta.", "x"),
		},
		cfg: map[string]any{"append_system_prompt": []string{"USER-LAYER"}},
		tail: []string{
			"USER-LAYER", "PROJECT-LAYER", "<batching>",
			"Project instructions from AGENTS.md:\n\nbe brief\n",
			skillHeader + "\ndemo — Demo. (path: <workdir>/.agents/skills/demo/SKILL.md)\nzeta-two — Zeta. (path: <workdir>/.agents/skills/zeta-two/SKILL.md)",
		},
	}})
}

func TestContractRuntimeDiscoveryFailures(t *testing.T) {
	skipShort(t)
	rows := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			"instructions_empty_file_fails_the_turn",
			map[string]string{"AGENTS.md": "  \n"},
			"engine: instructions file <workdir>/AGENTS.md is empty",
		},
		{
			"skill_with_invalid_name_fails_the_turn",
			map[string]string{".agents/skills/Bad/SKILL.md": skillFile("Bad", "Upper case.", "x")},
			`<workdir>/.agents/skills/Bad/SKILL.md: name "Bad" contains invalid character 'B'; only lowercase a-z, 0-9 and hyphens are allowed`,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			wd := runtimeWorkdir(t, row.files)
			d, fake := startRuntime(t, wd, nil)
			id := runTurn(t, d, "go")
			lastTurn := bodyOf(t, d.GetSession(t, id))["last_turn"].(map[string]any)
			got := strings.ReplaceAll(lastTurn["error"].(string), wd, "<workdir>")
			if lastTurn["outcome"] != "error" || got != row.want {
				t.Errorf("last_turn = outcome %v error %q, want an error %q", lastTurn["outcome"], got, row.want)
			}
			if n := len(fake.Requests()); n != 0 || len(d.Messages(t, id)) != 0 {
				t.Errorf("model requests = %d, messages = %d, want the turn refused before anything is recorded", n, len(d.Messages(t, id)))
			}
		})
	}
}
