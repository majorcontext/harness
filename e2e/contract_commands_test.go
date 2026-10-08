package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

const (
	reviewCommand  = "---\ndescription: Review a ref\nargument-hint: <ref>\n---\nReview $1 now.\n"
	badCommand     = "no frontmatter\n"
	badNameCommand = "---\ndescription: x\n---\nx\n"
)

var commandFiles = []action{
	writeFile{path: ".agents/commands/review.md", body: reviewCommand},
	writeFile{path: ".agents/commands/bad.md", body: badCommand},
	writeFile{path: ".agents/commands/Bad Name.md", body: badNameCommand},
}

// typed sends a typed line and waits until the command that it starts has ended.
func typed(as, line string) []action {
	return []action{command{as: as, text: line}, awaitCommands{as: as}}
}

// viaSymlinkedAncestor is the driver of a scenario whose work dir is reached
// through a symlink to its parent.
func viaSymlinkedAncestor(t *testing.T, h host, modelURL string) driver {
	t.Helper()
	real := filepath.Join(resolved(t.TempDir()), "real")
	if err := os.MkdirAll(filepath.Join(real, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(resolved(t.TempDir()), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg := writeGoalConfigWith(t, modelURL, scenarioConfig(nil))
	return h.openIn(t, cfg, filepath.Join(link, "work"), nil)
}

func TestContractCommands(t *testing.T) {
	ok := harnesstest.Step{Name: "ok", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	slow := harnesstest.Step{Name: "slow", Match: harnesstest.LastUserText("work"), Reply: harnesstest.Reply{Text: "partial", Block: true}}
	turn := func(user string) harnesstest.Step {
		return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "ok"}}
	}
	summary := harnesstest.Step{Name: "summary", Reply: harnesstest.Reply{Text: "gist"}}
	keepOne := map[string]any{"compaction_keep_turns": 1}
	seq := func(parts ...[]action) []action {
		var out []action
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	runScenarios(t, []scenario{
		{
			name:  "typed_commands_record_their_outcome",
			model: []harnesstest.Step{slow, ok},
			actions: seq(commandFiles, []action{create{as: "a"}},
				[]action{command{as: "a", text: "/thinking high", repeatable: true}, awaitCommands{as: "a"}},
				[]action{repeatInput{as: "a", text: "/thinking high", typed: true}, repeatInput{as: "a", text: "/thinking low", typed: true}, repeatInput{as: "a", text: "hi"}},
				typed("a", "/compact abc"),
				typed("a", "/clear"),
				typed("a", "/compact"),
				typed("a", "/compact 5"),
				typed("a", "/compact 0"),
				[]action{
					command{as: "a", text: "/nosuch x"}, waitIdle{as: "a"},
					command{as: "a", text: "//compact"}, waitIdle{as: "a"},
					command{as: "a", text: "/review HEAD"}, waitIdle{as: "a"},
					submit{as: "a", text: "/compact"}, waitIdle{as: "a"},
					submit{as: "a", text: "work"},
					awaitRequests{n: 5},
					command{as: "a", text: "/compact"},
					command{as: "a", text: "/abort"},
					awaitCommands{as: "a"},
					waitIdle{as: "a"},
					commandRecords{as: "a"},
				}),
		},
		{
			name:   "typed_compact_keeps_keep_turns_and_returns_the_range",
			config: keepOne,
			model:  []harnesstest.Step{turn("t1"), turn("t2"), turn("t3"), summary},
			actions: seq([]action{
				create{as: "a"},
				submit{as: "a", text: "t1"}, waitIdle{as: "a"},
				submit{as: "a", text: "t2"}, waitIdle{as: "a"},
				submit{as: "a", text: "t3"}, waitIdle{as: "a"},
			}, typed("a", "/compact 1"), []action{commandRecords{as: "a"}}),
		},
		{
			name:   "a_kill_interrupts_an_unfinished_command",
			config: keepOne,
			model:  []harnesstest.Step{turn("one"), turn("two"), {Name: "summary", Reply: harnesstest.Reply{Text: "gist", Block: true}, Repeat: true}},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "one"}, waitIdle{as: "a"},
				submit{as: "a", text: "two"}, waitIdle{as: "a"},
				command{as: "a", text: "/compact", repeatable: true},
				awaitRequests{n: 3},
				restart{kill: true},
				repeatInput{as: "a", text: "/compact", typed: true},
				commandRecords{as: "a"},
				release{step: "summary"},
				waitIdle{as: "a"},
			},
		},
		{
			name:    "commands_menu_lists_builtin_and_prompt_commands",
			actions: append(append([]action{}, commandFiles...), commands{}),
		},
		{
			name:    "commands_menu_lists_prompt_commands_under_a_symlinked_ancestor",
			driver:  viaSymlinkedAncestor,
			actions: append(append([]action{}, commandFiles...), commands{}),
		},
	})
}

func TestContractCommandsEnd(t *testing.T) {
	turn := func(user string) harnesstest.Step {
		return harnesstest.Step{Name: user, Match: harnesstest.LastUserText(user), Reply: harnesstest.Reply{Text: "ok"}}
	}
	runScenarios(t, []scenario{{
		name:   "end_is_refused_while_a_typed_command_runs",
		config: map[string]any{"compaction_keep_turns": 1},
		model:  []harnesstest.Step{turn("one"), turn("two"), {Name: "summary", Reply: harnesstest.Reply{Text: "gist", Block: true}}},
		actions: []action{
			create{as: "a"},
			submit{as: "a", text: "one"}, waitIdle{as: "a"},
			submit{as: "a", text: "two"}, waitIdle{as: "a"},
			command{as: "a", text: "/compact"},
			awaitRequests{n: 3},
			endSession{as: "a"},
			release{step: "summary"},
			awaitCommands{as: "a"},
			waitIdle{as: "a"},
			commandRecords{as: "a"},
			endSession{as: "a"},
		},
	}})
}
