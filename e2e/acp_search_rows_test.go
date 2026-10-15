package e2e

import (
	"slices"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

func acpSearchReply() []harnesstest.Step {
	return []harnesstest.Step{{Name: "search", Reply: harnesstest.Reply{Text: "Go 1.27.2 is current.", WebSearch: goSearch}}}
}

// acpOffersSearch fails when the first model request lacks the web_search tool, or when want is false and it has it.
func acpOffersSearch(want bool) acpStep {
	return acpCheck(func(t *testing.T, c *acpConn) {
		if got := slices.Contains(acpRequests(t, c, 1)[0].Tools, "web_search"); got != want {
			t.Errorf("the request offers web_search = %v, want %v", got, want)
		}
	})
}

var acpSearchScenarios = []acpScenario{
	{
		name:   "a_web_search_is_a_completed_fetch_tool_call",
		cite:   "A `web_search` part of an assistant message is a `tool_call` update with the kind `fetch`, the status `completed`, the title `Search: <query>`, the query as `rawInput`, and one `resource_link` content item for each source (see Web search).",
		model:  acpSearchReply(),
		config: searchOn,
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "find the release", stop: "end_turn"},
			acpOffersSearch(true),
		},
	},
	{
		name:   "a_failed_web_search_is_a_failed_fetch_tool_call",
		cite:   "A failed search is the same update with the status `failed` and the error code as text content.",
		model:  []harnesstest.Step{{Name: "search", Reply: harnesstest.Reply{Text: "I could not search.", WebSearch: failedSearch}}},
		config: searchOn,
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "find the release", stop: "end_turn"},
		},
	},
	{
		name:    "allowed_tools_without_web_search_turn_the_search_off",
		cite:    "A session whose `allowed_tools` leaves out `web_search` gets no web search, also when `web.search` is true.",
		model:   acpSearchReply(),
		config:  searchOn,
		allowed: []string{"read_file", "grep"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "find the release", stop: "end_turn"},
			acpOffersSearch(false),
		},
	},
	{
		name:    "allowed_tools_that_name_web_search_keep_the_search",
		cite:    "A session whose `allowed_tools` leaves out `web_search` gets no web search, also when `web.search` is true.",
		model:   acpSearchReply(),
		config:  searchOn,
		allowed: []string{"read_file", "web_search"},
		steps: []acpStep{
			acpNew{as: "a"},
			acpPrompt{as: "a", text: "find the release", stop: "end_turn"},
			acpOffersSearch(true),
		},
	},
}
