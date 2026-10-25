package e2e

const (
	specClaudeCompactSummary    = "Compaction summary. The adapter logs the summary text of the CLI"
	specView                    = "A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, context gauge, cache expiry, last turn, compaction count, subscription usage, head seq)"
	specCreateEffort            = "`Runtime.Create` applies the same check to the `effort` of its request, so `POST /sessions` with another value fails with `invalid_request` and creates no session."
	specUpdate                  = "func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)"
	specMessages                = "`GET /sessions/{id}/messages?before=&limit=` answers a `protocol.MessagePage` of the conversation that a reader sees, oldest first"
	specBootstrapGone           = "one-page-route"
	specRestartOpens            = "A restart opens no session by itself: the embedder opens each session that has work to resume"
	specInvalidReason           = "The message of an `invalid_request` error is only the reason, such as `keep_turns must be >= 1`:"
	specErrors                  = "Body: `{\"error\":{\"code\":\"...\",\"message\":\"...\",\"details\":{}}}`."
	specReceipt                 = "| New id | `201 {input_id, seq}` |"
	specDiscard                 = "the backend sends an `item.discarded` frame with the `item_id` of the dropped item before the first delta of the new one"
	specItems                   = "| `item.completed` | `protocol.ItemCompleted`: its `message` has the role `user`, `assistant`, or `tool` |"
	specOneResult               = "Every tool call item gets exactly one result item, or an open request, before its turn ends."
	specExternal                = "external-items-complete"
	specTaskInputs              = "task-notice-is-input"
	specChildReport             = "The report names the child, its agent, the outcome, and the reason, and holds the last assistant text of the child"
	specChildReason             = "The reason is a fixed prefix by `error_class`"
	specChildCrash              = "a crashed child turn ends `crashed` and settles `failed`"
	specChildLong               = "A result of at most 4096 bytes, after masking, stays whole in the report."
	specChildLost               = "a crashed turn reads `lost to restart: turn was in flight when the process last stopped`"
	specChildBound              = "masked and cut at 500 runes with `… [truncated]`"
	specChildStatus             = "The `status` and `log` actions of the `task` tool show this reason."
	specChildHint               = "the recover hint is masked and cut at 120 runes"
	specChildHandle             = "the parent retains the masked result as `tool_result.retained` in the append of `child.settled`"
	specChildNoRead             = "the preview ends `… [truncated; full result unavailable]`"
	specChildClaude             = "on a backend with `OwnsLoop`, that turn reads every report that waits"
	specChildNoGoal             = "a child session has none"
	specChildAgent              = "`protocol.Session.agent` is the agent profile name that the child was spawned with"
	specChildWording            = "child-report-segment"
	specChildPinned             = "The segment is pinned: the model reads it at the same place in each later model call"
	specPinSlot                 = "the model reads it at the same place in each later model call"
	specPinReaders              = "it is in no history that a reader sees"
	specPinDrain                = "drain-two-messages"
	specPinInTurn               = "A compaction inside a turn settles the pins there at once."
	specPinRestart              = "Replay rebuilds a pin from the log, so a pin survives a restart."
	specPinCompact              = "A compaction moves every pin to the end of the history that it leaves"
	specPinPaired               = "so a pin never falls between a tool call and its result"
	specPinKept                 = "Replay rebuilds a pin from the log, so a pin survives a restart."
	specCrash                   = "Append `turn.ended{interrupted, crashed}`; keep the partial"
	specCrashQueue              = "The session then starts the next queued input, or waits for input when none is queued"
	specHandoffResume           = "A suspended turn has no open tool call, so the next owner resumes it automatically"
	specQueue                   = "(queue: next turn; steer: next item boundary)"
	specMCPText                 = "a call to a tool that is not there reads `no such tool available`"
	specGoalDeferred            = "no-deferred-goal"
	specCursor                  = "One per-session `seq` serves paging and SSE resume."
	specEventsRoute             = "GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream"
	specBoxGlobal               = "no-box-global-journal"
	specNoProvider              = "A model that no configured provider serves fails with `model_unavailable` at create and at a settings change; an unknown window never does."
	specUnknownWindow           = "else the 128000-token default with `WindowEstimated`"
	specThreshold               = "A setting at or below 0 is the default."
	specOverflowFails           = "When nothing can fold or the summary fails, the turn fails."
	specCompactResult           = "`Compact()` returns `protocol.Compacted`"
	specChildResend             = "later-send-not-refused"
	specEndTree                 = "stops the turn of each descendant that this runtime runs, as the tree interrupt does, and each of those turns ends with cause `ended`."
	specClearGoal               = "Stops the turn with `goal_cleared` while the goal is active"
	specWaiting                 = "| `waiting` | A turn ended `awaiting_input`; a request is open |"
	specWarm                    = "The session calls `Warm` once on create and on wake, fire-and-forget under the session context."
	specCompactOwned            = "A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`."
	specClaudeGauge             = "A Claude Code turn reads its context gauge and its cost from the `result` frame."
	specClaudeCompactPostTokens = "A `compact_boundary` with positive `post_tokens` sets the reading until a later assistant frame reports usage."
	specToolCallsAtOnce         = "The backend records the assistant item that holds a tool call when the `tool_use` frame arrives"
	specClaudeOnce              = "A failed Claude Code turn runs the CLI once and fails with the text of the `result` frame"
	specHistoryBridge           = "When another provider recorded a message after the newest message that this backend saw"
	specBackendState            = "Private backend state is a chain of `backend.state` records"
	specMirrorBlob              = "are appended as chunk blobs"
	specKeepOwned               = "A `keep_turns` below 1, or any `keep_turns` for a backend with `OwnsContext`, is `invalid_request`."
	specErrorText               = "The actor masks and bounds each error text that it writes to the log"
	specGoalFailed              = "An error the user must fix yields `failed`."
	specNoParkedGoal            = "no-deferred-goal"

	specStatusFollows = "status-line-follows"
	specBadFileSkip   = "A file that cannot be read, is empty, or is not UTF-8 is skipped"
	specBlankJoin     = "The runtime joins them with a blank line."
	specMCPNoLeak     = "An error that is not the server's own RPC error names a reason, never the endpoint URL or a response body."
	specRetainBound   = "`read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`."
	specNoReader      = "A turn whose allowed tools omit `read_tool_result` retains nothing, so a preview never names a tool that the model cannot call."
	specRetainIndex   = "Each compaction record holds an index of the newest 32 retained results in its `index` field, and the model of the harness loop reads it after the summary, so a handle stays reachable after its preview folds. A backend that owns the context compacts on its own and never reads that index, so after its compaction a handle is reachable only when its own summary keeps it."
	specFileCap       = "A file-size cap of 20 MiB bounds one file:"
	specToolImages    = "shows no blob."
	specProfileSkip   = "is skipped with a WARN log line"
	specProfileFail   = "Any other file that is not valid, such as one with no `name`, fails the load, and the error names the file"
	specProfileModel  = "`model` (a ref or an alias; omitted or `inherit` keeps the model of the parent)"
	specProfileColor  = "`color` is read and ignored"
	specProfileSwap   = "beside the built-in profiles, which a file of the same name replaces"
	specFileSkipped   = "A file that cannot be read, is empty, or is not UTF-8 is skipped, and so is a skill that is not valid or repeats a name."

	specListOrder     = "list in creation order"
	specStatusRoute   = "no-status-routes"
	specAnswerReceipt = "an answer replies 202 {seq, status}, a dismissal 204"

	specStopped        = "Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs"
	specInterrupt      = "An interrupt while the goal is `active` ends the goal"
	specSameBody       = "| Same id, same body | `200` with the original receipt |"
	specOtherBody      = "| Same id, other body | `409 input_conflict` |"
	specTurnMismatch   = "A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running."
	specRetryable      = "`ErrRetryable` (a 429 with `Retry-After`, a 5xx that outlasted the request retries, a transport error except one that declares itself permanent, a truncated stream, a content filter) calls the model again"
	specModelsRoute    = "GET    /models                                models and their capabilities"
	specHandoff        = "admit no new tool call, let running tools finish within the budget, append `turn.suspended`"
	specPromptOnce     = "It reads them once, when the session is created or opened, and sends them as `turn.Request.Instructions` on each model call."
	specRepeatName     = "fails the load the same way, and the error names both files."
	specMidTurnFails   = "the turn fails at its next model call"
	specTypedReceipt   = "A typed slash command answers the same way. Its receipt adds `command`, the newest status of the command"
	specCmdRepeat      = "A repeat of the input ID with the same line returns the newest status; another line, or an input ID of another input, is `input_conflict`."
	specCmdOps         = "`model`, `thinking`, and `tier` are `Update`"
	specCmdFailed      = "`failed` (the error text of a sentinel error"
	specCmdResult      = "Its result is the `protocol.Compacted` of `Compact`."
	specCmdInterrupt   = "`Open` records `interrupted` for each command that an earlier owner accepted and never finished"
	specCmdSwitch      = "builtin-commands-rows"
	specCmdUnsupport   = "A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`"
	specCmdMenuRoutes  = "A control command names its operation and `available_during_task`; the handler adds the route of the same operation, where one exists."
	specGoalOwnTurn    = "so the turn that calls `set` is not judged"
	specGoalPrompt     = "goal-evaluator-prompt"
	specGoalWording    = "goal-tool-wording"
	specGoalNoEval     = "`SetGoal` without it is an invalid request."
	specOverflowFolds  = "A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history."
	specLimitFails     = "fails the tool call with `engine: tree concurrency limit reached`"
	specTaskWithheld   = "A session at `max_task_depth` has no `task` tool."
	specGoalTranscript = "The evaluator transcript renders each part as a bracketed line"
	specGoalAdjust     = "A turn that calls `adjust` is judged on the new condition with no spent turn"
	specGoalAdjustPost = "a condition that `set` posted and that still waits is replaced by the adjusted one"
	specTaskWording    = "task-tool-wording"
	specReadOnlyKinds  = "The built-in `explore` and `plan` allow only the read-only file tools of both kinds of backend"
	specProfileKnown   = "a spawn keeps only the names of a profile that `known` accepts for the model of the child"
	specCrashMarker    = "[harness: this turn was interrupted by a process restart and could not complete]"
	specDismissed      = "A dismissal closes the call with an error result that says the user dismissed the question."
	specNoStart        = "That turn has no input; a dismissal starts none."
	specAnswerMap      = "A question takes an answer that maps each question to text."
	specProviderSwap   = "A settings change to a model of another provider dismisses it too."
	specRequestRoute   = "POST   /sessions/{id}/requests/{request}      {answer} | {dismiss}"
	specProcessUnknown = "An unknown name, or any name without a `WorkDir`, is `process_not_found`."
	specNoRoute        = "A path or method that no route serves answers 404 or 405 with `invalid_request`."
	specGitOracle      = "workspace-changes-rows"
	specBannerPrefix   = "each request of the session is a prefix of the next, also across processes and after a compaction in the middle of a turn"
	specPinsMove       = "A compaction moves the newest pin of each kind, the banner and each notice, to the end of the history that it leaves"
)

