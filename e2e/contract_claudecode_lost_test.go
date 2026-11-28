package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

var claudeLostRows = map[string]runtimeRow{
	"claudecode_lost_conversation_runs_the_turn_on_a_new_cli_session": sameAsServe(),
	"claudecode_other_resume_failure_still_fails_the_turn":            sameAsServe(),
}

// claudeForget removes the sessions that the "transcripts" mode of fakeclaude
// knows, as a new machine has a CLI config directory with no transcript.
type claudeForget struct{}

func (claudeForget) run(t *testing.T, r *run) {
	path := filepath.Join(claudeDriverOf(t, r).stateDir, "parked.sessions")
	if err := os.Remove(path); err != nil {
		t.Fatalf("forget sessions: %v", err)
	}
}

func TestContractClaudeCodeLostConversation(t *testing.T) {
	note := "main moved by 3 commits; your work was rebased onto it."
	firstTurn := []action{create{as: "a"}, submit{as: "a", text: "hi"}, waitIdle{as: "a"}, claudeForget{}}
	runScenarios(t, []scenario{
		{
			name:   "claudecode_lost_conversation_runs_the_turn_on_a_new_cli_session",
			driver: claudeLane{mode: "transcripts", historyTool: true}.newDriver,
			actions: withActions(firstTurn,
				postInputContext{as: "a", text: "after the move", context: note}, waitIdle{as: "a"},
				claudeHistoryTool{as: "a"}, claudeSession{as: "a"},
				submit{as: "a", text: "third"}, waitIdle{as: "a"},
				claudeTurnEnds{as: "a"}, claudeInvocations{as: "a"}, claudeInputs{as: "a"},
			),
		},
		{
			name: "claudecode_other_resume_failure_still_fails_the_turn",
			driver: claudeLane{mode: "transcripts", env: map[string]string{
				"FAKE_CLAUDE_RESUME_ERROR": "No conversation found with session ID: fake-session-9",
			}}.newDriver,
			actions: withActions(firstTurn,
				submit{as: "a", text: "again"}, waitIdle{as: "a"},
				claudeTurnEnds{as: "a"}, claudeInvocations{as: "a"}, claudeInputs{as: "a"},
			),
		},
	})
}
