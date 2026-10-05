package e2e

import (
	"maps"
	"net/http"
	"os"
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
	batchingSuffix = "to determine the dependent values."
)

func skillFile(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
}

// inDir writes files into the work dir, then runs rest.
func inDir(files map[string]string, rest ...action) []action {
	var out []action
	for _, path := range slices.Sorted(maps.Keys(files)) {
		out = append(out, writeFile{path: path, body: files[path]})
	}
	return append(out, rest...)
}

// systemTail checks the system prompt of the first model request after the
// base prompt. It records whether the tool-batching segment follows the base
// prompt and whether a blank line joins the segments. It cuts out the batching
// segment and its separator, then the rest must equal the segments joined by
// that separator. A turn that fails before its first model call records sent
// false.
type systemTail struct {
	parts []string // the segments after the base prompt, with the work dir as <workdir>
}

func (a systemTail) run(t *testing.T, r *run) {
	t.Helper()
	reqs := r.fake.Requests()
	rec := map[string]any{"sent": len(reqs) > 0, "batching_segment": false, "blank_line_join": false}
	if len(reqs) > 0 {
		sys := reqs[0].System
		for _, w := range workdirPaths(r.drv.Workdir()) {
			sys = strings.ReplaceAll(sys, w, "<workdir>")
		}
		_, tail, ok := strings.Cut(sys, "Working directory: <workdir>")
		if !ok {
			t.Fatalf("the base prompt does not end with the work dir:\n%s", sys)
		}
		sep := "\n"
		if strings.HasPrefix(tail, "\n\n") {
			sep = "\n\n"
		}
		rec["blank_line_join"] = sep == "\n\n"
		if i := strings.Index(tail, batchingPrefix); i >= 0 {
			rec["batching_segment"] = true
			end := strings.Index(tail[i:], batchingSuffix)
			if end < 0 {
				t.Fatalf("the tool-batching segment does not end as expected:\n%s", tail[i:])
			}
			head, rest := tail[:i], tail[i+end+len(batchingSuffix):]
			if rest != "" {
				rest = strings.TrimPrefix(rest, sep)
			} else {
				head = strings.TrimSuffix(head, sep)
			}
			tail = head + rest
		}
		want := ""
		if len(a.parts) > 0 {
			want = sep + strings.Join(a.parts, sep)
		}
		if tail != want {
			t.Errorf("system segments after the base prompt differ:\n got %q\nwant %q", tail, want)
		}
	}
	r.record(t, "system_tail", "", callResult{Status: http.StatusOK, Body: rec})
}

// inTree is the driver of a scenario whose work dir is sub of a tree that
// holds files.
func inTree(files map[string]string, sub string) func(*testing.T, host, string) driver {
	return inTreeWith(files, sub, nil)
}

