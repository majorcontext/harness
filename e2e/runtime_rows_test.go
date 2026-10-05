package e2e

// Lines of docs/architecture.md that the re-golden and deleted rows cite.
const (
	specView          = "A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, context gauge, last turn, compaction count, subscription usage, head seq)"
	specUpdate        = "func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)"
	specErrors        = "Body: `{\"error\":{\"code\":\"...\",\"message\":\"...\",\"details\":{}}}`."
	specReceipt       = "| New id | `201 {input_id, seq}` |"
	specItems         = "| `item.completed` | `item_id`, `turn_id`, `message` (user, assistant, tool result) |"
	specOneResult     = "Every tool call item gets exactly one result item, or an open request, before its turn ends."
	specExternal      = "| Items | External items become `item.completed` | stream-json frames | `item/completed` |"
	specTaskInputs    = "Task notifications become inputs"
	specChildReport   = "The report names the child, its agent, the outcome, and the error, and holds the last assistant text of the child"
	specChildNoGoal   = "A child session has no `goal` tool"
	specChildWording  = "Does a child report to a busy parent keep the task notification of the engine? Yes, built"
	specAnswerNoSteer = "A run that answers a question takes no steer input"
	specCrash         = "Append `turn.ended{interrupted, crashed}`; keep the partial"
	specCrashQueue    = "The session then starts the next queued input, or waits for input when none is queued."
	specHandoffResume = "A suspended turn has no open tool call, so the next owner resumes it automatically."
	specQueue         = "(queue: next turn; steer: next item boundary)"
	specMCPText       = "By design, an error has no `engine:` prefix, a call to a tool that is not there reads `no such tool available`, binary content becomes text"
	specMCPNoStatus   = "There is no `status` action, no MCP status segment, and no background retry."
	specMCPStatus     = "Switch oracle: the `mcp_*` rows except `mcp_status_*`."
	specGoalDeferred  = "The deferred and parked rows are deleted."
	specCursor        = "One per-session `seq` serves paging and SSE resume."
	specEventsRoute   = "GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream"
	specBoxGlobal     = "| Box-global `events.jsonl` | Delete |"
	specModelCheck    = "An unknown model fails with `model_unavailable` at create and at a settings change."
	specOverflowFails = "When no turn can fold or the summary fails, the turn fails."
	specCompactResult = "`Compact()` returns `protocol.Compacted`"
	specChildResend   = "changes by design in one way: a later send is not refused"
	specWaiting       = "| `waiting` | A turn ended `awaiting_input`; a request is open |"
	specWarm          = "The session calls it once on create and on wake, fire-and-forget under the session context."
	specCompactOwned  = "A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`."

	specErrorText    = "the actor masks and bounds each error text that it writes to the log"
	specGoalFailed   = "An error the user must fix yields `failed`."
	specNoParkedGoal = "There is no deferred goal and no parked goal."

	specPromptSwitch = "At the switch, the `runtime_prompt` contract rows change in three ways"
	specModelTool    = "Port it fully before the switch: `status`, `list`, and `set` through `Session.Update`"

	specOpenContinuation  = "Does the switch wrap the messages that the engine writes for the model"
	specOpenListOrder     = "Does `GET /sessions` keep creation order?"
	specOpenAnswerReceipt = "Does the answer route keep the serve receipt"
	specOpenGauge         = "Does the switch keep the context gauge and the session cost of a Claude Code turn?"
	specOpenRetry         = "Does a failed Claude Code turn run again?"
	specOpenChildFailure  = "Does a failed child, an exhausted child, or a long result in a report to a busy parent keep the engine text?"
	specOpenChildParts    = "Does the log keep a `task_report` part and an `engine_context` part?"

	specStopped       = "Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs"
	specInterrupt     = "`interrupt` stops the running turn only. The next queued input then starts"
	specSameBody      = "| Same id, same body | `200` with the original receipt |"
	specOtherBody     = "| Same id, other body | `409 input_conflict` |"
	specTurnMismatch  = "A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running."
	specRetryable     = "`ErrRetryable` (a 429, a 5xx, a truncated stream, a response with no output) calls the model again after a wait of 1 s that doubles up to 8 s, with jitter, up to `prompt_retries` times"
	specModelsRoute   = "GET    /models                                models and their capabilities"
	specHandoff       = "admit no new tool call, let running tools finish within the budget, append `turn.suspended`"
	specToolsSerial   = "Tools run one at a time, so the tool-batching segment is gone."
	specPromptOnce    = "It reads them once, when the session is created or opened, and sends them as `turn.Request.Instructions` on each model call."
	specOpenMidTurn   = "Does a settings change to a model of another kind of backend take effect in the middle of a turn?"
	specTypedReceipt  = "A typed slash command answers the same way. Its receipt adds `command`, the newest status of the command"
	specCmdRepeat     = "A repeat of the input ID with the same line returns the newest status; another line, or an input ID of another input, is `input_conflict`."
	specCmdOps        = "`model`, `thinking`, and `tier` are `Update`"
	specCmdFailed     = "`failed` (the error text of a sentinel error"
	specCmdResult     = "Its result is the `protocol.Compacted` of `Compact`."
	specCmdInterrupt  = "`Open` records `interrupted` for each command that an earlier owner accepted and never finished"
	specCmdUnsupport  = "A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`"
	specCmdMenuRoutes = "A control command names its operation and `available_during_task`; the handler adds the route of the same operation, where one exists."
	specGoalOwnTurn   = "So the turn that calls `set` is not judged; the condition runs as a turn of its own after it"
	specGoalPrompt    = "Its prompt copies the engine prompt, with a third form for `impossible`."
	specGoalWording   = "The tool copies the engine description and error wording."
	specGoalNoEval    = "`SetGoal` without it is an invalid request."
	specOverflowFolds = "A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history."
	specBannerPrefix  = "Each request is a prefix of the next, also after a compaction in the middle of a turn, and a compaction can only move the place earlier."
)

