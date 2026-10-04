package e2e

// Lines of docs/architecture.md that the re-golden and deleted rows cite.
const (
	specView          = "A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, head seq)"
	specUpdate        = "func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)"
	specErrors        = "Body: `{\"error\":{\"code\":\"...\",\"message\":\"...\",\"details\":{}}}`."
	specReceipt       = "| New id | `201 {input_id, seq}` |"
	specItems         = "| `item.completed` | `item_id`, `turn_id`, `message` (user, assistant, tool result) |"
	specOneResult     = "Every tool call item gets exactly one result item, or an open request, before its turn ends."
	specExternal      = "| Items | External items become `item.completed` | stream-json frames | `item/completed` |"
	specTaskInputs    = "Task notifications become inputs"
	specChildReport   = "The report names the child, its agent, the outcome, and the error, and holds the last assistant text of the child"
	specChildNoGoal   = "A child session has no `goal` tool"
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
	specChildResend   = "changes by design in one way: a later send is not refused"
	specWarm          = "The session calls it once on create and on wake, fire-and-forget under the session context."

	specOpenContinuation = "Does the switch wrap the messages that the engine writes for the model"
	specOpenMCPReason    = "Does the switch keep the classified reason of a failed MCP connect?"
	specOpenPlugins      = "Does the switch keep the plugin inventory?"
	specOpenSteerWrap    = "Does the switch wrap an input that joins a running turn at an item boundary?"
	specOpenCrashMarker  = "Does the switch keep the crash marker?"
	specOpenListOrder    = "Does `GET /sessions` keep creation order?"
	specOpenBanner       = "Does the switch keep the engine banner"
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
	"auto_compact_on_threshold":                                 pendingOn("F02"),
	"bifrost_429_then_ok":                                       reGolden(specItems, specView, "F02"),
	"bifrost_context_overflow":                                  pendingOn("F02"),
	"bifrost_goal_met_first_turn":                               reGolden(specItems),
	"bifrost_goal_not_met_then_met":                             reGolden(specItems),
	"bifrost_max_tokens_continuation":                           pendingOn("F02", specOpenContinuation),
	"bifrost_reasoning_and_effort":                              reGolden(specItems, specUpdate),
	"bifrost_text_reply":                                        reGolden(specItems),
	"bifrost_tool_error_marker":                                 reGolden(specItems),
	"bifrost_tool_round_trip":                                   reGolden(specItems),
	"bifrost_two_tool_calls_one_turn":                           reGolden(specItems, specOneResult),
	"bootstrap_cold_then_resident_windows":                      pendingOn("phase 4"),
	"bootstrap_cold_window_after_kill":                          pendingOn("phase 4"),
	"builtin_commands_run_and_record":                           pendingOn("F02", "phase 4"),
	"busy_deferred_goal_with_max_turns":                         deletedBy(specGoalDeferred),
	"child_crash_recovered":                                     pendingOn("F02", "F11"),
	"child_error_delivered":                                     reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView, "F02"),
	"claudecode_compact_delegated":                              pendingOn("F02", "F18", "F20"),
	"claudecode_configured_mcp_servers_reach_the_cli":           pendingOn("F20"),
	"claudecode_context_window_from_model_usage":                pendingOn("F02"),
	"claudecode_error_result_fails_turn":                        pendingOn("F02", "F20"),
	"claudecode_history_bridge_after_native_turn":               pendingOn("F20"),
	"claudecode_interrupt_mid_turn":                             pendingOn("F02", "F20"),
	"claudecode_question_dismissed_by_compact":                  pendingOn("F02", "phase 5"),
	"claudecode_question_dismissed_by_next_prompt":              pendingOn("F02", "phase 5"),
	"claudecode_question_parks_then_answer_resumes":             pendingOn("F02", "phase 5"),
	"claudecode_question_unknown_call_id_conflicts":             pendingOn("F02", "phase 5"),
	"claudecode_queued_prompt_injected_mid_turn":                pendingOn("F01", "F02"),
	"claudecode_rate_limit_event_reaches_subscription_usage":    pendingOn("F02"),
	"claudecode_resume_across_turns":                            pendingOn("F20"),
	"claudecode_resume_survives_restart":                        pendingOn("F20"),
	"claudecode_subagent_frames_keep_parent":                    pendingOn("F20"),
	"claudecode_thinking_block_is_reasoning":                    reGolden(specExternal, specView, "F02"),
	"claudecode_turn_text_and_tool":                             pendingOn("F02", "F20"),
	"codex_http_mcp_tool_schema_is_sanitized":                   reGolden(specItems),
	"codex_http_reasoning_replays_on_tool_round_trip":           reGolden(specItems),
	"codex_http_sse_text_turn":                                  reGolden(specItems),
	"codex_http_sse_tool_round_trip_resends_history":            reGolden(specItems),
	"codex_http_usage_headers_reach_session":                    pendingOn("F02"),
	"codex_ws_chain_miss_resends_full_history":                  reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_chains_two_turns":                                 reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_drop_mid_turn_resends_full_history":               pendingOn("F02", "phase 5"),
	"codex_ws_effort_sets_reasoning_effort":                     pendingOn("phase 5"),
	"codex_ws_prewarm_warms_first_turn":                         reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_reasoning_chains_tool_round_trip":                 reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_refused_falls_back_to_http":                       reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_tool_round_trip":                                  reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_uncoded_chain_miss_resends_full_history":          reGolden(specItems, specWarm, specOpenBanner),
	"codex_ws_usage_frame_reaches_session":                      pendingOn("F02", "phase 5"),
	"compact_manual":                                            pendingOn("F02", "F18", "phase 4"),
	"compact_survives_restart":                                  pendingOn("F02", "F18", "phase 4"),
	"context_overflow":                                          pendingOn("F02"),
	"deferred_goal_judges_finished_turn":                        deletedBy(specGoalDeferred),
	"deferred_goal_with_max_turns":                              deletedBy(specGoalDeferred),
	"driver_child_send_and_cancel":                              reGolden(specTaskInputs, specChildNoGoal, specReceipt, specView, "F02"),
	"driver_clean_restart":                                      reGolden(specView, "F02"),
	"driver_compact":                                            pendingOn("F02", "F18"),
	"driver_queue_goal_and_end":                                 pendingOn("phase 4"),
	"driver_resume_streams":                                     reGolden(specCursor, specBoxGlobal),
	"driver_settings_and_reads":                                 pendingOn("F02", "phase 4"),
	"end_session_semantics":                                     pendingOn("F02", "phase 4"),
	"enqueue_while_busy_runs_after":                             sameAsServe(),
	"file_tools_read_edges":                                     sameAsServe(),
	"file_tools_roundtrip":                                      sameAsServe(),
	"file_tools_search_edges":                                   sameAsServe(),
	"file_tools_write_edit_guards":                              sameAsServe(),
	"goal_busy_send_is_queued":                                  pendingOn("F02", "F18"),
	"goal_cleared_before_first_turn":                            deletedBy(specGoalDeferred),
	"goal_exhausts_max_turns":                                   pendingOn("F02", "F18"),
	"goal_met_first_turn":                                       sameAsServe(),
	"goal_not_met_then_met":                                     sameAsServe(),
	"goal_provider_exhausted_parks":                             deletedBy(specGoalDeferred),
	"goal_update_deferred_goal_waits_for_first_turn":            deletedBy(specGoalDeferred),
	"goal_update_while_busy":                                    pendingOn("F02", "F18"),
	"history_survives_clean_restart":                            sameAsServe(),
	"interrupt_drops_unfinished_text_then_queue_continues":      sameAsServe(),
	"interrupt_idle_is_noop":                                    sameAsServe(),
	"journal_pages_follow_cursor":                               reGolden(specCursor, specEventsRoute),
	"kill_mid_turn_then_continue":                               reGolden(specCrash, specOpenCrashMarker),
	"max_tokens_continuation":                                   pendingOn("F02", specOpenContinuation),
	"mcp_auto_default_threshold_defers_at_21_tools":             sameAsServe(),
	"mcp_auto_default_threshold_stays_eager_at_20_tools":        sameAsServe(),
	"mcp_auto_defers_over_threshold":                            sameAsServe(),
	"mcp_auto_stays_eager_under_threshold":                      sameAsServe(),
	"mcp_eager_lists_namespaced_tools":                          sameAsServe(),
	"mcp_http_sse_reply":                                        sameAsServe(),
	"mcp_instructions_in_system_prompt":                         sameAsServe(),
	"mcp_lazy_call_without_select_loads_the_tool":               sameAsServe(),
	"mcp_lazy_search_select_then_call":                          sameAsServe(),
	"mcp_lazy_select_reports_each_name":                         sameAsServe(),
	"mcp_non_text_results_become_text_and_blobs":                reGolden(specMCPText),
	"mcp_paged_tool_list_is_merged":                             sameAsServe(),
	"mcp_per_server_tool_loading_overrides_global":              sameAsServe(),
	"mcp_resources_list_and_read":                               reGolden(specMCPText),
	"mcp_resources_paged_list_is_merged":                        sameAsServe(),
	"mcp_server_lost_mid_session_hides_the_endpoint":            reGolden(specMCPText),
	"mcp_status_reports_connected_and_unavailable_servers":      deletedBy(specMCPStatus),
	"mcp_stdio_server_call":                                     sameAsServe(),
	"mcp_stdio_server_starts_in_configured_dir":                 sameAsServe(),
	"mcp_tool_call_result":                                      sameAsServe(),
	"mcp_tool_error_and_rpc_error_reach_model":                  reGolden(specMCPText),
	"mcp_two_servers_share_a_tool_name":                         sameAsServe(),
	"mcp_unavailable_at_start_then_connect":                     reGolden(specMCPNoStatus),
	"mcp_unavailable_connect_fails_with_classified_reason":      reGolden(specMCPNoStatus, specMCPText, specOpenMCPReason),
	"messages_page_after_compaction":                            pendingOn("F18", "phase 4"),
	"messages_page_windows":                                     pendingOn("phase 4"),
	"one_tool_round_trip":                                       sameAsServe(),
	"openai_key_http_sse_text_turn":                             reGolden(specItems),
	"persisted_queue_dispatches_after_deferred_arm":             deletedBy(specGoalDeferred),
	"plugin_after_hook_sees_output":                             sameAsServe(),
	"plugin_before_hook_rewrites_and_blocks":                    sameAsServe(),
	"plugin_boxes_style_command_and_dir":                        sameAsServe(),
	"plugin_crash_mid_call_session_continues":                   reGolden(specView, specOpenPlugins, "F02"),
	"plugin_event_and_after_hook_payloads":                      sameAsServe(),
	"plugin_system_segment_in_every_request":                    sameAsServe(),
	"plugin_system_transform_reads_session_messages":            sameAsServe(),
	"plugin_tools_listed_and_run":                               reGolden(specView, specOpenPlugins, "F02"),
	"provider_429_then_ok":                                      reGolden(specView, "F02"),
	"provider_5xx_then_ok":                                      reGolden(specView, "F02"),
	"queue_delete_while_busy":                                   pendingOn("F02", "phase 4"),
	"queue_survives_clean_restart_then_delete":                  pendingOn("F02", "phase 4"),
	"queue_survives_clean_restart_then_drains_with_next_prompt": reGolden(specQueue, specHandoffResume, specView, "F02"),
	"queued_input_runs_after_kill":                              reGolden(specCrash, specCrashQueue, specOpenCrashMarker),
	"queued_input_survives_kill":                                deletedBy(specCrashQueue),
	"queued_prompt_runs_before_deferred_auto_arm":               deletedBy(specGoalDeferred),
	"replay_after_kill_full_transcript":                         pendingOn("F02", "phase 4"),
	"send_to_child_and_cancel_tree":                             reGolden(specChildResend, specChildNoGoal, specReceipt, specTaskInputs, specView, "F02"),
	"session_settings_validation_and_persistence":               reGolden(specModelCheck, specErrors, specUpdate, specView, "F02"),
	"sse_resume_after_kill":                                     reGolden(specCursor, specBoxGlobal),
	"sse_resume_cursor":                                         reGolden(specCursor, specBoxGlobal),
	"status_and_list_cold_after_restart":                        pendingOn("F02", specOpenListOrder),
	"steer_joins_the_turn_at_the_tool_boundary":                 reGolden(specQueue, specOpenSteerWrap),
	"stream_stall":                                              reGolden(specView, "F02"),
	"task_child_result_reaches_parent":                          reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"text_reply":                                                sameAsServe(),
	"tool_error_reaches_model":                                  sameAsServe(),
	"two_tool_calls_one_turn":                                   reGolden(specOneResult),
	"two_turns_keep_history":                                    sameAsServe(),
	"usage_survives_a_mid_turn_restart":                         pendingOn("F02"),
}