// inTreeWith is inTree with keys in the user config of the host.
func inTreeWith(files map[string]string, sub string, config map[string]any) func(*testing.T, host, string) driver {
	return func(t *testing.T, h host, modelURL string) driver {
		t.Helper()
		base := resolved(t.TempDir())
		for rel, body := range files {
			path := filepath.Join(base, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cfg := writeGoalConfigWith(t, modelURL, scenarioConfig(config))
		workDir := filepath.Join(base, sub)
		if h.runtime {
			return newRuntimeDriverIn(t, cfg, false, workDir)
		}
		d := &httpDriver{sessDir: t.TempDir(), workDir: workDir, config: cfg, enqSeq: map[string]int64{}, typedSeq: map[string]int64{}}
		d.p = d.serve(t)
		return d
	}
}

func TestContractPromptSkills(t *testing.T) {
	runScenarios(t, []scenario{
		{
			name:  "skills_listed_sorted_and_read_through_read_file",
			model: toolChain(ftRead(ftArgs("path", ".agents/skills/demo-skill/SKILL.md"))),
			actions: inDir(map[string]string{
				".agents/skills/demo-skill/SKILL.md": skillFile("demo-skill", "Demonstrates discovery.", "The skill body."),
				".agents/skills/alpha/SKILL.md":      skillFile("alpha", "Sorts first.", "Alpha body."),
			}, append(slices.Clone(oneTurn), systemTail{parts: []string{skillHeader +
				"\nalpha — Sorts first. (path: <workdir>/.agents/skills/alpha/SKILL.md)" +
				"\ndemo-skill — Demonstrates discovery. (path: <workdir>/.agents/skills/demo-skill/SKILL.md)"}})...),
		},
		{
			name:   "skills_dirs_config_replaces_the_default_dir",
			config: map[string]any{"skills_dirs": []string{"custom"}},
			model:  textReply("ok"),
			actions: inDir(map[string]string{
				".agents/skills/ignored/SKILL.md": skillFile("ignored", "Not scanned.", "x"),
				"custom/picked/SKILL.md":          skillFile("picked", "Scanned.", "x"),
			}, append(slices.Clone(oneTurn), systemTail{parts: []string{skillHeader + "\npicked — Scanned. (path: <workdir>/custom/picked/SKILL.md)"}})...),
		},
		{
			name:   "skills_from_several_dirs_are_listed_sorted_by_name",
			config: map[string]any{"skills_dirs": []string{"zdir", "adir"}},
			model:  textReply("ok"),
			actions: inDir(map[string]string{
				"zdir/zeta/SKILL.md":  skillFile("zeta", "Last.", "x"),
				"adir/alpha/SKILL.md": skillFile("alpha", "First.", "x"),
			}, append(slices.Clone(oneTurn), systemTail{parts: []string{skillHeader +
				"\nalpha — First. (path: <workdir>/adir/alpha/SKILL.md)\nzeta — Last. (path: <workdir>/zdir/zeta/SKILL.md)"}})...),
		},
		{
			name:   "skills_dirs_empty_list_disables_discovery",
			config: map[string]any{"skills_dirs": []string{}},
			model:  textReply("ok"),
			actions: inDir(map[string]string{".agents/skills/ignored/SKILL.md": skillFile("ignored", "Not scanned.", "x")},
				append(slices.Clone(oneTurn), systemTail{})...),
		},
	})
}

const (
	sections      = "# Alpha\nalpha body line one\nalpha body line two\n# Beta\nbeta body\n# Gamma\ngamma body\n"
	oversizeMark  = "[... truncated: <workdir>/AGENTS.md is 160 bytes. The first 50 bytes are above. 110 bytes are not shown. Read the full file with the read_file tool. ...]"
	chainHeader   = "Project instructions, root to working directory. The deepest file wins on conflict.\n\n"
	instructionOf = "Project instructions from AGENTS.md:\n\n"
)

func TestContractPromptInstructions(t *testing.T) {
	plain := strings.Repeat("plain text line\n", 10)
	runScenarios(t, []scenario{
		{
			name:  "instructions_single_file_at_workdir",
			model: textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": "# Rules\nBe brief.\n"},
				append(slices.Clone(oneTurn), systemTail{parts: []string{instructionOf + "# Rules\nBe brief.\n"}})...),
		},
		{
			name:  "instructions_chain_runs_root_to_workdir_and_stops_at_the_git_root",
			model: textReply("ok"),
			driver: inTree(map[string]string{
				"outer/AGENTS.md":           "above the repository",
				"outer/repo/.git":           "gitdir: elsewhere",
				"outer/repo/AGENTS.md":      "# Root\nroot rule",
				"outer/repo/AGENT.md":       "ignored: AGENTS.md wins",
				"outer/repo/sub/AGENT.md":   "# Sub\nsub rule",
				"outer/repo/sub/deep/.keep": "",
			}, "outer/repo/sub/deep"),
			actions: append(slices.Clone(oneTurn), systemTail{parts: []string{chainHeader +
				"From ../../AGENTS.md:\n\n# Root\nroot rule\n\nFrom ../AGENT.md:\n\n# Sub\nsub rule"}}),
		},
		{
			name:  "instructions_outside_a_repository_read_only_the_workdir_file",
			model: textReply("ok"),
			driver: inTree(map[string]string{
				"AGENTS.md":      "above",
				"work/AGENTS.md": "mine\n",
			}, "work"),
			actions: append(slices.Clone(oneTurn), systemTail{parts: []string{instructionOf + "mine\n"}}),
		},
		{
			name:   "instructions_oversize_with_headings_become_an_outline_read_by_range",
			config: map[string]any{"instructions_max_bytes": 50},
			model:  toolChain(ftRead(ftArgs("path", "AGENTS.md", "offset", 4, "limit", 2))),
			actions: inDir(map[string]string{"AGENTS.md": sections}, append(slices.Clone(oneTurn), systemTail{parts: []string{
				instructionOf + "# Alpha\nalpha body line one\nalpha body line two\n\n" +
					"[instructions outline] 2 of the 3 sections of <workdir>/AGENTS.md are not in this prompt. You MUST read a section with the read_file tool before you rely on it:\n" +
					"  Beta — read_file(path=<workdir>/AGENTS.md, offset=4, limit=2) — beta body\n" +
					"  Gamma — read_file(path=<workdir>/AGENTS.md, offset=6, limit=2) — gamma body"}})...),
		},
		{
			name:   "instructions_mode_full_keeps_the_truncation_marker",
			config: map[string]any{"instructions_max_bytes": 50, "instructions_mode": "full"},
			model:  textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": sections}, append(slices.Clone(oneTurn), systemTail{parts: []string{
				instructionOf + "# Alpha\nalpha body line one\nalpha body line two\n# \n" +
					"[... truncated: <workdir>/AGENTS.md is 84 bytes. The first 50 bytes are above. 34 bytes are not shown. Read the full file with the read_file tool. ...]"}})...),
		},
		{
			name:   "instructions_oversize_without_headings_keep_the_truncation_marker",
			config: map[string]any{"instructions_max_bytes": 50},
			model:  textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": plain}, append(slices.Clone(oneTurn), systemTail{parts: []string{
				instructionOf + "plain text line\nplain text line\nplain text line\npl\n" + oversizeMark}})...),
		},
		{
			name:   "instructions_negative_max_bytes_keeps_the_whole_file",
			config: map[string]any{"instructions_max_bytes": -1},
			model:  textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": strings.Repeat("x", 65<<10)},
				append(slices.Clone(oneTurn), systemTail{parts: []string{instructionOf + strings.Repeat("x", 65<<10)}})...),
		},
		{
			name:   "instructions_false_in_config_injects_nothing",
			config: map[string]any{"instructions": false},
			model:  textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": sections},
				append(slices.Clone(oneTurn), systemTail{})...),
		},
		{
			name:   "instructions_path_in_config_replaces_discovery",
			config: map[string]any{"instructions_path": "other.md"},
			model:  textReply("ok"),
			actions: inDir(map[string]string{"AGENTS.md": sections, "other.md": "OTHER\n"},
				append(slices.Clone(oneTurn), systemTail{parts: []string{"Project instructions from other.md:\n\nOTHER\n"}})...),
		},
	})
}

