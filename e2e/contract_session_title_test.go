package e2e

import (
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// sessionTitleRows is the disposition of the rows of TestContractSessionTitle.
var sessionTitleRows = map[string]runtimeRow{
	"session_title_is_the_derived_title_of_the_first_prompt":                         sameAsServe(),
	"session_rename_beats_the_derived_title_and_survives_a_restart":                  sameAsServe(),
	"session_title_that_is_empty_has_a_line_break_or_is_too_long_is_invalid_request": sameAsServe(),
}

func TestContractSessionTitle(t *testing.T) {
	rest := agentStep("rest", "ok", true)
	runScenarios(t, []scenario{
		{
			name:  "session_title_is_the_derived_title_of_the_first_prompt",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "\n  fix the flaky   login test, then rerun the whole suite twice\nsecond line"},
				waitIdle{as: "a"},
				getSession{as: "a"},
			},
		},
		{
			name:  "session_rename_beats_the_derived_title_and_survives_a_restart",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first prompt"},
				waitIdle{as: "a"},
				setTitle{as: "a", title: "  Login fix  "},
				getSession{as: "a"},
				restart{},
				getSession{as: "a"},
			},
		},
		{
			name:  "session_title_that_is_empty_has_a_line_break_or_is_too_long_is_invalid_request",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				setTitle{as: "a", title: "   "},
				setTitle{as: "a", title: "two\nlines"},
				setTitle{as: "a", title: strings.Repeat("x", 201)},
				getSession{as: "a"},
			},
		},
	})
}
