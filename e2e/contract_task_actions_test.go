package e2e

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/majorcontext/harness/harnesstest"
)

type accountProxyCapture struct {
	mu    sync.Mutex
	users []string
}

func newAccountProxy(t *testing.T, modelURL string) (string, *accountProxyCapture) {
	t.Helper()
	target, err := url.Parse(modelURL)
	if err != nil {
		t.Fatal(err)
	}
	reverse := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Header.Del("Proxy-Authorization")
	}}
	capture := new(accountProxyCapture)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
		if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
			user, _, _ := strings.Cut(string(decoded), ":")
			capture.mu.Lock()
			capture.users = append(capture.users, user)
			capture.mu.Unlock()
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return (&url.URL{Scheme: "http", User: url.UserPassword("subject|box", "test-password"), Host: strings.TrimPrefix(proxy.URL, "http://")}).String(), capture
}

type accountProxyDriver struct {
	laneHost
	capture *accountProxyCapture
}

func newAccountProxyDriver(t *testing.T, h host, modelURL string) driver {
	t.Helper()
	proxyURL, capture := newAccountProxy(t, modelURL)
	entry := map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "base_url": "http://subscription-api.test", "vendor": "codex"}
	configPath := writeGoalConfigWith(t, "http://subscription-api.test", map[string]any{
		"providers":       map[string]any{"anthropic": entry},
		"account_routing": map[string]any{"proxy_url_env": "ACCOUNT_PROXY", "separator": "|"},
	})
	env := map[string]string{"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL, "http_proxy": proxyURL, "https_proxy": proxyURL,
		"ACCOUNT_PROXY": proxyURL, "NO_PROXY": "localhost,127.0.0.1", "E2E_MODEL_PROXY": proxyURL}
	return &accountProxyDriver{laneHost: h.open(t, configPath, env), capture: capture}
}

type recordProxyAccounts struct {
	expected    []string
	unqualified bool
}

func accountProxyUsers(t *testing.T, capture *accountProxyCapture, expected []string, unqualified bool) []string {
	t.Helper()
	capture.mu.Lock()
	users := slices.Clone(capture.users)
	capture.mu.Unlock()
	slices.Sort(users)
	seen := map[string]bool{}
	parents := 0
	for _, user := range users {
		seen[user] = true
		if user == "subject|box" {
			parents++
		}
	}
	if unqualified && parents == 0 {
		t.Fatalf("proxy usernames = %v, want an unqualified parent request", users)
	}
	for _, account := range expected {
		if !seen["subject|box|"+account] {
			t.Fatalf("proxy usernames = %v, want account %q", users, account)
		}
	}
	return users
}

func (a recordProxyAccounts) run(t *testing.T, r *run) {
	d, ok := r.drv.(*accountProxyDriver)
	if !ok {
		t.Fatalf("account proxy driver = %T", r.drv)
	}
	users := accountProxyUsers(t, d.capture, a.expected, a.unqualified)
	r.record(t, "proxy_accounts", "", callResult{Status: http.StatusOK, Body: users})
}

