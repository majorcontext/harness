package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
	"github.com/majorcontext/harness/internal/testpoll"
)

// sessionTitleRows is the disposition of the rows of TestContractSessionTitle.
var sessionTitleRows = map[string]runtimeRow{
	"session_title_is_the_derived_title_of_the_first_prompt":                         sameAsServe(),
	"session_title_of_a_prompt_with_an_escape_sequence_has_no_control_character":     sameAsServe(),
	"session_rename_beats_the_derived_title_and_survives_a_restart":                  sameAsServe(),
	"session_title_that_is_empty_has_a_line_break_or_is_too_long_is_invalid_request": sameAsServe(),
	"session_title_model_writes_the_title_after_the_first_prompt":                    sameAsServe(),
	"session_title_rename_to_the_derived_title_beats_a_later_model_title":            sameAsServe(),
	"session_title_model_that_answers_empty_keeps_the_derived_title":                 sameAsServe(),
	"session_title_without_a_title_model_sends_no_title_request":                     sameAsServe(),
	"session_title_model_on_a_model_that_runs_its_own_agent_loop_writes_the_title":   sameAsServe(),
}

func isTitleRequest(r harnesstest.Request) bool {
	return strings.Contains(r.System, "Generate a short title for a conversation")
}

func notTitleRequest(r harnesstest.Request) bool { return !isTitleRequest(r) }

// awaitTitle polls the view of the session until its title is title.
type awaitTitle struct{ as, title string }

func (a awaitTitle) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	testpoll.Until(t, waitBound, "the title is not "+a.title, func() bool {
		body, _ := r.drv.GetSession(t, id).Body.(map[string]any)
		return body["title"] == a.title
	})
}

// noTitleRequest fails when a model server saw a title request.
type noTitleRequest struct{}

func (noTitleRequest) run(t *testing.T, r *run) {
	t.Helper()
	for _, req := range r.fake.Requests() {
		if isTitleRequest(req) {
			t.Fatalf("a title request reached the model server: %q", req.LastUserText())
		}
	}
}

func TestContractSessionTitle(t *testing.T) {
	rest := agentStep("rest", "ok", true)
	runScenarios(t, []scenario{
		{
			name:  "session_title_without_a_title_model_sends_no_title_request",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "first prompt"},
				waitIdle{as: "a"},
				noTitleRequest{},
			},
		},
		{
			name:  "session_title_is_the_derived_title_of_the_first_prompt",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "\n  fix the flaky   login test, then rerun the whole suite twice\nsecond line"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				journalPage{as: "a"},
				submit{as: "a", text: "second prompt"},
				waitIdle{as: "a"},
				journalPage{as: "a"},
			},
		},
		{
			name:  "session_title_of_a_prompt_with_an_escape_sequence_has_no_control_character",
			model: []harnesstest.Step{rest},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "\x1b[31merror\x1b[0m in build"},
				waitIdle{as: "a"},
				getSession{as: "a"},
				journalPage{as: "a"},
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
				setTitle{as: "a", title: "a\ttab"},
				setTitle{as: "a", title: strings.Repeat("x", 201)},
				getSession{as: "a"},
			},
		},
	})
}

// awaitUsage polls the view of the session until its usage input tokens reach input.
type awaitUsage struct {
	as    string
	input int
}

func (a awaitUsage) run(t *testing.T, r *run) {
	t.Helper()
	id := r.id(t, a.as)
	testpoll.Until(t, waitBound, "the usage did not reach the title call", func() bool {
		body, _ := r.drv.GetSession(t, id).Body.(map[string]any)
		u, _ := body["usage"].(map[string]any)
		return u != nil && fmt.Sprint(u["input_tokens"]) == fmt.Sprint(a.input)
	})
}

func TestContractSessionTitleModel(t *testing.T) {
	turn := harnesstest.Step{Name: "turn", Match: notTitleRequest, Reply: harnesstest.Reply{Text: "ok"}, Repeat: true}
	runScenarios(t, []scenario{
		{
			name:   "session_title_model_on_a_model_that_runs_its_own_agent_loop_writes_the_title",
			driver: ownsLoopConfig(map[string]any{"title_model": "claude-code/sonnet"}),
			model:  []harnesstest.Step{turn},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "please look at why login is flaky"},
				waitIdle{as: "a"},
				awaitTitle{as: "a", title: "Claude answer"},
				awaitUsage{as: "a", input: 12},
				getSession{as: "a"},
			},
		},
		{
			name:       "session_title_model_writes_the_title_after_the_first_prompt",
			concurrent: true,
			config:     map[string]any{"title_model": "anthropic/title-model"},
			model: []harnesstest.Step{
				turn,
				{Name: "title", Match: isTitleRequest, Reply: harnesstest.Reply{Text: "\n\"Fix the flaky login test.\"\nignored second line", Block: true, Usage: harnesstest.Usage{Input: 11, Output: 7}}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "please look at why login is flaky"},
				waitIdle{as: "a"},
				awaitRequests{n: 2},
				release{step: "title"},
				awaitTitle{as: "a", title: "Fix the flaky login test"},
				awaitUsage{as: "a", input: 16},
				getSession{as: "a"},
				journalPage{as: "a"},
			},
		},
		{
			name:       "session_title_rename_to_the_derived_title_beats_a_later_model_title",
			concurrent: true,
			config:     map[string]any{"title_model": "anthropic/title-model"},
			model: []harnesstest.Step{
				turn,
				{Name: "title", Match: isTitleRequest, Reply: harnesstest.Reply{Text: "Model title", Block: true, Usage: harnesstest.Usage{Input: 11, Output: 7}}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "please look at why login is flaky"},
				waitIdle{as: "a"},
				awaitRequests{n: 2},
				setTitle{as: "a", title: "please look at why login is flaky"},
				release{step: "title"},
				awaitUsage{as: "a", input: 16},
				getSession{as: "a"},
			},
		},
		{
			name:       "session_title_model_that_answers_empty_keeps_the_derived_title",
			concurrent: true,
			config:     map[string]any{"title_model": "anthropic/title-model"},
			model: []harnesstest.Step{
				turn,
				{Name: "title", Match: isTitleRequest, Reply: harnesstest.Reply{Text: "", Block: true, Usage: harnesstest.Usage{Input: 11, Output: 7}}},
			},
			actions: []action{
				create{as: "a"},
				submit{as: "a", text: "please look at why login is flaky"},
				waitIdle{as: "a"},
				awaitRequests{n: 2},
				release{step: "title"},
				awaitUsage{as: "a", input: 16},
				getSession{as: "a"},
				journalPage{as: "a"},
			},
		},
	})
}