// Reasons that the session rows give.
const (
	specWindow         = "of the window of the session model, or of the window of the reading when the model reports none"
	specFailedSummary  = "A failed summary appends no `compaction.applied`, only a `context.measured` record with its usage when it has any, and the turn starts on the full history."
	specOpenStarts     = "The next owner thus runs the input that waited for a stopped summary"
	specInterruptTable = "| `Interrupt` | Stops the turn, and ends an active goal | Stops it, and ends an active goal | Stops it, and ends an active goal |"
	specCompactBusy    = "| `Compact` | `session_busy` | `session_busy` | `session_busy` |"
	specOverflowTwice  = "With no new input in the turn, a second overflow fails it"
	specFailedRuns     = "After any other failed turn, the next queued input runs, as after a completed turn."
	specExhaustedHolds = "Only `provider_exhausted` leaves the queue waiting, and `Open` follows the same rule"
	specExhaustedQueue = "queued inputs wait for the next input"
	specGoalImpossible = "`met` yields `achieved`, and `impossible` yields `failed`."
	specGoalBusy       = "`SetGoal` while a turn runs, or with queued input, starts nothing; the next turn that ends is the first one evaluated."
	specGoalClear      = "`ClearGoal` during a goal turn or its evaluation stops it with cause `goal_cleared` and returns after it ends."
	specGoalWithdraw   = "`SetGoal`, `ClearGoal`, each verdict, and each pause or failure withdraw the queued inputs with `source: goal`."
	specGoalRestart    = "An `active` goal on an idle session judges the last turn when the goal has not judged it"
	specNarrow         = "and the allowed tools of the parent narrowed by the profile"
	specAskRule        = "A session of any backend asks with the runtime tool `ask_user_question` when `Options.AskUserQuestion` is set and the session is not a child and has no active goal (see Tools)."
)

const (
	// reasonRetryHeld is the id of the reason that the rows with a server that fails its first initialize cite.
	reasonRetryHeld = "mcp-retry-held"
	// reasonWebLoopback is the id of the reason that the rows which fetch from a local server cite.
	reasonWebLoopback = "web-loopback-seam"
	// reasonWebProxy is the id of the reason that the rows which fetch through a proxy cite.
	reasonWebProxy = "web-proxy-seam"
	// reasonReportHeld is the id of the reason that the row with a held child report cites.
	reasonReportHeld = "child-report-held"
)

func sameAsServe() runtimeRow { return runtimeRow{kind: rowSame} }

// reGolden gives each reason or finding that decides a difference of the row.
func reGolden(cites ...string) runtimeRow { return runtimeRow{kind: rowRegolden, cites: cites} }

// runtimeOnly marks a row that only the runtime host runs, for the reason id that its determinism needs
// an in-process seam. The row has a contract golden that the runtime host matches.
func runtimeOnly(reason string) runtimeRow {
	return runtimeRow{kind: rowRuntimeOnly, cites: []string{reason}}
}

func deletedBy(spec string) runtimeRow { return runtimeRow{kind: rowDeleted, cites: []string{spec}} }

// pendingOn names the findings, phases, and reasons that a row waits for.
func pendingOn(cites ...string) runtimeRow { return runtimeRow{kind: rowPending, cites: cites} }