func TestContractTaskActions(t *testing.T) {
	general := "general-purpose"
	runScenarios(t, []scenario{
		{
			name:       "task_status_and_log_of_a_settled_child",
			concurrent: true,
			model: delegation(general, childDone, taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("status", kid), onSession("log", kid), onSession("log", kid, "tail", 1)}
			}))),
			actions: slices.Concat(spawned, []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
		},
		{
			name:       "task_status_and_log_of_a_failed_child",
			concurrent: true,
			model: []harnesstest.Step{
				taskStep("delegate", userStarts("delegate"), fixed(spawn(general, "child work"))),
				{Name: "child", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "looking", Block: true, ToolCalls: []harnesstest.ToolCall{
					{ID: "toolu_ls", Name: "ls", Input: map[string]any{"path": "."}},
				}}},
				{Name: "child_end", Match: harnesstest.LastToolResult("ls"), Reply: harnesstest.Reply{HTTPStatus: 400, ErrorMessage: "request rejected api_key=abcd1234efgh5678 " + strings.Repeat("detail ", 100)}},
				{Name: "ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
				taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("status", kid), onSession("log", kid, "tail", 2)}
				})),
				{Name: "rest", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
			},
			actions: slices.Concat(spawnedAfter(5), []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
		},
		{
			name:       "task_cancel_and_send_to_a_running_child",
			concurrent: true,
			model: delegation(general, childRuns, flagTaskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("send", kid, "prompt", "more work"), onSession("cancel", kid)}
			}))),
			actions: slices.Concat(spawnedRunning, []action{submit{as: "a", text: "actions"}, awaitSettled{as: "kid", parent: "a"}, writeFile{path: settledFlag, body: "1"}, waitIdle{as: "a"}, waitIdle{as: "kid"}}),
		},
		{
			name:       "task_spawn_past_max_tree_tokens_is_refused",
			concurrent: true,
			driver:     limited("max_tree_tokens", "HARNESS_MAX_TREE_TOKENS", 10),
			model:      delegation(general, childDone, taskStep("again", userStarts("again"), fixed(spawn(general, "second work")))),
			actions:    slices.Concat(spawned, []action{submit{as: "a", text: "again"}, waitIdle{as: "a"}}),
		},
	})
}

func TestContractTaskSend(t *testing.T) {
	general := "general-purpose"
	runScenarios(t, append([]scenario{
		{
			name:       "task_send_runs_a_settled_child_again",
			concurrent: true,
			model: delegation(general, childRuns,
				taskStep("queue", userStarts("queue it"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "queued work")}
				})),
				harnesstest.Step{Name: "queued", Match: userStarts("queued work"), Reply: harnesstest.Reply{Text: "queued done"}},
				taskStep("fresh", userStarts("again"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "fresh work")}
				})),
				harnesstest.Step{Name: "fresh_child", Match: userStarts("fresh work"), Reply: harnesstest.Reply{Text: "fresh done", Block: true}}),
			actions: slices.Concat(spawnedRunning, []action{
				submit{as: "a", text: "queue it"},
				awaitRequests{n: 5},
				waitIdle{as: "a"},
				release{step: "child"},
				awaitRequests{n: 7},
				waitIdle{as: "a"},
				submit{as: "a", text: "again"},
				awaitRequests{n: 10},
				waitIdle{as: "a"},
				release{step: "fresh_child"},
				awaitRequests{n: 11},
				waitIdle{as: "a"},
				waitIdle{as: "kid"},
			}),
		},
		{
			name:       "task_two_sends_to_a_settled_child_need_one_slot",
			concurrent: true,
			driver:     limited("max_concurrent_tasks", "HARNESS_MAX_CONCURRENT_TASKS", 1),
			model: delegation(general, childDone,
				taskStep("sends", userStarts("two sends"), onKid(func(kid string) []map[string]any {
					return []map[string]any{onSession("send", kid, "prompt", "more one"), onSession("send", kid, "prompt", "more two")}
				})),
				harnesstest.Step{Name: "more_one", Match: userStarts("more one"), Reply: harnesstest.Reply{Text: "one done", Block: true}},
				harnesstest.Step{Name: "more_two", Match: userStarts("more two"), Reply: harnesstest.Reply{Text: "two done", Block: true}}),
			actions: slices.Concat(spawned, []action{
				submit{as: "a", text: "two sends"},
				awaitRequests{n: 7},
				waitIdle{as: "a"},
				release{step: "more_one"},
				awaitRequests{n: 8},
				release{step: "more_two"},
				awaitRequests{n: 9},
				waitIdle{as: "a"},
				waitIdle{as: "kid"},
			}),
		},
	}, heldReportRows()...))
}