func sameAsServe() runtimeRow { return runtimeRow{kind: rowSame} }

// reGolden cites each spec line that decides a difference of the row, and
// each finding that owns the rest. A break that no decision covers is a line
// under Open questions.
func reGolden(cites ...string) runtimeRow { return runtimeRow{kind: rowRegolden, cites: cites} }

func deletedBy(spec string) runtimeRow { return runtimeRow{kind: rowDeleted, cites: []string{spec}} }

// pendingOn names the findings, phases, and spec lines that a row waits for.
func pendingOn(cites ...string) runtimeRow { return runtimeRow{kind: rowPending, cites: cites} }

// runtimeRows is the disposition of each contract row on the runtime host.
var runtimeRows = map[string]runtimeRow{
	"auto_compact_on_threshold":                                   reGolden(specView),
	"bifrost_429_then_ok":                                         reGolden(specItems, specView),
	"bifrost_context_overflow":                                    reGolden(specView, specOverflowFails),
	"bifrost_goal_met_first_turn":                                 reGolden(specItems),
	"bifrost_goal_not_met_then_met":                               reGolden(specItems),
	"bifrost_max_tokens_continuation":                             pendingOn(specOpenContinuation),
	"bifrost_prompt_attachments":                                  reGolden(specItems),
	"bifrost_reasoning_and_effort":                                reGolden(specItems, specUpdate),
	"bifrost_text_reply":                                          reGolden(specItems),
	"bifrost_tool_error_marker":                                   reGolden(specItems),
	"bifrost_tool_round_trip":                                     reGolden(specItems),
	"bifrost_two_tool_calls_one_turn":                             reGolden(specItems, specOneResult),
	"bootstrap_cold_then_resident_windows":                        pendingOn("phase 4"),
	"bootstrap_cold_window_after_kill":                            pendingOn("phase 4"),
	"builtin_commands_run_and_record":                             pendingOn("phase 4"),
	"busy_deferred_goal_with_max_turns":                           deletedBy(specGoalDeferred),
	"child_crash_recovered":                                       reGolden(specView, specCrash, specChildReport),
	"child_usage_limit_delivered":                                 reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView),
	"child_error_delivered":                                       reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView),
	"child_report_reaches_a_busy_parent_at_the_tool_boundary":     reGolden(specChildReport, specChildWording, specOpenChildParts),
	"child_error_reaches_a_busy_parent_at_the_tool_boundary":      reGolden(specChildReport, specChildWording, specOpenChildFailure, specOpenChildParts),
	"claudecode_compact_delegated":                                reGolden(specView, specCompactOwned, specCompactResult),
	"claudecode_cli_gets_the_tools_of_the_engine_bridge":          sameAsServe(),
	"claudecode_configured_mcp_servers_reach_the_cli":             sameAsServe(),
	"claudecode_context_window_from_model_usage":                  reGolden(specView),
	"claudecode_error_result_fails_turn":                          reGolden(specView, specOpenRetry, specOpenGauge),
	"claudecode_history_bridge_after_native_turn":                 reGolden(specUpdate),
	"claudecode_interrupt_mid_turn":                               reGolden(specView),
	"claudecode_question_dismissed_by_compact":                    reGolden(specWaiting, specView, specCompactResult),
	"claudecode_question_dismissed_by_next_prompt":                reGolden(specWaiting, specView),
	"claudecode_question_answer_run_takes_no_steer_input":         reGolden(specWaiting, specView, specAnswerNoSteer, specOpenAnswerReceipt),
	"claudecode_question_parks_then_answer_resumes":               reGolden(specWaiting, specView, specOpenAnswerReceipt),
	"claudecode_question_unknown_call_id_conflicts":               reGolden(specWaiting, specView, specErrors, specOpenAnswerReceipt),
	"claudecode_prompt_attachments":                               sameAsServe(),
	"claudecode_queued_prompt_injected_mid_turn":                  reGolden(specView),
	"claudecode_rate_limit_event_reaches_subscription_usage":      reGolden(specView),
	"claudecode_resume_across_turns":                              sameAsServe(),
	"claudecode_resume_survives_restart":                          sameAsServe(),
	"claudecode_subagent_frames_keep_parent":                      sameAsServe(),
	"claudecode_thinking_block_is_reasoning":                      reGolden(specExternal, specView),
	"claudecode_turn_text_and_tool":                               reGolden(specView, specOpenGauge),
	"codex_http_mcp_tool_schema_is_sanitized":                     reGolden(specItems),
	"codex_http_reasoning_replays_on_tool_round_trip":             reGolden(specItems),
	"codex_http_sse_text_turn":                                    reGolden(specItems),
	"codex_http_sse_tool_round_trip_resends_history":              reGolden(specItems),
	"codex_ws_chain_miss_resends_full_history":                    reGolden(specItems, specWarm),
	"codex_ws_chains_two_turns":                                   reGolden(specItems, specWarm),
	"codex_ws_drop_mid_turn_resends_full_history":                 reGolden(specView, specItems),
	"codex_ws_effort_sets_reasoning_effort":                       reGolden(specView, specItems, specUpdate),
	"codex_ws_prewarm_warms_first_turn":                           reGolden(specItems, specWarm),
	"codex_ws_reasoning_chains_tool_round_trip":                   reGolden(specItems, specWarm),
	"codex_ws_refused_falls_back_to_http":                         reGolden(specItems, specWarm),
	"codex_ws_tool_round_trip":                                    reGolden(specItems, specWarm),
	"codex_ws_uncoded_chain_miss_resends_full_history":            reGolden(specItems, specWarm),
	"codex_ws_usage_frame_reaches_session":                        reGolden(specView, specItems),
	"codex_http_usage_headers_reach_session":                      reGolden(specView, specItems),
	"compact_manual":                                              pendingOn("phase 4"),
	"compact_survives_restart":                                    pendingOn("phase 4"),
	"context_overflow":                                            reGolden(specView, specOverflowFails),
	"deferred_goal_judges_finished_turn":                          deletedBy(specGoalDeferred),
	"deferred_goal_with_max_turns":                                deletedBy(specGoalDeferred),
	"driver_child_send_and_cancel":                                reGolden(specTaskInputs, specChildNoGoal, specReceipt, specView),
	"driver_clean_restart":                                        reGolden(specView),
	"driver_compact":                                              reGolden(specView, specCompactResult),
	"driver_queue_goal_and_end":                                   pendingOn("phase 4"),
	"driver_resume_streams":                                       reGolden(specCursor, specBoxGlobal),
	"driver_settings_and_reads":                                   pendingOn("phase 4"),
	"end_session_semantics":                                       pendingOn("phase 4"),
	"enqueue_joins_the_turn_at_the_tool_boundary":                 sameAsServe(),
	"enqueue_while_busy_runs_after":                               sameAsServe(),
	"file_tools_read_edges":                                       sameAsServe(),
	"file_tools_roundtrip":                                        sameAsServe(),
	"file_tools_search_edges":                                     sameAsServe(),
	"file_tools_write_edit_guards":                                sameAsServe(),
	"goal_busy_send_is_queued":                                    reGolden(specView),
	"goal_cleared_before_first_turn":                              deletedBy(specGoalDeferred),
	"goal_exhausts_max_turns":                                     reGolden(specView),
	"goal_met_first_turn":                                         sameAsServe(),
	"goal_not_met_then_met":                                       sameAsServe(),
	"goal_provider_exhausted_parks":                               deletedBy(specGoalDeferred),
	"goal_update_deferred_goal_waits_for_first_turn":              deletedBy(specGoalDeferred),
	"goal_update_while_busy":                                      reGolden(specView),
	"history_survives_clean_restart":                              sameAsServe(),
	"interrupt_drops_unfinished_text_then_queue_continues":        sameAsServe(),
	"interrupt_idle_is_noop":                                      sameAsServe(),
	"journal_pages_follow_cursor":                                 reGolden(specCursor, specEventsRoute),
	"kill_mid_turn_then_continue":                                 sameAsServe(),
	"max_tokens_continuation":                                     pendingOn(specOpenContinuation),
	"mcp_auto_default_threshold_defers_at_21_tools":               sameAsServe(),
	"mcp_auto_default_threshold_stays_eager_at_20_tools":          sameAsServe(),
	"mcp_auto_defers_over_threshold":                              sameAsServe(),
	"mcp_auto_stays_eager_under_threshold":                        sameAsServe(),
	"mcp_eager_lists_namespaced_tools":                            sameAsServe(),
	"mcp_http_sse_reply":                                          sameAsServe(),
	"mcp_instructions_in_system_prompt":                           sameAsServe(),
	"mcp_lazy_call_without_select_loads_the_tool":                 sameAsServe(),
	"mcp_lazy_search_select_then_call":                            sameAsServe(),
	"mcp_lazy_select_reports_each_name":                           sameAsServe(),
	"mcp_non_text_results_become_text_and_blobs":                  reGolden(specMCPText),
	"mcp_paged_tool_list_is_merged":                               sameAsServe(),
	"mcp_per_server_tool_loading_overrides_global":                sameAsServe(),
	"mcp_resources_list_and_read":                                 reGolden(specMCPText),
	"mcp_resources_paged_list_is_merged":                          sameAsServe(),
	"mcp_server_lost_mid_session_hides_the_endpoint":              reGolden(specMCPText),
	"mcp_status_reports_connected_and_unavailable_servers":        deletedBy(specMCPStatus),
	"mcp_stdio_server_call":                                       sameAsServe(),
	"mcp_stdio_server_starts_in_configured_dir":                   sameAsServe(),
	"mcp_tool_call_result":                                        sameAsServe(),
	"mcp_tool_error_and_rpc_error_reach_model":                    reGolden(specMCPText),
	"mcp_two_servers_share_a_tool_name":                           sameAsServe(),
	"mcp_unavailable_at_start_then_connect":                       reGolden(specMCPNoStatus),
	"mcp_unavailable_connect_fails_with_classified_reason":        reGolden(specMCPNoStatus, specMCPText),
	"messages_page_after_compaction":                              pendingOn("phase 4"),
	"messages_page_windows":                                       pendingOn("phase 4"),
	"one_tool_round_trip":                                         sameAsServe(),
	"openai_key_http_sse_text_turn":                               reGolden(specItems),
	"persisted_queue_dispatches_after_deferred_arm":               deletedBy(specGoalDeferred),
	"plugin_after_hook_sees_output":                               sameAsServe(),
	"plugin_before_hook_rewrites_and_blocks":                      sameAsServe(),
	"plugin_boxes_style_command_and_dir":                          sameAsServe(),
	"plugin_crash_mid_call_session_continues":                     reGolden(specView),
	"plugin_event_and_after_hook_payloads":                        sameAsServe(),
	"plugin_system_segment_in_every_request":                      sameAsServe(),
	"plugin_system_transform_reads_session_messages":              sameAsServe(),
	"plugin_session_messages_carry_attachments":                   sameAsServe(),
	"plugin_tools_listed_and_run":                                 reGolden(specView),
	"provider_429_then_ok":                                        reGolden(specView),
	"provider_usage_limit_fails_turn":                             reGolden(specView),
	"provider_5xx_then_ok":                                        reGolden(specView),
	"provider_error_text_is_masked_and_bounded":                   reGolden(specView, specErrorText, specGoalFailed, specNoParkedGoal),
	"queue_delete_while_busy":                                     reGolden(specView),
	"queue_survives_clean_restart_then_delete":                    reGolden(specView, specHandoffResume),
	"queue_survives_clean_restart_then_drains_with_next_prompt":   reGolden(specQueue, specHandoffResume, specView),
	"queued_input_runs_after_kill":                                reGolden(specCrash, specCrashQueue),
	"queued_input_survives_kill":                                  deletedBy(specCrashQueue),
	"queued_prompt_runs_before_deferred_auto_arm":                 deletedBy(specGoalDeferred),
	"replay_after_kill_full_transcript":                           pendingOn("phase 4"),
	"send_to_child_and_cancel_tree":                               reGolden(specChildResend, specChildNoGoal, specReceipt, specTaskInputs, specView),
	"settings_model_change_reaches_the_next_model_call_of_a_turn": reGolden(specUpdate),
	"session_info_reports_the_session":                            reGolden(specPromptSwitch, specModelTool),
	"session_info_reports_what_the_session_loaded":                reGolden(specPromptSwitch, specModelTool),
	"session_info_reports_the_plugin_and_its_system_segment":      reGolden(specPromptSwitch, specModelTool),
	"session_settings_validation_and_persistence":                 reGolden(specModelCheck, specErrors, specUpdate, specView),
	"sse_resume_after_kill":                                       reGolden(specCursor, specBoxGlobal),
	"sse_resume_cursor":                                           reGolden(specCursor, specBoxGlobal),
	"status_and_list_cold_after_restart":                          pendingOn(specOpenListOrder),
	"steer_joins_the_turn_at_the_tool_boundary":                   sameAsServe(),
	"stream_stall":                                                reGolden(specView),
	"task_child_result_reaches_parent":                            reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"text_reply":                                                  sameAsServe(),
	"tool_error_reaches_model":                                    sameAsServe(),
	"two_tool_calls_one_turn":                                     reGolden(specOneResult),
	"two_turns_keep_history":                                      sameAsServe(),
	"usage_survives_a_mid_turn_restart":                           reGolden(specView, specHandoffResume),

	"a_kill_interrupts_an_unfinished_command":                         reGolden(specTypedReceipt, specCmdRepeat, specCmdInterrupt, specReceipt),
	"banner_holds_its_place_when_a_turn_compacts_in_the_middle":       reGolden(specOverflowFolds, specBannerPrefix, specView),
	"codex_http_truncated_and_empty_responses_are_retried":            reGolden(specView, specItems, specRetryable),
	"codex_service_tier_reaches_the_request":                          reGolden(specView, specItems, specUpdate),
	"codex_settings_switch_to_another_provider_keeps_the_history":     reGolden(specView, specItems, specUpdate),
	"commands_menu_lists_builtin_and_prompt_commands":                 reGolden(specCmdMenuRoutes, specCmdUnsupport),
	"create_checks_the_model":                                         reGolden(specModelCheck, specErrors, specView),
	"create_without_a_model_takes_the_default_model":                  reGolden(specView),
	"create_takes_an_unknown_model_when_no_window_is_required":        reGolden(specView),
	"create_takes_an_unknown_model_with_a_configured_window":          reGolden(specView),
	"goal_tool_actions_report_and_refuse":                             pendingOn(specGoalPrompt),
	"goal_tool_adjust_after_set_runs_the_adjusted_condition":          pendingOn(specGoalOwnTurn, specGoalPrompt),
	"goal_tool_adjust_keeps_the_turn_limit":                           pendingOn(specGoalPrompt),
	"goal_tool_refusals_copy_the_engine_wording":                      pendingOn(specGoalWording),
	"goal_tool_set_runs_the_condition_as_its_own_turn":                pendingOn(specGoalPrompt),
	"input_receipts_and_conflicts":                                    reGolden(specReceipt, specSameBody, specOtherBody, specTurnMismatch, specErrors),
	"instructions_are_read_when_the_session_starts":                   reGolden(specPromptOnce),
	"interrupt_cuts_a_running_tool_then_queue_continues":              reGolden(specStopped, specInterrupt),
	"interrupt_during_retry_backoff_ends_the_turn":                    reGolden(specView, specStopped),
	"models_lists_the_configured_providers":                           reGolden(specModelsRoute, specView),
	"no_goal_evaluator_means_no_goal":                                 reGolden(specGoalNoEval, specErrors, specCmdFailed, specView),
	"plugin_inventory_reports_not_spawned_then_running":               reGolden(specView),
	"plugin_sees_the_model_of_each_call":                              reGolden(specUpdate),
	"restart_lets_a_running_tool_finish_and_cuts_the_next_call":       reGolden(specView, specHandoff, specToolsSerial),
	"retries_stop_after_prompt_retries":                               reGolden(specView, specRetryable),
	"settings_change_to_claude_code_mid_turn_waits_for_the_next_turn": reGolden(specUpdate, specView, specOpenMidTurn),
	"typed_commands_record_their_outcome":                             reGolden(specTypedReceipt, specCmdRepeat, specCmdOps, specCmdFailed, specReceipt),
	"typed_compact_keeps_keep_turns_and_returns_the_range":            reGolden(specTypedReceipt, specCmdResult, specReceipt),
	"unknown_tool_call_gets_an_error_result":                          reGolden(specMCPText),
}