// runtimeRows is the disposition of each contract row on the runtime host.
var runtimeRows = withRows(deferredModelRows, mcpLiveRows, pluginLiveRows, codexChainRowKinds, codexPluginRowKinds, codexItemIDRowKinds, sessionTitleRows, claudeMCPRows, claudeChildReportRows, accountRows, midTurnCompactionRows, map[string]runtimeRow{
	"web_search_keeps_the_cached_prefix_of_anthropic_stable_across_calls_and_turns":                            sameAsServe(),
	"claudecode_web_search_keeps_the_system_prompt_stable_across_turns":                                        sameAsServe(),
	"codex_web_search_keeps_the_cached_prefix_stable_across_calls_and_turns":                                   sameAsServe(),
	"web_search_pause_turn_resume_that_hangs_up_records_the_finished_parts_and_fails_the_call":                 sameAsServe(),
	"web_search_titles_cannot_forge_engine_context_after_a_switch":                                             sameAsServe(),
	"web_search_is_offered_to_a_model_call_after_the_model_tool_switches_to_a_model_with_search":               sameAsServe(),
	"web_fetch_is_not_offered_unless_web_fetch_is_true":                                                        sameAsServe(),
	"web_fetch_is_offered_when_web_fetch_is_true":                                                              sameAsServe(),
	"web_fetch_call_without_web_fetch_true_is_an_unknown_tool":                                                 sameAsServe(),
	"web_fetch_refuses_private_loopback_and_link_local_addresses":                                              sameAsServe(),
	"web_fetch_refuses_other_schemes_credentials_and_bad_input":                                                sameAsServe(),
	"plugin_hook_denies_a_web_fetch_call_by_url":                                                               sameAsServe(),
	"web_fetch_turns_html_into_markdown":                                                                       runtimeOnly(reasonWebLoopback),
	"web_fetch_turns_html_into_text_or_returns_the_raw_source":                                                 runtimeOnly(reasonWebLoopback),
	"web_fetch_passes_json_through_and_names_a_failing_status":                                                 runtimeOnly(reasonWebLoopback),
	"web_fetch_parses_only_the_first_two_mebibytes_of_an_html_body":                                            runtimeOnly(reasonWebLoopback),
	"claudecode_system_prompt_maps_webfetch_to_web_fetch_when_the_turn_offers_it":                              sameAsServe(),
	"claudecode_system_prompt_keeps_webfetch_absent_when_the_turn_does_not_offer_web_fetch":                    sameAsServe(),
	"web_fetch_bounds_the_output_for_a_deeply_nested_page":                                                     runtimeOnly(reasonWebLoopback),
	"web_fetch_answers_a_binary_type_with_its_type_and_size":                                                   runtimeOnly(reasonWebLoopback),
	"web_fetch_treats_bytes_of_another_image_format_than_the_declared_one_as_binary":                           runtimeOnly(reasonWebLoopback),
	"web_fetch_returns_an_image_as_a_blob":                                                                     runtimeOnly(reasonWebLoopback),
	"web_fetch_cuts_a_body_at_ten_mebibytes_and_the_text_at_max_bytes":                                         runtimeOnly(reasonWebLoopback),
	"web_fetch_sends_a_get_with_the_harness_user_agent_and_no_cookie_or_credential":                            runtimeOnly(reasonWebLoopback),
	"web_fetch_follows_redirects_and_names_the_final_url":                                                      runtimeOnly(reasonWebLoopback),
	"web_fetch_checks_the_address_of_each_redirect":                                                            runtimeOnly(reasonWebLoopback),
	"web_fetch_sends_the_request_through_the_proxy_of_the_environment":                                         runtimeOnly(reasonWebProxy),
	"web_fetch_skips_the_proxy_for_a_host_in_no_proxy":                                                         runtimeOnly(reasonWebProxy),
	"web_fetch_refuses_a_host_that_resolves_to_a_private_address_before_it_uses_the_proxy":                     runtimeOnly(reasonWebProxy),
	"web_fetch_fetches_an_https_page_through_a_connect_tunnel_of_the_proxy":                                    runtimeOnly(reasonWebProxy),
	"web_fetch_checks_the_address_of_a_redirect_that_the_proxy_answers":                                        runtimeOnly(reasonWebProxy),
	"web_fetch_sends_a_name_that_does_not_resolve_to_the_proxy":                                                runtimeOnly(reasonWebProxy),
	"web_fetch_fails_when_the_proxy_is_unreachable_and_does_not_dial_the_host":                                 runtimeOnly(reasonWebProxy),
	"web_fetch_stops_after_five_redirects":                                                                     runtimeOnly(reasonWebLoopback),
	"claudecode_bridge_offers_web_fetch_when_web_fetch_is_true":                                                sameAsServe(),
	"claudecode_bridge_runs_web_fetch_through_the_engine":                                                      sameAsServe(),
	"claudecode_plugin_hook_denies_a_web_fetch_call_of_the_cli":                                                sameAsServe(),
	"web_search_pause_turn_resume_that_fails_records_the_finished_parts_and_fails_the_call":                    sameAsServe(),
	"web_search_pause_turn_resumes_inside_one_model_call":                                                      sameAsServe(),
	"web_search_pause_turn_after_three_resumes_records_the_finished_parts_and_fails_the_call":                  sameAsServe(),
	"web_search_that_failed_is_logged_with_its_error_and_replays_as_text":                                      sameAsServe(),
	"web_search_shows_its_query_in_the_goal_transcript":                                                        sameAsServe(),
	"claudecode_web_search_result_with_no_sources_gives_a_part_with_none":                                      sameAsServe(),
	"claudecode_history_tool_shows_the_query_of_a_web_search_of_another_provider":                              sameAsServe(),
	"web_search_on_anthropic_is_logged_and_replayed_to_the_same_provider":                                      sameAsServe(),
	"web_search_is_off_unless_the_config_turns_it_on":                                                          sameAsServe(),
	"web_search_on_openai_compat_sends_no_search":                                                              sameAsServe(),
	"web_search_of_anthropic_replays_as_text_after_a_switch_to_another_provider":                               sameAsServe(),
	"codex_web_search_is_logged_and_replayed_to_the_same_provider":                                             sameAsServe(),
	"codex_web_search_sends_no_hosted_tool_before_the_model_list_is_read":                                      sameAsServe(),
	"codex_web_search_is_off_unless_the_config_turns_it_on":                                                    sameAsServe(),
	"claudecode_web_search_is_offered_and_recorded_as_one_item":                                                sameAsServe(),
	"claudecode_web_search_call_without_the_option_fails_the_turn":                                             sameAsServe(),
	"claudecode_tool_search_call_and_its_result_do_not_fail_the_turn":                                          sameAsServe(),
	"claudecode_mcp_tool_loading_eager_turns_the_cli_tool_search_off":                                          sameAsServe(),
	"claudecode_mcp_tool_loading_lazy_turns_the_cli_tool_search_on":                                            sameAsServe(),
	"claudecode_mcp_tool_loading_auto_leaves_the_cli_tool_search_to_the_cli":                                   sameAsServe(),
	"claudecode_mcp_tool_loading_unset_leaves_the_cli_tool_search_to_the_cli":                                  sameAsServe(),
	"codex_minimal_on_a_model_without_it_clamps_up_to_low":                                                     reGolden(specUpdate),
	"thinking_off_on_a_model_that_lists_off_sends_thinking_disabled":                                           reGolden(specUpdate),
	"anthropic_adaptive_thinking_raises_max_tokens_for_the_level":                                              reGolden(specUpdate),
	"bedrock_mantle_claude_id_sends_the_effort_field":                                                          reGolden(specUpdate),
	"stored_thinking_stays_in_the_history_of_a_model_that_thinks_by_default":                                   sameAsServe(),
	"stored_thinking_stays_in_a_tool_round_of_a_model_that_thinks_by_default":                                  sameAsServe(),
	"stored_thinking_leaves_the_history_of_a_model_with_thinking_off_by_default":                               sameAsServe(),
	"stored_thinking_leaves_the_history_when_thinking_is_disabled_on_a_model_that_thinks_by_default":           sameAsServe(),
	"stored_thinking_leaves_the_history_when_a_budget_model_has_thinking_off":                                  sameAsServe(),
	"thinking_off_on_a_model_whose_thinking_can_be_disabled_sends_thinking_disabled":                           reGolden(specUpdate),
	"thinking_off_on_a_model_that_takes_between_tools_sends_between_tools":                                     reGolden(specUpdate),
	"stored_thinking_stays_in_a_tool_round_when_off_sends_between_tools":                                       sameAsServe(),
	"thinking_off_is_not_offered_by_a_model_that_always_thinks":                                                reGolden(specUpdate),
	"anthropic_effort_field_carries_xhigh_and_max":                                                             reGolden(specUpdate),
	"anthropic_effort_field_clamps_xhigh_for_a_model_without_it":                                               reGolden(specUpdate),
	"anthropic_budget_only_model_clamps_xhigh_and_max_to_the_high_budget":                                      reGolden(specUpdate),
	"unknown_model_clamps_xhigh_and_max_to_high":                                                               reGolden(specUpdate),
	"thinking_command_accepts_max_and_names_the_clamp":                                                         reGolden(specUpdate),
	"bifrost_clamps_xhigh_and_max_to_high":                                                                     reGolden(specUpdate),
	"codex_reasoning_effort_carries_xhigh_and_max":                                                             reGolden(specUpdate),
	"codex_clamps_a_level_that_the_model_does_not_list":                                                        reGolden(specUpdate),
	"claudecode_effort_flag_carries_xhigh_and_max":                                                             reGolden(specUpdate),
	"send_between_the_end_of_a_child_turn_and_its_report_reports_the_second_turn":                              runtimeOnly(reasonReportHeld),
	"send_to_a_running_child_whose_turn_ends_before_the_text_lands_reports_the_second_turn":                    runtimeOnly(reasonReportHeld),
	"mcp_connect_adds_tools_but_not_instructions":                                                              runtimeOnly(reasonRetryHeld),
	"skills_listed_sorted_and_read_through_read_file":                                                          reGolden(specBlankJoin),
	"skills_dirs_config_replaces_the_default_dir":                                                              reGolden(specBlankJoin),
	"skills_from_several_dirs_are_listed_sorted_by_name":                                                       reGolden(specBlankJoin),
	"skills_dirs_empty_list_disables_discovery":                                                                reGolden(specBlankJoin),
	"instructions_single_file_at_workdir":                                                                      reGolden(specBlankJoin),
	"instructions_chain_runs_root_to_workdir_and_stops_at_the_git_root":                                        reGolden(specBlankJoin),
	"instructions_outside_a_repository_read_only_the_workdir_file":                                             reGolden(specBlankJoin),
	"instructions_oversize_with_headings_become_an_outline_read_by_range":                                      reGolden(specBlankJoin),
	"instructions_mode_full_keeps_the_truncation_marker":                                                       reGolden(specBlankJoin),
	"instructions_chain_over_four_times_the_cap_drops_the_middle_files_nearest_the_root":                       reGolden(specBlankJoin),
	"instructions_first_section_over_the_cap_is_cut_with_the_marker_and_the_rest_is_outlined":                  reGolden(specBlankJoin),
	"instructions_outline_drops_the_teasers_over_its_budget":                                                   reGolden(specBlankJoin),
	"instructions_mode_full_from_the_environment_keeps_the_truncation_marker":                                  reGolden(specBlankJoin),
	"instructions_mode_other_than_full_keeps_the_outline":                                                      reGolden(specBlankJoin),
	"instructions_oversize_without_headings_keep_the_truncation_marker":                                        reGolden(specBlankJoin),
	"instructions_300_kb_file_with_headings_keeps_the_sections_that_fit_the_default_cap_and_outlines_the_rest": reGolden(specBlankJoin),
	"instructions_300_kb_file_without_headings_is_cut_at_the_default_cap_with_the_marker":                      reGolden(specBlankJoin),
	"instructions_300_kb_file_with_one_huge_first_section_is_cut_with_the_marker_and_the_rest_is_outlined":     reGolden(specBlankJoin),
	"instructions_negative_max_bytes_keeps_the_whole_file":                                                     reGolden(specBlankJoin),
	"instructions_false_in_config_injects_nothing":                                                             reGolden(specBlankJoin),
	"instructions_path_in_config_replaces_discovery":                                                           reGolden(specBlankJoin),
	"system_segments_order_append_layers_then_instructions_then_skills":                                        reGolden(specBlankJoin),
	"instructions_empty_file_fails_the_turn":                                                                   reGolden(specFileSkipped, specBadFileSkip, specView),
	"instructions_empty_and_invalid_files_in_a_chain_fail_the_turn":                                            reGolden(specFileSkipped, specBadFileSkip, specView, specBlankJoin),
	"skill_with_an_uppercase_name_fails_the_turn":                                                              reGolden(specFileSkipped, specBadFileSkip, specBlankJoin),
	"skill_named_unlike_its_directory_fails_the_turn":                                                          reGolden(specFileSkipped, specBadFileSkip, specView),
	"task_profiles_skip_bad_files":                                                                             reGolden(specProfileSkip, specProfileFail, specOneResult, specView),
	"task_profile_file_with_model_inherit_and_color_is_a_profile":                                              reGolden(specProfileModel, specProfileColor, specTaskInputs, specChildReport),
	"task_profile_file_replaces_a_built_in_profile":                                                            reGolden(specProfileSwap, specTaskInputs, specChildReport),
	"two_slow_tool_calls_of_one_message_overlap_and_log_in_call_order":                                         sameAsServe(),
	"interrupt_cancels_every_running_tool_call_of_a_batch":                                                     sameAsServe(),
	"interrupt_records_the_result_of_a_call_that_finished_beside_a_held_one":                                   sameAsServe(),
	"retained_results_of_one_batch_keep_their_own_handles":                                                     sameAsServe(),
	"calls_on_one_file_run_in_call_order_within_a_batch":                                                       sameAsServe(),
	"plugin_hooks_run_for_each_call_of_a_batch":                                                                sameAsServe(),
	"file_tools_size_cap":                                                                        reGolden(specFileCap),
	"bash_output_and_exit_status":                                                                sameAsServe(),
	"read_file_returns_an_image":                                                                 reGolden(specToolImages),
	"a_tool_result_image_reaches_the_model_after_a_restart":                                      sameAsServe(),
	"a_missing_tool_result_image_leaves_the_text_for_the_model":                                  sameAsServe(),
	"write_guard_belongs_to_one_session":                                                         sameAsServe(),
	"mcp_tool_action_refusals":                                                                   sameAsServe(),
	"mcp_refused_call_hides_the_response_body":                                                   reGolden(specMCPNoLeak, specMCPText),
	"mcp_select_of_a_down_server_is_pending":                                                     sameAsServe(),
	"mcp_search_ranks_the_tools":                                                                 sameAsServe(),
	"tool_result_retention_keeps_a_preview_and_reads_it_back":                                    sameAsServe(),
	"tool_result_over_the_session_budget_keeps_a_preview_with_a_notice":                          sameAsServe(),
	"read_tool_result_bounds_the_default_budget":                                                 reGolden(specRetainBound),
	"tool_result_handles_continue_across_turns_and_a_restart":                                    reGolden(specRetainIndex, specCompactResult),
	"tool_result_retention_keeps_the_plugin_hooked_result":                                       sameAsServe(),
	"a_child_without_read_tool_result_retains_nothing":                                           reGolden(specNoReader, specTaskInputs, specChildReport),
	"auto_compact_on_threshold":                                                                  reGolden(specView, specPinsMove),
	"bifrost_429_then_ok":                                                                        reGolden(specItems, specView),
	"bifrost_context_overflow":                                                                   reGolden(specView, specOverflowFails),
	"bifrost_goal_met_first_turn":                                                                reGolden(specItems),
	"bifrost_goal_not_met_then_met":                                                              reGolden(specItems),
	"bifrost_prompt_attachments":                                                                 reGolden(specItems),
	"bifrost_reasoning_and_effort":                                                               reGolden(specItems, specUpdate),
	"bifrost_text_reply":                                                                         reGolden(specItems),
	"bifrost_tool_error_marker":                                                                  reGolden(specItems),
	"bifrost_tool_round_trip":                                                                    reGolden(specItems),
	"bifrost_two_tool_calls_one_turn":                                                            reGolden(specItems, specOneResult),
	"bootstrap_cold_then_resident_windows":                                                       reGolden(specMessages, specBootstrapGone, specErrors),
	"bootstrap_cold_window_after_kill":                                                           reGolden(specMessages, specBootstrapGone, specErrors),
	"builtin_commands_run_and_record":                                                            reGolden(specCmdSwitch, specTypedReceipt, specCmdMenuRoutes),
	"busy_deferred_goal_with_max_turns":                                                          deletedBy(specGoalDeferred),
	"child_crash_recovered":                                                                      reGolden(specView, specCrash, specChildReport, specChildAgent),
	"child_crash_reaches_a_busy_parent":                                                          reGolden(specView, specCrash, specChildReport, specChildCrash, specChildLost, specChildWording, specChildAgent),
	"child_usage_limit_delivered":                                                                reGolden(specTaskInputs, specChildReport, specChildReason, specChildNoGoal, specView, specChildAgent),
	"child_error_delivered":                                                                      reGolden(specTaskInputs, specChildReport, specChildReason, specChildNoGoal, specView, specChildAgent),
	"child_report_reaches_a_busy_parent_at_the_tool_boundary":                                    reGolden(specChildReport, specChildWording),
	"child_error_reaches_a_busy_parent_at_the_tool_boundary":                                     reGolden(specChildReport, specChildReason, specChildWording),
	"child_usage_limit_reaches_a_busy_parent":                                                    reGolden(specChildReport, specChildReason, specChildWording),
	"child_report_to_a_busy_parent_keeps_its_slot_in_the_next_turn":                              reGolden(specChildReport, specChildWording, specChildPinned, specPinSlot),
	"child_report_to_a_busy_parent_stays_through_a_compaction":                                   reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult, specPinsMove),
	"prompt_and_child_report_in_one_drain_are_two_messages":                                      reGolden(specChildReport, specChildWording, specChildPinned, specPinDrain),
	"task_log_of_a_child_shows_no_pinned_report":                                                 reGolden(specTaskInputs, specChildReport, specChildWording, specChildPinned, specPinReaders, specItems, specOneResult),
	"child_report_to_a_busy_parent_after_the_cut_of_a_compaction":                                reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult, specPinsMove),
	"child_report_to_a_busy_parent_folded_with_a_long_kept_tail":                                 reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult, specPinPaired, specPinsMove),
	"child_report_to_a_busy_parent_stays_through_an_in_turn_compaction":                          reGolden(specChildReport, specChildWording, specChildPinned, specPinInTurn, specOverflowFolds, specPinsMove),
	"child_report_to_a_busy_parent_survives_a_restart":                                           reGolden(specChildReport, specChildWording, specChildPinned, specPinRestart, specPinKept),
	"child_rate_limit_reaches_a_busy_parent":                                                     reGolden(specChildReport, specChildReason, specChildWording),
	"child_result_within_the_byte_limit_reaches_a_busy_parent":                                   reGolden(specChildReport, specChildLong, specChildWording),
	"child_usage_limit_with_a_long_hint_reaches_a_busy_parent":                                   reGolden(specChildReport, specChildReason, specChildHint, specChildWording),
	"child_long_result_handle_reads_back":                                                        reGolden(specChildReport, specChildLong, specChildHandle, specChildWording),
	"claudecode_long_child_result_has_a_readable_handle":                                         sameAsServe(),
	"child_long_error_reaches_a_busy_parent":                                                     reGolden(specChildReport, specChildReason, specChildBound, specChildWording),
	"child_long_result_reaches_a_busy_parent":                                                    reGolden(specChildReport, specChildLong, specChildWording),
	"claudecode_compact_delegated":                                                               reGolden(specView, specCompactOwned, specCompactResult, specClaudeGauge, specClaudeCompactPostTokens, specRestartOpens),
	"claudecode_child_report_with_no_tool_call_in_flight_waits_for_the_next_turn":                sameAsServe(),
	"claudecode_child_reports_with_no_tool_call_in_flight_share_the_next_turn":                   sameAsServe(),
	"claudecode_queued_prompt_and_child_report_share_a_turn":                                     reGolden(specQueue, specChildReport, specChildClaude),
	"claudecode_cli_gets_the_tools_of_the_engine_bridge":                                         sameAsServe(),
	"claudecode_bridge_marks_exactly_the_read_only_tools_with_read_only_hint":                    sameAsServe(),
	"claudecode_bridge_loads_ask_user_question_and_the_file_and_shell_tools_upfront":             sameAsServe(),
	"question_tool_description_says_to_ask_with_the_tool_and_not_in_text":                        sameAsServe(),
	"claudecode_bridge_passes_the_read_only_hint_of_an_mcp_tool_through":                         sameAsServe(),
	"claudecode_bridge_model_tool_offers_deferred_set_and_status":                                sameAsServe(),
	"claudecode_bridge_defers_set_on_the_model_tool":                                             sameAsServe(),
	"claudecode_large_read_file_result_is_a_preview_with_a_handle_that_read_tool_result_reads":   sameAsServe(),
	"claudecode_session_info_lists_the_tools_that_the_call_offered":                              sameAsServe(),
	"claudecode_system_prompt_names_the_harness_tools_and_lists_the_skills":                      sameAsServe(),
	"claudecode_skills_dirs_config_changes_the_skill_list":                                       sameAsServe(),
	"claudecode_session_info_system_leaves_out_the_mcp_segment_that_the_cli_never_got":           sameAsServe(),
	"claudecode_append_system_prompt_over_the_argument_limit_reaches_the_cli":                    sameAsServe(),
	"claudecode_session_info_lists_the_skills":                                                   sameAsServe(),
	"claudecode_bridge_image_result_reaches_the_log_and_a_model_api_replay":                      sameAsServe(),
	"claudecode_built_in_tool_that_returns_a_result_fails_the_turn":                              sameAsServe(),
	"claudecode_refused_built_in_call_is_a_failed_tool_call_and_the_turn_goes_on":                sameAsServe(),
	"claudecode_refused_call_to_an_unknown_tool_name_is_a_failed_tool_call_and_the_turn_goes_on": sameAsServe(),
	"claudecode_refused_web_search_without_the_option_is_a_failed_call_and_a_pair":               sameAsServe(),
	"claudecode_built_in_tool_that_ran_and_failed_fails_the_turn":                                sameAsServe(),
	"claudecode_init_frame_with_a_built_in_tool_fails_the_turn":                                  sameAsServe(),
	"claudecode_parallel_edit_file_calls_on_one_path_lose_no_edit":                               sameAsServe(),
	"task_profile_maps_claude_code_tool_names_to_harness_tools":                                  sameAsServe(),
	"claudecode_bridge_serves_a_text_item_for_a_tool_with_no_output":                             sameAsServe(),
	"claudecode_plugin_hook_denies_a_bash_call_of_the_cli":                                       sameAsServe(),
	"claudecode_configured_mcp_tool_runs_through_the_harness_endpoint":                           sameAsServe(),
	"claudecode_endpoint_initialize_carries_the_mcp_server_instructions":                         sameAsServe(),
	"claudecode_context_window_from_model_usage":                                                 reGolden(specView, specClaudeGauge),
	"claudecode_compact_reads_post_tokens":                                                       reGolden(specView, specClaudeGauge, specClaudeCompactPostTokens),
	"claudecode_compact_post_tokens_yields_to_assistant_usage":                                   reGolden(specView, specClaudeGauge, specClaudeCompactPostTokens),
	"claudecode_compact_keeps_the_window":                                                        reGolden(specView, specClaudeGauge, specClaudeCompactPostTokens),
	"claudecode_cli_exit_runs_once":                                                              reGolden(specView, specClaudeOnce),
	"claudecode_error_result_fails_turn":                                                         reGolden(specView, specClaudeOnce, specClaudeGauge),
	"claudecode_error_during_execution_pauses_a_goal_and_keeps_its_errors":                       sameAsServe(),
	"claudecode_credential_refusal_in_errors_fails_a_goal":                                       sameAsServe(),
	"claudecode_history_bridge_after_native_turn":                                                reGolden(specUpdate, specClaudeGauge),
	"claudecode_compaction_then_a_harness_model_reads_the_banner_and_the_summary":                reGolden(specUpdate, specClaudeCompactSummary),
	"claudecode_parallel_calls_then_a_chat_model_gets_a_valid_request":                           reGolden(specToolCallsAtOnce),
	"claudecode_interrupt_mid_turn":                                                              reGolden(specView),
	"claudecode_question_dismissed_by_compact":                                                   reGolden(specWaiting, specView, specCompactResult, specClaudeGauge),
	"claudecode_question_dismissed_by_next_prompt":                                               reGolden(specWaiting, specView, specClaudeGauge),
	"claudecode_question_parks_then_answer_resumes":                                              reGolden(specWaiting, specView, specAnswerReceipt, specClaudeGauge),
	"claudecode_question_unknown_call_id_conflicts":                                              reGolden(specWaiting, specView, specErrors, specAnswerReceipt, specClaudeGauge),
	"claudecode_text_and_thinking_stream_as_deltas_before_the_item":                              sameAsServe(),
	"claudecode_retried_reply_with_a_tool_call_alone_completes_under_a_new_item":                 reGolden(specDiscard),
	"claudecode_tool_result_between_the_deltas_and_the_frame_of_a_block_keeps_the_live_item":     sameAsServe(),
	"claudecode_retried_reply_starts_a_new_live_item":                                            reGolden(specDiscard),
	"claudecode_prompt_attachments":                                                              sameAsServe(),
	"claudecode_queued_prompt_injected_mid_turn":                                                 reGolden(specView, specClaudeGauge),
	"claudecode_steer_held_during_text_joins_at_the_next_tool_boundary":                          sameAsServe(),
	"claudecode_cli_exit_before_answering_a_steer_fails_the_turn":                                sameAsServe(),
	"claudecode_interrupt_while_awaiting_a_steer_answer_interrupts_the_turn":                     sameAsServe(),
	"claudecode_steer_during_final_text_runs_as_the_next_turn":                                   sameAsServe(),
	"claudecode_steer_the_cli_queued_past_its_result_answers_in_the_same_turn":                   sameAsServe(),
	"claudecode_rate_limit_event_reaches_subscription_usage":                                     reGolden(specView, specClaudeGauge),
	"claudecode_rejected_usage_limit_ends_the_turn_provider_exhausted":                           reGolden(specView),
	"claudecode_resume_across_turns":                                                             sameAsServe(),
	"claudecode_resume_survives_restart":                                                         sameAsServe(),
	"claudecode_thinking_block_is_reasoning":                                                     reGolden(specExternal, specView, specClaudeGauge),
	"claudecode_turn_text_and_tool":                                                              reGolden(specView, specClaudeGauge),
	"codex_http_mcp_tool_schema_is_sanitized":                                                    reGolden(specItems),
	"codex_http_reasoning_replays_on_tool_round_trip":                                            reGolden(specItems),
	"codex_http_sse_text_turn":                                                                   reGolden(specItems),
	"codex_http_sse_tool_round_trip_resends_history":                                             reGolden(specItems),
	"codex_ws_chain_miss_resends_full_history":                                                   reGolden(specItems, specWarm),
	"codex_ws_chains_two_turns":                                                                  reGolden(specItems, specWarm),
	"codex_ws_drop_mid_turn_resends_full_history":                                                reGolden(specView, specItems),
	"codex_ws_effort_sets_reasoning_effort":                                                      reGolden(specView, specItems, specUpdate),
	"codex_ws_prewarm_warms_first_turn":                                                          reGolden(specItems, specWarm),
	"codex_ws_reasoning_chains_tool_round_trip":                                                  reGolden(specItems, specWarm),
	"codex_ws_refused_falls_back_to_http":                                                        reGolden(specItems, specWarm),
	"codex_ws_tool_round_trip":                                                                   reGolden(specItems, specWarm),
	"codex_ws_uncoded_chain_miss_resends_full_history":                                           reGolden(specItems, specWarm),
	"codex_ws_usage_frame_reaches_session":                                                       reGolden(specView, specItems),
	"codex_http_usage_headers_reach_session":                                                     reGolden(specView, specItems),
	"compact_summary_cut_at_max_tokens_is_kept":                                                  sameAsServe(),
	"compact_logs_the_plain_summary_and_the_model_reads_it_with_the_banner":                      sameAsServe(),
	"compact_manual":                                          reGolden(specMessages, specBootstrapGone, specErrors, specPinsMove),
	"compact_survives_restart":                                reGolden(specMessages, specBootstrapGone, specErrors, specRestartOpens),
	"context_overflow":                                        reGolden(specView, specOverflowFails),
	"deferred_goal_judges_finished_turn":                      deletedBy(specGoalDeferred),
	"deferred_goal_with_max_turns":                            deletedBy(specGoalDeferred),
	"driver_child_send_and_cancel":                            reGolden(specTaskInputs, specChildNoGoal, specReceipt, specView, specChildAgent),
	"driver_clean_restart":                                    reGolden(specView, specRestartOpens),
	"driver_compact":                                          reGolden(specView, specCompactResult),
	"driver_queue_goal_and_end":                               reGolden(specView, specReceipt, specClearGoal),
	"driver_resume_streams":                                   reGolden(specCursor, specBoxGlobal),
	"driver_settings_and_reads":                               reGolden(specMessages, specBootstrapGone, specErrors),
	"end_then_send_runs_no_report_of_the_stopped_child":       reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal),
	"end_then_open_before_the_child_turn_ends_runs_no_report": reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal),
	"end_idle_parent_cancels_running_child":                   reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal, specChildAgent),
	"end_session_semantics":                                   reGolden(specView, specErrors, specReceipt),
	"enqueue_joins_the_turn_at_the_tool_boundary":             sameAsServe(),
	"enqueue_while_busy_runs_after":                           sameAsServe(),
	"file_tools_read_edges":                                   sameAsServe(),
	"file_tools_roundtrip":                                    sameAsServe(),
	"file_tools_search_edges":                                 sameAsServe(),
	"file_tools_write_edit_guards":                            sameAsServe(),
	"goal_busy_send_is_queued":                                reGolden(specView),
	"goal_cleared_before_first_turn":                          deletedBy(specGoalDeferred),
	"goal_exhausts_max_turns":                                 reGolden(specView),
	"goal_met_first_turn":                                     sameAsServe(),
	"goal_not_met_then_met":                                   sameAsServe(),
	"goal_provider_exhausted_parks":                           deletedBy(specGoalDeferred),
	"goal_update_deferred_goal_waits_for_first_turn":          deletedBy(specGoalDeferred),
	"goal_update_while_busy":                                  reGolden(specView),
	"banner_and_history_prefix_stay_the_same_after_a_restart": sameAsServe(),
	"banner_and_history_prefix_stay_the_same_after_a_compaction_and_a_restart": sameAsServe(),
	"banner_names_the_created_time_of_the_session":                             sameAsServe(),
	"history_survives_clean_restart":                                           sameAsServe(),
	"interrupt_drops_unfinished_text_then_queue_continues":                     sameAsServe(),
	"interrupt_idle_is_noop":                                                   sameAsServe(),
	"journal_pages_follow_cursor":                                              reGolden(specCursor, specEventsRoute),
	"kill_mid_turn_then_continue":                                              sameAsServe(),
	"mcp_eager_defers_the_tools_past_the_schema_budget":                        sameAsServe(),
	"mcp_select_stops_at_the_byte_cap_and_names_the_tools_left_out":            sameAsServe(),
	"mcp_unset_loading_defers_every_tool_over_the_schema_budget":               sameAsServe(),
	"mcp_unset_loading_keeps_the_tools_under_the_schema_budget":                sameAsServe(),
	"mcp_eager_lists_namespaced_tools":                                         sameAsServe(),
	"mcp_http_sse_reply":                                                       sameAsServe(),
	"mcp_instructions_in_system_prompt":                                        sameAsServe(),
	"mcp_call_of_a_deferred_tool_goes_through_the_plugin_guard":                sameAsServe(),
	"mcp_lazy_native_call_of_a_deferred_tool_runs_it":                          sameAsServe(),
	"mcp_lazy_search_select_then_call":                                         sameAsServe(),
	"mcp_lazy_select_reports_each_name":                                        sameAsServe(),
	"mcp_non_text_results_become_text_and_blobs":                               reGolden(specToolImages),
	"mcp_paged_tool_list_is_merged":                                            sameAsServe(),
	"mcp_per_server_tool_loading_overrides_global":                             sameAsServe(),
	"mcp_resources_list_and_read":                                              reGolden(specMCPText),
	"mcp_resources_paged_list_is_merged":                                       sameAsServe(),
	"mcp_server_lost_mid_session_hides_the_endpoint":                           reGolden(specMCPText),
	"mcp_status_reports_connected_and_unavailable_servers":                     sameAsServe(),
	"mcp_stdio_server_call":                                                    sameAsServe(),
	"mcp_stdio_server_starts_in_configured_dir":                                sameAsServe(),
	"mcp_tool_call_result":                                                     sameAsServe(),
	"mcp_tool_error_and_rpc_error_reach_model":                                 reGolden(specMCPText),
	"mcp_two_servers_share_a_tool_name":                                        sameAsServe(),
	"pins_after_a_compaction_keep_each_request_a_prefix_of_the_next":           reGolden(specMessages),
	"bifrost_pins_after_a_compaction_keep_each_request_a_prefix_of_the_next":   reGolden(specMessages),
	"mcp_notice_then_compaction_keeps_a_call_paired":                           runtimeOnly(reasonRetryHeld),
	"bifrost_mcp_notice_then_compaction_keeps_a_call_paired":                   runtimeOnly(reasonRetryHeld),
	"mcp_unavailable_at_start_then_connect":                                    runtimeOnly(reasonRetryHeld),
	"mcp_notice_and_history_prefix_stay_the_same_after_a_restart":              runtimeOnly(reasonRetryHeld),
	"mcp_unavailable_connect_fails_with_classified_reason":                     reGolden(specMCPText),
	"messages_page_after_compaction":                                           reGolden(specMessages, specBootstrapGone, specErrors),
	"messages_page_windows":                                                    reGolden(specMessages, specBootstrapGone, specErrors),
	"model_tool_false_removes_the_model_tool":                                  sameAsServe(),
	"model_tool_reports_lists_and_switches_the_model":                          sameAsServe(),
	"one_tool_round_trip":                                                      sameAsServe(),
	"openai_key_http_sse_text_turn":                                            reGolden(specItems),
	"persisted_queue_dispatches_after_deferred_arm":                            deletedBy(specGoalDeferred),
	"plugin_after_hook_sees_output":                                            sameAsServe(),
	"plugin_before_hook_timeout_over_default_is_honored":                       sameAsServe(),
	"plugin_before_hook_rewrites_and_blocks":                                   sameAsServe(),
	"plugin_chat_params_hook_sets_the_cap_and_sampling_of_a_model_call":        sameAsServe(),
	"plugin_shell_env_hook_sets_the_environment_of_a_bash_command":             sameAsServe(),
	"plugin_boxes_style_command_and_dir":                                       sameAsServe(),
	"plugin_crash_mid_call_session_continues":                                  reGolden(specView),
	"plugin_event_and_after_hook_payloads":                                     sameAsServe(),
	"plugin_system_segment_in_every_request":                                   sameAsServe(),
	"plugin_user_guard_sees_the_project_rewrite":                               sameAsServe(),
	"plugin_project_plugins_alone_keep_their_chain":                            sameAsServe(),
	"plugin_flagged_before_hook_denies_when_the_plugin_exits":                  sameAsServe(),
	"plugin_flagged_before_hook_denies_on_an_error":                            sameAsServe(),
	"plugin_flagged_before_hook_denies_at_the_deadline":                        sameAsServe(),
	"plugin_unflagged_before_hook_fails_open":                                  sameAsServe(),
	"plugin_system_transform_reads_session_messages":                           sameAsServe(),
	"plugin_session_messages_carry_attachments":                                sameAsServe(),
	"plugin_tool_calls_an_mcp_server_through_the_host":                         sameAsServe(),
	"plugin_mcp_call_to_an_unknown_server_fails":                               sameAsServe(),
	"plugin_mcp_call_reports_a_tool_error":                                     sameAsServe(),
	"plugin_tool_generates_through_the_host":                                   sameAsServe(),
	"plugin_tool_blob_output_reaches_the_model":                                sameAsServe(),
	"plugin_generate_with_an_unserved_model_fails":                             sameAsServe(),
	"plugin_generate_rejects_a_negative_max_tokens":                            sameAsServe(),
	"plugin_generate_resolves_an_alias":                                        sameAsServe(),
	"plugin_generate_serves_a_model_that_runs_its_own_agent_loop":              sameAsServe(),
	"plugin_exit_during_generate_ends_the_call":                                sameAsServe(),
	"plugin_tools_listed_and_run":                                              reGolden(specView),
	"provider_429_then_ok":                                                     reGolden(specView),
	"provider_usage_limit_fails_turn":                                          reGolden(specView),
	"provider_5xx_then_ok":                                                     reGolden(specView),
	"provider_error_text_is_masked_and_bounded":                                reGolden(specView, specErrorText, specGoalFailed, specNoParkedGoal),
	"queue_delete_while_busy":                                                  reGolden(specView),
	"queue_survives_clean_restart_then_delete":                                 reGolden(specView, specHandoffResume),
	"queue_survives_clean_restart_then_drains_with_next_prompt":                reGolden(specQueue, specHandoffResume, specView),
	"queued_input_runs_after_kill":                                             reGolden(specCrash, specCrashQueue),
	"queued_input_survives_kill":                                               deletedBy(specCrashQueue),
	"queued_prompt_runs_before_deferred_auto_arm":                              deletedBy(specGoalDeferred),
	"replay_after_kill_full_transcript":                                        reGolden(specMessages, specBootstrapGone, specErrors),
	"send_to_child_and_cancel_tree":                                            reGolden(specChildResend, specChildNoGoal, specReceipt, specTaskInputs, specView, specChildAgent),
	"settings_model_change_reaches_the_next_model_call_of_a_turn":              reGolden(specUpdate),
	"session_info_reports_the_session":                                         reGolden(specStatusFollows),
	"session_info_reports_what_the_session_loaded":                             reGolden(specStatusFollows),
	"session_info_reports_the_plugin_and_its_system_segment":                   reGolden(specStatusFollows),
	"create_with_an_unknown_effort_is_invalid_request":                         reGolden(specCreateEffort, specErrors, specView, specListOrder),
	"session_settings_validation_and_persistence":                              reGolden(specNoProvider, specErrors, specUpdate, specView, specRestartOpens),
	"sse_resume_after_kill":                                                    reGolden(specCursor, specBoxGlobal, specRestartOpens),
	"sse_resume_cursor":                                                        reGolden(specCursor, specBoxGlobal),
	"list_sessions_in_creation_order":                                          reGolden(specView, specListOrder, specRestartOpens),
	"status_and_list_cold_after_restart":                                       reGolden(specView, specListOrder, specStatusRoute, specRestartOpens),
	"steer_joins_the_turn_at_the_tool_boundary":                                sameAsServe(),
	"stream_stall":                                      reGolden(specView),
	"task_child_result_reaches_parent":                  reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_spawn_runs_the_child_on_its_model_and_effort": reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView, specChildAgent),
	"text_reply":                                        sameAsServe(),
	"tool_error_reaches_model":                          sameAsServe(),
	"two_tool_calls_one_turn":                           reGolden(specOneResult),
	"two_turns_keep_history":                            sameAsServe(),
	"usage_survives_a_mid_turn_restart":                 reGolden(specView, specHandoffResume),

	"end_is_refused_while_a_typed_command_runs":                                             reGolden(specTypedReceipt, specErrors, specReceipt),
	"a_kill_interrupts_an_unfinished_command":                                               reGolden(specTypedReceipt, specCmdRepeat, specCmdInterrupt, specReceipt),
	"banner_stays_after_the_results_when_a_compaction_follows_two_calls":                    reGolden(specMessages),
	"banner_holds_its_place_when_a_turn_compacts_in_the_middle":                             reGolden(specOverflowFolds, specBannerPrefix, specView),
	"codex_http_responses_without_a_tool_call_end_the_turn":                                 sameAsServe(),
	"codex_http_truncated_responses_are_retried":                                            sameAsServe(),
	"codex_service_tier_reaches_the_request":                                                reGolden(specView, specItems, specUpdate),
	"codex_settings_switch_to_another_provider_keeps_the_history":                           reGolden(specView, specItems, specUpdate),
	"commands_menu_lists_builtin_and_prompt_commands":                                       reGolden(specCmdMenuRoutes, specCmdUnsupport),
	"create_checks_the_provider":                                                            reGolden(specNoProvider, specUnknownWindow, specErrors, specInvalidReason, specView),
	"create_names_no_built_in_openrouter_provider":                                          reGolden(specNoProvider, specErrors, specView),
	"create_without_a_model_takes_the_default_model":                                        reGolden(specView),
	"create_without_a_model_ignores_claude_on_path_and_a_codex_login":                       reGolden(specView),
	"create_takes_an_unknown_model_with_a_configured_window":                                reGolden(specView),
	"goal_tool_actions_report_and_refuse":                                                   reGolden(specView, specGoalTranscript),
	"goal_tool_adjust_after_set_runs_the_adjusted_condition":                                reGolden(specView, specGoalOwnTurn, specGoalAdjustPost),
	"goal_tool_adjust_keeps_the_turn_limit":                                                 reGolden(specView, specGoalAdjust),
	"goal_tool_refusals_copy_the_engine_wording":                                            reGolden(specView, specGoalWording),
	"goal_tool_set_runs_the_condition_as_its_own_turn":                                      reGolden(specView, specGoalOwnTurn, specGoalTranscript),
	"input_receipts_and_conflicts":                                                          reGolden(specReceipt, specSameBody, specOtherBody, specTurnMismatch, specErrors),
	"instructions_are_read_when_the_session_starts":                                         reGolden(specPromptOnce),
	"interrupt_cuts_a_running_tool_then_queue_continues":                                    reGolden(specStopped, specInterrupt),
	"interrupt_during_retry_backoff_ends_the_turn":                                          reGolden(specView, specStopped),
	"models_lists_the_models_that_a_provider_entry_names":                                   reGolden(specModelsRoute, specView),
	"models_lists_the_configured_providers":                                                 reGolden(specModelsRoute, specView),
	"no_goal_evaluator_means_no_goal":                                                       reGolden(specGoalNoEval, specErrors, specCmdFailed, specView),
	"plugin_inventory_reports_not_spawned_then_running":                                     reGolden(specView),
	"plugin_sees_the_model_of_each_call":                                                    reGolden(specUpdate),
	"plugin_segment_that_changes_keeps_only_its_newest_version_after_a_compaction":          sameAsServe(),
	"restart_lets_a_running_tool_finish_and_cuts_the_next_alone_call":                       reGolden(specView, specHandoff),
	"bifrost_max_tokens_cut_response_is_retried":                                            sameAsServe(),
	"max_tokens_cut_response_fails_the_turn_after_stream_max_retries":                       sameAsServe(),
	"max_tokens_cut_tool_call_is_retried_without_running":                                   sameAsServe(),
	"provider_5xx_is_retried_by_the_request_before_the_turn_retries":                        sameAsServe(),
	"request_and_stream_retries_multiply":                                                   sameAsServe(),
	"provider_429_is_not_retried_by_the_request":                                            sameAsServe(),
	"stream_retries_default_to_five":                                                        sameAsServe(),
	"stream_retries_stop_after_the_default_of_five":                                         sameAsServe(),
	"provider_connection_failure_is_retried":                                                sameAsServe(),
	"provider_refusal_is_retried_as_a_content_filter":                                       sameAsServe(),
	"provider_refusal_fails_the_turn_after_stream_max_retries":                              sameAsServe(),
	"bifrost_content_filter_response_is_retried":                                            sameAsServe(),
	"bifrost_5xx_is_retried_by_the_request":                                                 sameAsServe(),
	"codex_http_incomplete_content_filter_is_retried":                                       sameAsServe(),
	"codex_http_incomplete_with_another_reason_is_retried":                                  sameAsServe(),
	"codex_http_incomplete_fails_the_turn_after_stream_max_retries":                         sameAsServe(),
	"provider_429_without_retry_after_is_not_retried":                                       sameAsServe(),
	"codex_login_headers_reach_the_request":                                                 sameAsServe(),
	"codex_login_models_list_the_models_of_the_backend":                                     sameAsServe(),
	"codex_login_models_list_of_the_entry_makes_no_catalog_request":                         sameAsServe(),
	"codex_login_websocket_dial_carries_the_login":                                          sameAsServe(),
	"codex_login_near_expiry_refreshes_once_for_concurrent_sessions":                        sameAsServe(),
	"codex_login_rotated_refresh_token_is_stored":                                           sameAsServe(),
	"codex_login_rejected_token_refreshes_and_retries":                                      sameAsServe(),
	"codex_login_rewritten_file_is_used_by_the_next_request":                                sameAsServe(),
	"codex_login_refresh_failure_fails_the_turn_with_guidance":                              sameAsServe(),
	"codex_login_missing_fails_the_turn_with_guidance":                                      sameAsServe(),
	"anthropic_injected_request_carries_no_credential_header":                               sameAsServe(),
	"anthropic_injected_extra_headers_pass_through_as_placeholders":                         sameAsServe(),
	"anthropic_injected_401_fails_the_turn_with_the_login_url":                              sameAsServe(),
	"anthropic_injected_403_is_not_login_required":                                          sameAsServe(),
	"codex_injected_http_request_carries_no_credential_header":                              sameAsServe(),
	"codex_injected_http_request_carries_the_hash_of_the_session_id_as_cache_key":           sameAsServe(),
	"codex_injected_models_from_chatgpt_session_reports_a_cache_expiry":                     sameAsServe(),
	"codex_injected_models_from_chatgpt_websocket_dial_carries_the_hash_as_session_headers": sameAsServe(),
	"codex_injected_websocket_request_carries_the_hash_of_the_session_id_as_cache_key":      sameAsServe(),
	"codex_injected_websocket_dial_carries_no_credential_header":                            sameAsServe(),
	"codex_injected_extra_headers_pass_through_as_placeholders":                             sameAsServe(),
	"codex_injected_401_fails_the_turn_with_login_required":                                 sameAsServe(),
	"codex_injected_401_without_a_login_url_names_the_provider_only":                        sameAsServe(),
	"codex_injected_websocket_401_fails_the_turn_with_login_required":                       sameAsServe(),
	"codex_injected_403_is_not_login_required":                                              sameAsServe(),
	"codex_injected_redirect_is_an_error":                                                   sameAsServe(),
	"codex_models_from_chatgpt_lists_the_models_of_the_backend":                             sameAsServe(),
	"codex_without_models_from_lists_the_static_catalog_and_fetches_nothing":                sameAsServe(),
	"codex_models_from_chatgpt_with_a_401_lists_the_static_catalog":                         sameAsServe(),
	"codex_login_entry_without_models_from_lists_the_static_catalog_and_fetches_nothing":    sameAsServe(),
	"codex_login_cancel_during_refresh_keeps_the_rotated_token":                             sameAsServe(),
	"codex_login_unsaved_refresh_stays_in_memory":                                           sameAsServe(),
	"codex_login_transient_refresh_failure_does_not_retry_on_every_request":                 sameAsServe(),
	"codex_login_redirect_from_the_bound_url_is_refused":                                    sameAsServe(),
	"codex_login_never_sends_the_openai_api_key":                                            sameAsServe(),
	"codex_login_logout_during_a_refresh_leaves_no_file":                                    sameAsServe(),
	"codex_login_rejected_websocket_fails_the_turn_without_the_http_fallback":               sameAsServe(),
	"codex_login_malformed_refresh_answer_fails_the_turn_with_guidance":                     sameAsServe(),
	"codex_http_5xx_is_retried_by_the_request":                                              sameAsServe(),
	"codex_http_context_overflow_compacts_and_runs_the_turn_again":                          sameAsServe(),
	"compact_summary_is_retried_after_a_refusal":                                            sameAsServe(),
	"retried_call_with_a_tool_call_alone_completes_under_a_new_item":                        sameAsServe(),
	"retried_call_discards_the_item_that_the_failed_call_streamed":                          sameAsServe(),
	"a_late_subscriber_receives_the_frames_of_the_streaming_item":                           sameAsServe(),
	"a_subscriber_that_resumes_past_the_item_start_receives_no_frame_of_the_item":           sameAsServe(),
	"chat_subscriber_past_the_item_start_receives_no_frame_of_the_item":                     sameAsServe(),
	"max_tokens_cut_response_is_retried":                                                    sameAsServe(),
	"stream_retries_stop_after_stream_max_retries":                                          reGolden(specView, specRetryable),
	"settings_change_to_claude_code_mid_turn_fails_the_turn":                                reGolden(specUpdate, specView, specMidTurnFails, specClaudeGauge),
	"typed_commands_record_their_outcome":                                                   reGolden(specTypedReceipt, specCmdRepeat, specCmdOps, specCmdFailed, specReceipt),
	"typed_compact_keeps_keep_turns_and_returns_the_range":                                  reGolden(specTypedReceipt, specCmdResult, specReceipt),
	"unknown_tool_call_gets_an_error_result":                                                reGolden(specMCPText),
	"task_profile_sets_the_tools_model_and_prompt_of_the_child":                             sameAsServe(),
	"task_spawn_fails_on_an_agent_name_repeated_in_one_dir":                                 reGolden(specRepeatName, specErrors, specView),
	"task_spawn_fails_on_an_agent_name_repeated_across_dirs":                                reGolden(specRepeatName, specErrors, specView),
	"session_of_a_child_opens_after_an_agent_name_is_repeated":                              reGolden(specTaskInputs, specChildReport, specChildNoGoal, specReceipt, specView, specChildAgent),
	"agent_defs_dirs_replace_the_default_profile_dir":                                       reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_explore_and_plan_children_get_read_only_tools":                                    sameAsServe(),
	"task_refusals":                                                                              reGolden(specTaskWording, specOneResult),
	"task_refusal_past_max_task_depth":                                                           reGolden(specTaskWithheld, specMCPText, specChildNoGoal),
	"task_refusal_past_max_concurrent_tasks":                                                     reGolden(specLimitFails, specChildNoGoal),
	"task_status_and_log_of_a_failed_child":                                                      reGolden(specChildReport, specChildReason, specChildBound, specChildStatus, specItems, specOneResult),
	"task_status_and_log_of_a_settled_child":                                                     reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_cancel_and_send_to_a_running_child":                                                    reGolden(specTaskInputs, specChildWording, specChildNoGoal, specItems, specOneResult),
	"task_spawn_past_max_tree_tokens_is_refused":                                                 reGolden(specLimitFails, specChildNoGoal),
	"task_send_runs_a_settled_child_again":                                                       reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_two_sends_to_a_settled_child_need_one_slot":                                            reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_action_refusals":                                                                       reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_tree_reaches_a_grandchild":                                                             reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_cancel_of_a_child_stops_the_grandchild":                                                reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_cancel_of_a_grandchild_reports_to_the_nearest_live_ancestor":                           reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_tree_interrupt_stops_the_grandchild":                                                   reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView, specChildAgent),
	"task_profile_keeps_the_plugin_tools_of_its_list":                                            reGolden(specProfileKnown, specTaskInputs, specItems, specOneResult),
	"task_child_on_claude_code_gets_the_builtin_tools_of_its_profile":                            sameAsServe(),
	"claudecode_turn_gets_no_plugin_system_segment":                                              sameAsServe(),
	"claudecode_question_turn_records_its_cost_after_the_response_ends":                          reGolden(specWaiting, specView, specClaudeGauge),
	"claudecode_steer_during_a_pending_question_stays_queued_for_the_next_turn":                  reGolden(specWaiting, specView, specClaudeGauge),
	"claudecode_steer_between_a_call_and_a_later_question_of_one_response_stays_queued":          reGolden(specWaiting, specView, specClaudeGauge),
	"claudecode_question_denied_by_a_plugin_hook_gets_an_error_result_and_the_turn_goes_on":      reGolden(specOneResult, specView, specClaudeGauge),
	"claudecode_answered_question_replays_to_a_model_api_session":                                reGolden(specView, specClaudeGauge),
	"claudecode_question_dismissed_by_resolve":                                                   reGolden(specDismissed, specNoStart, specWaiting, specView, specClaudeGauge),
	"claudecode_question_dismissed_by_a_model_of_another_provider":                               reGolden(specProviderSwap, specDismissed, specView, specErrors, specClaudeGauge),
	"claudecode_question_answer_bodies_that_are_refused":                                         reGolden(specAnswerMap, specRequestRoute, specErrors, specView, specAnswerReceipt, specClaudeGauge),
	"model_api_question_answered_after_a_restart_keeps_the_result_next_to_its_call":              sameAsServe(),
	"model_api_second_question_call_gets_an_error_when_the_first_is_denied":                      sameAsServe(),
	"model_api_question_with_invalid_input_gets_an_error_result":                                 sameAsServe(),
	"model_api_session_info_lists_the_question_tool_when_it_is_offered":                          sameAsServe(),
	"model_api_child_session_is_not_offered_the_question_tool":                                   sameAsServe(),
	"model_api_goal_session_is_not_offered_the_question_tool":                                    sameAsServe(),
	"model_api_question_with_repeated_question_texts_gets_an_error_result":                       sameAsServe(),
	"model_api_question_with_repeated_option_labels_gets_an_error_result":                        sameAsServe(),
	"model_api_question_with_a_long_header_gets_an_error_result":                                 sameAsServe(),
	"model_api_question_with_preview_metadata_answers_and_annotations_opens_a_request":           sameAsServe(),
	"model_api_interrupt_during_the_before_hook_of_a_question_opens_no_request":                  sameAsServe(),
	"model_api_goal_set_during_a_model_call_refuses_the_question":                                sameAsServe(),
	"model_api_handoff_while_a_sibling_tool_runs_opens_no_request":                               sameAsServe(),
	"model_api_question_parks_then_answer_resumes":                                               sameAsServe(),
	"model_api_question_dismissed_by_resolve":                                                    sameAsServe(),
	"model_api_question_dismissed_by_next_prompt":                                                sameAsServe(),
	"model_api_question_stays_open_when_an_input_of_a_client_source_arrives":                     sameAsServe(),
	"model_api_goal_set_while_machine_input_waits_behind_a_question_dismisses_it":                sameAsServe(),
	"claudecode_question_stays_open_when_an_input_of_a_client_source_arrives":                    reGolden(specWaiting, specView, specAnswerReceipt, specClaudeGauge),
	"model_api_held_steer_input_joins_the_answer_turn_at_its_next_tool_call":                     sameAsServe(),
	"model_api_held_queue_input_runs_as_the_next_turn_after_the_answer_turn":                     sameAsServe(),
	"claudecode_held_machine_input_runs_when_a_model_of_another_provider_dismisses_the_question": reGolden(specWaiting, specView, specProviderSwap, specClaudeGauge),
	"model_api_question_stays_open_when_a_child_report_arrives":                                  sameAsServe(),
	"model_api_queued_machine_input_runs_after_the_question_is_dismissed":                        sameAsServe(),
	"model_api_typed_input_dismisses_the_question_and_runs_with_the_queued_machine_input":        sameAsServe(),
	"model_api_session_without_the_option_is_not_offered_the_question_tool":                      sameAsServe(),
	"claudecode_question_beside_a_running_call_keeps_the_call_result_and_parks":                  reGolden(specOneResult, specDismissed, specWaiting, specView, specClaudeGauge),
	"usage_survives_a_kill_mid_turn":                                                             reGolden(specView, specCrash, specCrashMarker),

	"auto_compaction_estimates_the_context_when_no_call_reports_prompt_tokens":            reGolden(specPinsMove, specView),
	"auto_compaction_waits_for_the_reading_to_fall_after_an_empty_summary":                sameAsServe(),
	"auto_compaction_estimates_the_context_when_the_newest_turn_reports_no_prompt_tokens": reGolden(specPinsMove, specView),
	"auto_compaction_runs_again_after_a_model_change_moves_the_window":                    reGolden(specPinsMove, specView),
	"auto_compaction_waits_for_the_reading_to_fall_before_it_runs_again":                  reGolden(specPinsMove, specView),
	"mcp_instructions_are_cut_at_4000_runes":                                              sameAsServe(),
	"mcp_instruction_tool_names_stop_at_2048_bytes":                                       sameAsServe(),
	"mcp_catalog_lists_200_deferred_tools_then_counts_the_rest":                           sameAsServe(),
	"a_negative_compaction_threshold_is_the_default_threshold":                            reGolden(specView, specThreshold, specPinsMove),
	"auto_compaction_with_a_failed_summary_keeps_the_history":                             reGolden(specView, specFailedSummary),
	"restart_during_auto_compaction_runs_the_queued_input_on_the_next_owner":              reGolden(specView, specOpenStarts),
	"interrupt_stops_an_auto_compaction":                                                  reGolden(specView, specErrors, specInterruptTable, specCompactBusy),
	"auto_compaction_uses_the_window_of_the_session_model":                                reGolden(specView, specWindow, specPinsMove),
	"context_overflow_compacts_and_runs_the_turn_again":                                   reGolden(specView, specOverflowFolds, specPinsMove),
	"context_overflow_after_the_compaction_fails_the_turn":                                reGolden(specView, specOverflowFolds, specOverflowTwice, specPinsMove),
	"a_stalled_summary_fails_the_overflowed_turn":                                         reGolden(specView, specOverflowFails),
	"compact_during_a_turn_is_session_busy":                                               reGolden(specView, specErrors, specCompactBusy),
	"provider_usage_limit_fails_the_turn_and_holds_the_queue":                             reGolden(specView, specExhaustedHolds, specExhaustedQueue, specRestartOpens),
	"a_failed_turn_runs_the_next_queued_input":                                            reGolden(specView, specFailedRuns),
	"session_usage_counts_every_model_call_but_the_evaluation":                            reGolden(specView, specCompactResult, specPinsMove),
	"goal_impossible_verdict_fails_the_goal":                                              reGolden(specView, specGoalImpossible, specGoalPrompt),
	"goal_set_on_a_busy_session_judges_the_running_turn":                                  reGolden(specView, specGoalBusy),
	"goal_clear_and_input_during_a_goal_turn":                                             reGolden(specView, specGoalClear, specGoalWithdraw),
	"interrupt_during_a_goal_turn_ends_the_goal":                                          sameAsServe(),
	"goal_set_after_an_interrupt_runs_normally":                                           sameAsServe(),
	"interrupt_during_a_goal_evaluation_ends_the_goal":                                    sameAsServe(),
	"interrupt_during_a_compaction_between_goal_turns_ends_the_goal":                      sameAsServe(),
	"goal_judges_the_last_turn_after_a_restart":                                           reGolden(specView, specGoalRestart),
	"claudecode_child_and_goal_sessions_ask_no_question":                                  sameAsServe(),
	"codex_ws_restart_warms_the_websocket_again":                                          reGolden(specItems, specWarm),
	"task_profile_of_a_grandchild_keeps_the_tools_its_parent_allows":                      sameAsServe(),
	"a_failed_summary_keeps_its_usage_in_the_session":                                     reGolden(specView, specOverflowFails),

	"claudecode_history_bridge_after_a_mid_turn_model_change":                     reGolden(specUpdate, specView, specMidTurnFails, specClaudeGauge),
	"claudecode_history_bridge_after_a_turn_whose_cli_never_started":              reGolden(specUpdate, specView, specClaudeGauge),
	"claudecode_restart_mid_turn_stops_the_cli_and_resumes":                       reGolden(specView, specExternal, specHandoffResume, specOneResult, specClaudeGauge),
	"claudecode_crash_mid_turn_waits_for_input":                                   reGolden(specView, specExternal, specCrash, specCrashQueue, specCrashMarker, specHistoryBridge, specClaudeGauge),
	"claudecode_mirror_continues_a_turn_that_the_cli_took":                        reGolden(specView, specBackendState, specMirrorBlob, specHandoffResume, specClaudeGauge),
	"claudecode_mirror_restart_before_a_transcript_starts_the_turn_again":         reGolden(specView, specBackendState, specMirrorBlob, specHandoffResume, specClaudeGauge),
	"claudecode_mirror_resumes_after_a_restart":                                   reGolden(specBackendState, specMirrorBlob),
	"claudecode_mirror_crash_before_a_transcript_starts_a_new_session":            reGolden(specView, specBackendState, specMirrorBlob, specCrash, specCrashQueue, specClaudeGauge),
	"claudecode_interrupt_keeps_the_usage_of_the_result_after_the_signal":         reGolden(specView, specStopped, specClaudeGauge),
	"claudecode_running_tool_call_is_in_the_messages_before_its_result":           reGolden(specView, specToolCallsAtOnce, specStopped, specOneResult),
	"claudecode_interrupt_closes_a_tool_call_that_the_cli_left_open":              reGolden(specView, specStopped, specOneResult, specClaudeGauge),
	"claudecode_interrupt_closes_a_tool_call_that_the_cli_printed_on_the_signal":  reGolden(specView, specStopped, specOneResult, specClaudeGauge),
	"claudecode_interrupt_keeps_a_tool_result_that_the_cli_printed_on_the_signal": reGolden(specView, specStopped, specClaudeGauge),
	"claudecode_interrupt_ends_completed_when_the_cli_finishes_on_the_signal":     reGolden(specView, specClaudeGauge),
	"claudecode_interrupt_ignores_a_placeholder_result":                           sameAsServe(),
	"claudecode_placeholder_result_before_the_turn_is_skipped":                    sameAsServe(),
	"claudecode_interrupt_of_a_cli_that_exits_with_no_frame":                      reGolden(specView, specStopped, specClaudeGauge),

	"claudecode_runs_in_the_work_dir":                               sameAsServe(),
	"claudecode_append_system_prompt_reaches_the_cli_as_one_value":  sameAsServe(),
	"claudecode_compact_result_with_no_local_command_ends_the_turn": reGolden(specView, specCompactOwned, specClaudeGauge, specClaudeCompactPostTokens),
	"claudecode_queued_notification_result_does_not_end_the_turn":   reGolden(specView, specClaudeGauge),
	"claudecode_compact_refuses_keep_turns":                         reGolden(specView, specKeepOwned, specErrors, specClaudeGauge),
	"claudecode_cli_compaction_is_logged":                           reGolden(specView, specCompactOwned, specClaudeGauge),

	"process_http_lifecycle":           sameAsServe(),
	"process_http_unknown_name_is_404": reGolden(specProcessUnknown, specNoRoute, specErrors),
	"git_changes_uncommitted_scope_reports_modified_deleted_and_untracked_files": sameAsServe(),
	"git_changes_branch_scope_diffs_the_work_tree_against_the_default_branch":    sameAsServe(),
	"git_changes_uncommitted_scope_on_an_unborn_repo_reports_files_as_added":     sameAsServe(),
	"git_changes_refusals": reGolden(specGitOracle, specErrors, specInvalidReason),

	"commands_menu_lists_prompt_commands_under_a_symlinked_ancestor": reGolden(specCmdMenuRoutes, specCmdUnsupport),
}, cancelRows, upfrontRows, boxReplyRows, credentialProviderRows)
