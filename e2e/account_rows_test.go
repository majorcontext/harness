package e2e

var accountRows = map[string]runtimeRow{
	"account_routing_missing_fails_loudly":            sameAsServe(),
	"task_accounts_clear_and_replay":                  sameAsServe(),
	"task_accounts_inherit_to_grandchild":             sameAsServe(),
	"task_send_with_accounts_rejects_a_running_child": sameAsServe(),
	"task_send_changes_the_child_account":             sameAsServe(),
	"codex_cold_send_applies_account_before_warm":     sameAsServe(),
}
