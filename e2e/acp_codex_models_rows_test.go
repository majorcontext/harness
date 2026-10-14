package e2e

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

var chatgptCatalog = []harnesstest.CodexModel{
	{Slug: "live-b", DisplayName: "Live B", Description: "Second in the catalog.", Priority: 2},
	{Slug: "live-hidden", DisplayName: "Live Hidden", Description: "Hidden from the picker.", Visibility: "hide", Priority: 0},
	{Slug: "live-a", DisplayName: "Live A", Description: "First in the catalog.", Priority: 1, Efforts: []string{"none", "low", "high", "xhigh"}},
}

// acpCodexGroup checks the codex group of the model option of the first session/new answer against want, a list of model ids.
func acpCodexGroup(want ...string) acpCheck {
	return func(t *testing.T, c *acpConn) {
		t.Helper()
		var ids []string
		for _, v := range acpGroups(t, acpNewOptions(c)[0])["codex"].order {
			ids = append(ids, strings.TrimPrefix(v, "codex/"))
		}
		if !slices.Equal(ids, want) {
			t.Errorf("codex models = %v, want %v", ids, want)
		}
	}
}

func acpStaticCodexModels(t *testing.T, c *acpConn) {
	t.Helper()
	if got := len(acpGroups(t, acpNewOptions(c)[0])["codex"].order); got != 8 {
		t.Errorf("the model option lists %d codex models, want the 8 static ones", got)
	}
}

var acpCodexModelsScenarios = []acpScenario{
	{
		name:  "session_new_lists_the_models_that_the_chatgpt_backend_serves",
		cite:  "`Runtime.Models` fetches the model list of a provider entry with `auth` `chatgpt` from the ChatGPT backend on its first call, and keeps it for the life of the process.",
		login: &acpLoginSpec{chatgpt: true, stored: true, models: chatgptCatalog},
		steps: []acpStep{
			acpNew{as: "a"},
			acpCodexGroup("live-a", "live-b", "gpt-5.6-sol"),
			acpCheck(func(t *testing.T, c *acpConn) {
				v := acpGroups(t, acpNewOptions(c)[0])["codex"].values["codex/live-a"]
				if v.name != "Live A" || !strings.HasSuffix(v.description, "First in the catalog") {
					t.Errorf("live-a = %+v, want the name and the description of the backend", v)
				}
			}),
			acpSetConfig{as: "a", id: "model", value: "codex/live-a"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if got := acpLastEffortValues(c); got != "default,off,low,high" {
					t.Errorf("effort values of live-a = %v, want default and the levels of the backend that harness knows", got)
				}
			}),
			acpNew{as: "b"},
			acpCheck(func(t *testing.T, c *acpConn) {
				if n := c.env.login.openai.ModelRequests(); n != 1 {
					t.Errorf("the backend served %d model lists, want 1", n)
				}
			}),
		},
	},
	{
		name:  "session_new_lists_the_static_models_when_the_chatgpt_backend_fails",
		cite:  "When the fetch fails, times out, or returns no model, the list is the static list of the `codex` models.",
		login: &acpLoginSpec{chatgpt: true, stored: true, modelsStatus: http.StatusInternalServerError},
		steps: []acpStep{acpNew{as: "a"}, acpCheck(acpStaticCodexModels)},
	},
	{
		name:  "session_new_lists_the_static_models_when_the_chatgpt_login_is_rejected",
		cite:  "A login that the backend rejects with 401 or 403 gives the static list.",
		login: &acpLoginSpec{chatgpt: true, stored: true, modelsStatus: http.StatusForbidden},
		steps: []acpStep{acpNew{as: "a"}, acpCheck(acpStaticCodexModels)},
	},
}
