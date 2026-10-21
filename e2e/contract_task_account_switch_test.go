package e2e

import (
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

// accountSwitchModel is the model of the entry with no vendor.
const accountSwitchModel = "plain/gpt-test"

// newAccountSwitchDriver configures the anthropic entry with vendor codex behind the account proxy, and the
// openai-compat entry "plain" with no vendor on a chat server that answers the session that runs on it.
func newAccountSwitchDriver(t *testing.T, h host, modelURL string) driver {
	t.Helper()
	plain := harnesstest.NewChat(t,
		taskStep("spawn_on_plain", userStarts("delegate on plain"), fixed(withAccounts(spawn("general-purpose", "child of the plain parent")))),
		harnesstest.Step{Name: "refused", Match: lastResultHas("has no vendor"), Reply: harnesstest.Reply{Text: "refused"}},
	)
	proxyURL, capture := newAccountProxy(t, modelURL)
	configPath := writeGoalConfigWith(t, "http://subscription-api.test", map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://subscription-api.test", "vendor": "codex"},
			"plain":     map[string]any{"type": "openai-compat", "api_key_env": "ANTHROPIC_API_KEY", "base_url": plain.URL() + "/v1"},
		},
		"account_routing": map[string]any{"proxy_url_env": "ACCOUNT_PROXY", "separator": "|"},
	})
	env := map[string]string{"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL, "http_proxy": proxyURL, "https_proxy": proxyURL,
		"ACCOUNT_PROXY": proxyURL, "NO_PROXY": "localhost,127.0.0.1", "E2E_MODEL_PROXY": proxyURL}
	return &accountProxyDriver{laneHost: h.open(t, configPath, env), capture: capture}
}

func withAccounts(args map[string]any) map[string]any {
	args["accounts"] = map[string]string{"codex": "acct_switch"}
	return args
}

// A spawn with accounts and no model checks the vendor of the model that the child gets: the current model of
// the parent, not the model that created it.
func TestContractTaskAccountsFollowTheParentModel(t *testing.T) {
	runScenarios(t, []scenario{{
		name: "task_spawn_accounts_check_the_current_model_of_the_parent", concurrent: true, driver: newAccountSwitchDriver,
		model: []harnesstest.Step{
			taskStep("spawn_on_vendor", userStarts("delegate on vendor"), fixed(withAccounts(spawn("general-purpose", "child of the vendor parent")))),
			{Name: "child", Match: userStarts("child of the vendor parent"), Reply: harnesstest.Reply{Text: "child done"}},
			{Name: "parent_ack", Match: matchAll(rootStarts("delegate on vendor"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
			{Name: "parent_report", Match: rootStarts("delegate on vendor"), Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		},
		actions: []action{
			create{as: "a"},
			setModel{as: "a", model: accountSwitchModel},
			submit{as: "a", text: "delegate on plain"},
			waitIdle{as: "a"},
			create{as: "b", model: accountSwitchModel},
			setModel{as: "b", model: "anthropic/claude-fable-5"},
			submit{as: "b", text: "delegate on vendor"},
			awaitRequests{n: 3},
			bindChild{as: "kid", parent: "b"},
			awaitSettled{as: "kid", parent: "b"},
			waitIdle{as: "b"},
			recordProxyAccounts{expected: []string{"acct_switch"}},
		},
	}})
}