func TestContractPromptSystemOrder(t *testing.T) {
	runScenarios(t, []scenario{{
		name:  "system_segments_order_append_layers_then_instructions_then_skills",
		model: textReply("ok"),
		driver: inTreeWith(map[string]string{
			"AGENTS.md":                        "be brief\n",
			".harness.json":                    `{"append_system_prompt": ["PROJECT-LAYER"]}`,
			".agents/skills/demo/SKILL.md":     skillFile("demo", "Demo.", "x"),
			".agents/skills/zeta-two/SKILL.md": skillFile("zeta-two", "Zeta.", "x"),
		}, "", map[string]any{"append_system_prompt": []string{"USER-LAYER"}}),
		actions: append(slices.Clone(oneTurn), systemTail{parts: []string{
			"USER-LAYER", "PROJECT-LAYER",
			instructionOf + "be brief\n",
			skillHeader + "\ndemo — Demo. (path: <workdir>/.agents/skills/demo/SKILL.md)\nzeta-two — Zeta. (path: <workdir>/.agents/skills/zeta-two/SKILL.md)",
		}}),
	}})
}

func TestContractPromptDiscoveryFailures(t *testing.T) {
	oneTurnThenStatus := append(slices.Clone(oneTurn), getSession{as: "a"})
	runScenarios(t, []scenario{
		{
			name:  "instructions_empty_file_fails_the_turn",
			model: []harnesstest.Step{replyText("ok")},
			actions: inDir(map[string]string{"AGENTS.md": "  \n"},
				append(slices.Clone(oneTurnThenStatus), systemTail{})...),
		},
		{
			name:  "instructions_empty_and_invalid_files_in_a_chain_fail_the_turn",
			model: []harnesstest.Step{replyText("ok")},
			driver: inTree(map[string]string{
				".git/HEAD":     "x",
				"AGENTS.md":     "root rule",
				"a/AGENTS.md":   " \n",
				"a/b/AGENTS.md": "\xff",
			}, "a/b"),
			actions: append(slices.Clone(oneTurnThenStatus), systemTail{parts: []string{"Project instructions from ../../AGENTS.md:\n\nroot rule"}}),
		},
		{
			name:  "skill_with_an_uppercase_name_fails_the_turn",
			model: []harnesstest.Step{replyText("ok")},
			actions: inDir(map[string]string{
				".agents/skills/Bad/SKILL.md":   skillFile("Bad", "Upper case.", "x"),
				".agents/skills/zeta/SKILL.md":  skillFile("zeta", "Last.", "x"),
				".agents/skills/alpha/SKILL.md": skillFile("alpha", "First.", "x"),
			}, append(slices.Clone(oneTurn), systemTail{parts: []string{skillHeader +
				"\nalpha — First. (path: <workdir>/.agents/skills/alpha/SKILL.md)\nzeta — Last. (path: <workdir>/.agents/skills/zeta/SKILL.md)"}})...),
		},
		{
			name:  "skill_named_unlike_its_directory_fails_the_turn",
			model: []harnesstest.Step{replyText("ok")},
			actions: inDir(map[string]string{
				".agents/skills/a/SKILL.md": skillFile("b", "Named like another directory.", "x"),
			}, append(slices.Clone(oneTurnThenStatus), systemTail{})...),
		},
	})
}