func TestContractTaskRefusedActions(t *testing.T) {
	general := "general-purpose"
	refuse := taskStep("refuse", matchAll(rootStarts("delegate"), lastResultHas(`"parent_id"`)), func(r harnesstest.Request) []map[string]any {
		kid := kidOf(r)
		return []map[string]any{
			onSession("status", resultID(r, parentIDPattern)),
			onSession("log", "ses_nope"),
			ftArgs("action", "status"),
			onSession("log", kid, "tail", -1),
			onSession("stop", kid),
		}
	})
	runScenarios(t, []scenario{{
		name:       "task_action_refusals",
		concurrent: true,
		model: delegation(general, childDone,
			taskStep("act", userStarts("actions"), onKid(func(kid string) []map[string]any { return []map[string]any{onSession("status", kid)} })),
			refuse),
		actions: slices.Concat(spawned, []action{submit{as: "a", text: "actions"}, waitIdle{as: "a"}}),
	}})
}

type hiddenCreate struct{ as string }

func (a hiddenCreate) run(t *testing.T, r *run) {
	r.ids[a.as] = r.drv.Create(t)
}

type recordAccountSettings struct{ as string }

func (a recordAccountSettings) run(t *testing.T, r *run) {
	page := r.drv.JournalPage(t, r.id(t, a.as), 0, 1000)
	body, ok := page.Body.(map[string]any)
	if !ok {
		t.Fatalf("journal page body = %T", page.Body)
	}
	events, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("journal page events = %T", body["events"])
	}
	var records []any
	for _, raw := range events {
		event := raw.(map[string]any)
		kind, _ := event["k"].(string)
		data, _ := event["d"].(map[string]any)
		var accounts any
		switch kind {
		case "session.created":
			if settings, ok := data["settings"].(map[string]any); ok {
				accounts = settings["accounts"]
			}
		case "settings.changed":
			accounts = data["accounts"]
		}
		if kind == "session.created" || kind == "settings.changed" {
			records = append(records, map[string]any{"kind": kind, "accounts": accounts})
		}
	}
	r.record(t, "account_settings", a.as, callResult{Status: page.Status, Body: records})
}

func TestContractTaskAccounts(t *testing.T) {
	runScenarios(t, []scenario{taskAccountClearRow(), taskAccountMissingRouteRow(), taskAccountGrandchildRow(), taskAccountRunningSendRow(), taskAccountSettledSendRow()})
}

func taskAccountClearRow() scenario {
	return scenario{
		name: "task_accounts_clear_and_replay", entry: map[string]any{"vendor": "codex"}, driver: newAccountProxyDriver,
		model: []harnesstest.Step{{Name: "request", Match: userStarts("route"), Reply: harnesstest.Reply{Text: "unqualified route"}}},
		actions: []action{create{as: "a"}, setAccounts{as: "a", accounts: map[string]string{"codex": "acct_old"}},
			setAccounts{as: "a", accounts: map[string]string{}}, restart{kill: true}, submit{as: "a", text: "route"},
			awaitRequests{n: 1}, waitIdle{as: "a"}, recordAccountSettings{as: "a"}, recordProxyAccounts{unqualified: true}},
	}
}

func taskAccountMissingRouteRow() scenario {
	return scenario{
		name: "account_routing_missing_fails_loudly", entry: map[string]any{"vendor": "codex"},
		actions: []action{create{as: "a"}, setAccounts{as: "a", accounts: map[string]string{"codex": "acct_missing_route"}},
			submit{as: "a", text: "probe"}, waitIdle{as: "a"}, getSession{as: "a"}},
	}
}

func taskAccountGrandchildRow() scenario {
	general := "general-purpose"
	spawnChild := spawn(general, "child work")
	spawnChild["accounts"] = map[string]string{"codex": "acct_parent"}
	return scenario{
		name: "task_accounts_inherit_to_grandchild", concurrent: true, entry: map[string]any{"vendor": "codex"}, driver: newAccountProxyDriver,
		model: []harnesstest.Step{
			taskStep("spawn", userStarts("delegate"), fixed(spawnChild)),
			taskStep("grandchild", userStarts("child work"), fixed(spawn(general, "grandchild work"))),
			{Name: "grandchild_wait", Match: userStarts("grandchild work"), Reply: harnesstest.Reply{Text: "waiting", Block: true}},
			{Name: "child_wait", Match: matchAll(rootStarts("child work"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting", Block: true}},
			{Name: "parent_ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
		},
		actions: []action{hiddenCreate{as: "a"}, submit{as: "a", text: "delegate"}, awaitRequests{n: 5},
			bindChild{as: "kid", parent: "a"}, bindChild{as: "grandchild", parent: "kid"},
			recordAccountSettings{as: "kid"}, recordAccountSettings{as: "grandchild"},
			recordProxyAccounts{expected: []string{"acct_parent"}, unqualified: true}, cancelTree{as: "a"}},
	}
}

func taskAccountRunningSendRow() scenario {
	general := "general-purpose"
	return scenario{
		name: "task_send_with_accounts_rejects_a_running_child", concurrent: true, entry: map[string]any{"vendor": "codex"}, driver: newAccountProxyDriver,
		model: []harnesstest.Step{
			taskStep("spawn", userStarts("delegate"), fixed(spawn(general, "child work"))),
			{Name: "child_wait", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "waiting", Block: true}},
			taskStep("send", matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("send", kid, "prompt", "child resume", "accounts", map[string]string{"codex": "acct_new"})}
			})),
			{Name: "send_rejected", Match: lastResultHas("accounts can only change between turns"), Reply: harnesstest.Reply{Text: "account update rejected"}},
		},
		actions: []action{hiddenCreate{as: "a"}, submit{as: "a", text: "delegate"}, awaitRequests{n: 4}, waitIdle{as: "a"},
			bindChild{as: "kid", parent: "a"}, recordAccountSettings{as: "kid"}, recordProxyAccounts{unqualified: true}, cancelTree{as: "a"}},
	}
}

func taskAccountSettledSendRow() scenario {
	spawnChild := spawn("general-purpose", "child work")
	spawnChild["accounts"] = map[string]string{"codex": "acct_parent"}
	return scenario{
		name: "task_send_changes_the_child_account", concurrent: true, entry: map[string]any{"vendor": "codex"}, driver: newAccountProxyDriver,
		model: []harnesstest.Step{
			taskStep("spawn", userStarts("delegate"), fixed(spawnChild)),
			{Name: "child_done", Match: userStarts("child work"), Reply: harnesstest.Reply{Text: "child done"}},
			{Name: "parent_ack", Match: matchAll(rootStarts("delegate"), harnesstest.LastToolResult("task")), Reply: harnesstest.Reply{Text: "waiting"}},
			taskStep("send", userStarts("update"), onKid(func(kid string) []map[string]any {
				return []map[string]any{onSession("send", kid, "prompt", "child resume", "accounts", map[string]string{"codex": "acct_resumed"})}
			})),
			{Name: "parent_done", Reply: harnesstest.Reply{Text: "ok"}, Repeat: true},
		},
		actions: []action{hiddenCreate{as: "a"}, submit{as: "a", text: "delegate"}, awaitRequests{n: 4},
			bindChild{as: "kid", parent: "a"}, awaitSettled{as: "kid", parent: "a"}, waitIdle{as: "a"},
			submit{as: "a", text: "update"}, awaitRequests{n: 7}, waitIdle{as: "kid"}, awaitSettled{as: "kid", parent: "a"}, waitIdle{as: "a"},
			recordAccountSettings{as: "kid"}, recordProxyAccounts{expected: []string{"acct_parent", "acct_resumed"}, unqualified: true}},
	}
}
