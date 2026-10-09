package e2e

// Lines of docs/architecture.md that the re-golden and deleted rows cite.
const (
	specView          = "A `View` is immutable: the `protocol.Session` (status, turn, goal, queue, settings, usage, context gauge, last turn, compaction count, subscription usage, head seq)"
	specUpdate        = "func (s *Session) Update(ctx context.Context, p protocol.SettingsPatch) (protocol.Session, error)"
	specMessages      = "`GET /sessions/{id}/messages?before=&limit=` answers a `protocol.MessagePage` of the conversation that the model reads, oldest first, as the engine answered its message page"
	specBootstrapGone = "The engine bootstrap form (`stream_from`, `live_from`, `seqs`) has no counterpart: one page route and one event cursor replace it."
	specRestartOpens  = "A restart opens no session by itself: the embedder opens each session that has work to resume"
	specErrors        = "Body: `{\"error\":{\"code\":\"...\",\"message\":\"...\",\"details\":{}}}`."
	specReceipt       = "| New id | `201 {input_id, seq}` |"
	specItems         = "| `item.completed` | `item_id`, `turn_id`, `message` (user, assistant, tool result) |"
	specOneResult     = "Every tool call item gets exactly one result item, or an open request, before its turn ends."
	specExternal      = "| Items | External items become `item.completed` | stream-json frames | `item/completed` |"
	specTaskInputs    = "Task notifications become inputs"
	specChildReport   = "The report names the child, its agent, the outcome, and the reason, and holds the last assistant text of the child"
	specChildReason   = "The reason is the classified text of the engine: a fixed prefix by `error_class`, then the error text of the turn end."
	specChildCrash    = "A crashed child turn ends `crashed` and settles `failed`."
	specChildLong     = "A result of at most 4096 bytes stays whole in the report, and so does a larger result that is 4096 bytes or less once masked"
	specChildLost     = "except that a crashed turn reads `lost to restart: turn was in flight when the process last stopped`"
	specChildBound    = "the engine masked the cause with its secret patterns and cut it at 500 runes with `… [truncated]`, and the runtime does the same"
	specChildStatus   = "The `status` and `log` actions of the `task` tool show this reason, as the engine showed its classified reason."
	specChildHint     = "The recover hint is masked the same way and cut at 120 runes."
	specChildHandle   = "the parent retains the masked result as `tool_result.retained` in the append of `child.settled`"
	specChildNoRead   = "the preview ends `… [truncated; full result unavailable]`"
	specChildClaude   = "On Claude Code the engine checked out reports only when a turn started, so a busy parent gets no report in the middle of its turn"
	specChildNoGoal   = "A child session has no `goal` tool"
	specChildAgent    = "`protocol.Session.agent` is the agent profile name that the child was spawned with"
	specChildWording  = "Does a child report to a busy parent keep the task notification of the engine? Yes, built"
	specChildPinned   = "the segment is pinned for the model calls and kept out of history, as the engine pinned it"
	specPinSlot       = "the model reads it at the same place in each later model call"
	specPinReaders    = "it is in no history that a reader sees"
	specPinDrain      = "A drain that holds a prompt and a report gives two messages"
	specPinInTurn     = "A compaction that a running turn makes settles the segment there at once"
	specPinRestart    = "A pin is fixed to the messages around it and replay rebuilds it from the log, so a pin survives a restart."
	specPinCompact    = "A compaction moves each pinned segment that it folds to the end of the history"
	specPinCut        = "A compaction moves a pinned segment after the cut to the end of the kept history, so the next input follows it"
	specPinPaired     = "the runtime keeps the call and its result paired"
	specPinKept       = "so a restart lost the report; the runtime keeps it"
	specAnswerNoSteer = "A run that answers a question takes no steer input"
	specCrash         = "Append `turn.ended{interrupted, crashed}`; keep the partial"
	specCrashQueue    = "The session then starts the next queued input, or waits for input when none is queued."
	specHandoffResume = "A suspended turn has no open tool call, so the next owner resumes it automatically."
	specQueue         = "(queue: next turn; steer: next item boundary)"
	specMCPText       = "By design, an error has no `engine:` prefix, a call to a tool that is not there reads `no such tool available`"
	specMCPNotice     = "The notice is part of the system prompt where the engine pinned it as a message"
	specGoalDeferred  = "The deferred and parked rows are deleted."
	specCursor        = "One per-session `seq` serves paging and SSE resume."
	specEventsRoute   = "GET    /sessions/{id}/events?after=&limit=    page; SSE with Accept: text/event-stream"
	specBoxGlobal     = "| Box-global `events.jsonl` | Delete |"
	specNoProvider    = "A model that no configured provider serves fails with `model_unavailable` at create and at a settings change; an unknown window never does."
	specUnknownWindow = "else the default of 128000 tokens with `WindowEstimated`"
	specThreshold     = "A setting at or below 0 is the default."
	specOverflowFails = "When no turn can fold or the summary fails, the turn fails."
	specCompactResult = "`Compact()` returns `protocol.Compacted`"
	specChildResend   = "changes by design in one way: a later send is not refused"
	specEndTree       = "stops the turn of each descendant that this runtime runs, as the tree interrupt does, and each of those turns ends with cause `ended`."
	specClearGoal     = "Stops the turn with `goal_cleared` while the goal is active"
	specWaiting       = "| `waiting` | A turn ended `awaiting_input`; a request is open |"
	specWarm          = "The session calls it once on create and on wake, fire-and-forget under the session context."
	specCompactOwned  = "A backend with `OwnsContext` runs `/compact` as a turn. The backend logs `compaction.applied` with `by_backend: true`."

	specClaudeGauge   = "A Claude Code turn reads its context gauge and its cost from the `result` frame, as the engine did"
	specClaudeOnce    = "A failed Claude Code turn runs the CLI once and fails with the text of the `result` frame"
	specHistoryBridge = "When another provider recorded a message after the newest message that this backend saw"
	specBackendState  = "Private backend state is a chain of `backend.state` records"
	specMirrorBlob    = "The entries, the Claude Code transcript mirror, are appended as chunk blobs"
	specKeepOwned     = "A `keep_turns` below 1, or any `keep_turns` for a backend with `OwnsContext`, is `invalid_request`."

	specErrorText    = "the actor masks and bounds each error text that it writes to the log"
	specGoalFailed   = "An error the user must fix yields `failed`."
	specNoParkedGoal = "There is no deferred goal and no parked goal."

	specPromptSwitch = "At the switch, the `runtime_prompt` contract rows change in two ways"
	specBadFileSkip  = "a bad file degrades instead of failing the turn"
	specNoBatching   = "no batching segment follows the base prompt"
	specBlankJoin    = "The runtime joins them with a blank line."
	specMCPNoLeak    = "An error that is not the server's own RPC error names a reason, never the endpoint URL or a response body."
	specRetainBound  = "`read_tool_result` reads the blob back by line window or literal search, bounded by `max_bytes`."
	specNoReader     = "A turn whose allowed tools omit `read_tool_result` retains nothing, so a preview never names a tool that the model cannot call."
	specRetainIndex  = "Each compaction summary ends with an index of the newest 32 retained results, so a handle stays reachable after its preview folds."
	specFileCap      = "A file-size cap of 20 MiB bounds one file:"
	specToolImages   = "A reader of the history that shows text (`View.Messages`, `get_conversation_history`, the `log` of `task`) shows the text of the result and no blob."
	specProfileSkip  = "A file with a key that the format does not know is skipped with a WARN log line, as in the engine."
	specProfileFail  = "Any other file that is not valid, such as one with no `name`, fails the load, and the error names the file, with no `engine:` prefix"
	specProfileModel = "`model` (a ref or an alias; omitted or `inherit` keeps the model of the parent)"
	specProfileColor = "`color` is read and ignored"
	specProfileSwap  = "beside the built-in profiles, which a file of the same name replaces"
	specFileSkipped  = "A file that cannot be read, is empty, or is not UTF-8 is skipped, and so is a skill that is not valid or repeats a name."

	specContinuation  = "in `<harness-engine-context>` tags, so the model reads it as engine text"
	specListOrder     = "list in creation order"
	specStatusRoute   = "| `/wait`, `/request`, `/session/status`, `/event/tip` | Delete; the new API covers them |"
	specAnswerReceipt = "an answer replies 202 {seq, status}, a dismissal 204"

	specStopped        = "Keep the partial; unfinished tool calls get `interrupted` results; the next queued input runs"
	specInterrupt      = "`interrupt` stops the running run (a turn, an evaluation, or a compaction). The next queued input then starts"
	specSameBody       = "| Same id, same body | `200` with the original receipt |"
	specOtherBody      = "| Same id, other body | `409 input_conflict` |"
	specTurnMismatch   = "A `steer` input with `expected_turn_id` fails with `turn_mismatch` if that turn is not running."
	specRetryable      = "`ErrRetryable` (a 429, a 5xx, a truncated stream, a response with no output) calls the model again after a wait of 1 s that doubles up to 8 s, with jitter, up to `prompt_retries` times"
	specModelsRoute    = "GET    /models                                models and their capabilities"
	specHandoff        = "admit no new tool call, let running tools finish within the budget, append `turn.suspended`"
	specPromptOnce     = "It reads them once, when the session is created or opened, and sends them as `turn.Request.Instructions` on each model call."
	specRepeatName     = "fails the load the same way, and the error names both files."
	specMidTurnFails   = "When the new model has another kind of backend, one that owns its loop or one that does not, the turn fails at its next model call"
	specTypedReceipt   = "A typed slash command answers the same way. Its receipt adds `command`, the newest status of the command"
	specCmdRepeat      = "A repeat of the input ID with the same line returns the newest status; another line, or an input ID of another input, is `input_conflict`."
	specCmdOps         = "`model`, `thinking`, and `tier` are `Update`"
	specCmdFailed      = "`failed` (the error text of a sentinel error"
	specCmdResult      = "Its result is the `protocol.Compacted` of `Compact`."
	specCmdInterrupt   = "`Open` records `interrupted` for each command that an earlier owner accepted and never finished"
	specCmdSwitch      = "Switch oracle: `builtin_commands_run_and_record`, with the receipt, the record, and the routes in the new shape."
	specCmdUnsupport   = "A frontend command, or a control command with no operation here (`queue-clear`): `unsupported`"
	specCmdMenuRoutes  = "A control command names its operation and `available_during_task`; the handler adds the route of the same operation, where one exists."
	specGoalOwnTurn    = "So the turn that calls `set` is not judged; the condition runs as a turn of its own after it"
	specGoalPrompt     = "Its prompt copies the engine prompt, with a third form for `impossible`."
	specGoalWording    = "The tool copies the engine description and error wording, with the `engine:` prefix and the route names of the engine."
	specGoalNoEval     = "`SetGoal` without it is an invalid request."
	specOverflowFolds  = "A model call that overflows the context window compacts while its turn runs, for a backend without `OwnsContext`, and the turn calls the model again on the new history."
	specLimitFails     = "A spawn past `max_concurrent_tasks` or `max_tree_tokens` fails the tool call with the text of the engine"
	specTaskWithheld   = "A session at `max_task_depth` has no `task` tool, as in the engine, so a call to it reads as a call to a tool that is not there."
	specGoalTranscript = "The transcript that it reads shows a tool call as `[tool call <name>] <arguments>`"
	specGoalAdjust     = "Does `adjust` inside a goal turn judge that turn on the old condition, as the engine did?"
	specGoalAdjustPost = "A condition that `set` posted and that still waits is replaced by the adjusted one, so the turn of its own runs the adjusted condition."
	specTaskWording    = "The `task` tool keeps the engine actions and wording."
	specReadOnlyKinds  = "The built-in `explore` and `plan` allow only the read-only file tools, and `plan` asks for an implementation plan."
	specCancelReport   = "Does `cancel` of a child report its canceled grandchild to the nearest live ancestor, as the engine did?"
	specProfileKnown   = "a spawn keeps only the names of a profile that `known` accepts for the model of the child"
	specCrashMarker    = "Does the switch keep the crash marker? Yes, built"
	specDismissed      = "A dismissal closes the call with an error result that says the user dismissed the question."
	specNoStart        = "That turn has no input; a dismissal starts none."
	specAnswerMap      = "A question takes an answer that maps each question to text."
	specProviderSwap   = "A settings change to a model of another provider dismisses it too."
	specRequestRoute   = "POST   /sessions/{id}/requests/{request}      {answer} | {dismiss}"
	specProcessUnknown = "An unknown name, or any name without a `WorkDir`, is `process_not_found`."
	specNoRoute        = "A path or method that no route serves answers 404 or 405 with `invalid_request`."
	specGitOracle      = "Switch oracle: the `git_changes_*` rows, with the route renamed and the error body in the new shape."
	specBannerPrefix   = "Each request is a prefix of the next, also after a compaction in the middle of a turn, and a compaction can only move the place earlier."
)

// Lines of docs/architecture.md that the session rows cite.
const (
	specWindow         = "of the window of the session model, or of the window of the reading when the model reports none"
	specFailedSummary  = "A failed summary appends nothing, and the turn starts on the full history."
	specOpenStarts     = "The next owner thus runs the input that waited for a stopped summary"
	specInterruptTable = "| `Interrupt` | Stops the turn, and ends an active goal | Stops it, and ends an active goal | Stops it, and ends an active goal |"
	specCompactBusy    = "| `Compact` | `session_busy` | `session_busy` | `session_busy` |"
	specOverflowTwice  = "With no new input in the turn, a second overflow fails it"
	specFailedRuns     = "After any other failed turn, the next queued input runs, as after a completed turn."
	specExhaustedHolds = "Only `provider_exhausted` leaves the queue waiting, and `Open` follows the same rule"
	specExhaustedQueue = "queued inputs wait for the next input"
	specGoalImpossible = "`met` yields `achieved`, and `impossible` yields `failed`."
	specGoalBusy       = "`SetGoal` while a turn runs, or with queued input, starts nothing. The next turn that ends is the first one evaluated."
	specGoalClear      = "`ClearGoal` during a goal turn or its evaluation stops it with cause `goal_cleared` and returns after it ends."
	specGoalWithdraw   = "`SetGoal`, `ClearGoal`, each verdict, and each pause or failure withdraw the queued inputs with `source: goal`."
	specGoalRestart    = "An `active` goal on an idle session judges the last turn when the goal has not judged it"
	specNarrow         = "and the allowed tools of the parent narrowed by the profile"
	specAskRule        = "Claude Code asks with `AskUserQuestion` when `Options.AskUserQuestion` is set and the session is not a child and has no active goal."
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
	"mcp_connect_adds_tools_but_not_instructions":                                                              reGolden(specMCPNotice),
	"skills_listed_sorted_and_read_through_read_file":                                                          reGolden(specNoBatching, specBlankJoin),
	"skills_dirs_config_replaces_the_default_dir":                                                              reGolden(specNoBatching, specBlankJoin),
	"skills_from_several_dirs_are_listed_sorted_by_name":                                                       reGolden(specNoBatching, specBlankJoin),
	"skills_dirs_empty_list_disables_discovery":                                                                reGolden(specNoBatching),
	"instructions_single_file_at_workdir":                                                                      reGolden(specNoBatching, specBlankJoin),
	"instructions_chain_runs_root_to_workdir_and_stops_at_the_git_root":                                        reGolden(specNoBatching, specBlankJoin),
	"instructions_outside_a_repository_read_only_the_workdir_file":                                             reGolden(specNoBatching, specBlankJoin),
	"instructions_oversize_with_headings_become_an_outline_read_by_range":                                      reGolden(specNoBatching, specBlankJoin),
	"instructions_mode_full_keeps_the_truncation_marker":                                                       reGolden(specNoBatching, specBlankJoin),
	"instructions_chain_over_four_times_the_cap_drops_the_middle_files_nearest_the_root":                       reGolden(specNoBatching, specBlankJoin),
	"instructions_first_section_over_the_cap_is_cut_with_the_marker_and_the_rest_is_outlined":                  reGolden(specNoBatching, specBlankJoin),
	"instructions_outline_drops_the_teasers_over_its_budget":                                                   reGolden(specNoBatching, specBlankJoin),
	"instructions_mode_full_from_the_environment_keeps_the_truncation_marker":                                  reGolden(specNoBatching, specBlankJoin),
	"instructions_mode_other_than_full_keeps_the_outline":                                                      reGolden(specNoBatching, specBlankJoin),
	"instructions_oversize_without_headings_keep_the_truncation_marker":                                        reGolden(specNoBatching, specBlankJoin),
	"instructions_300_kb_file_with_headings_keeps_the_sections_that_fit_the_default_cap_and_outlines_the_rest": reGolden(specNoBatching, specBlankJoin),
	"instructions_300_kb_file_without_headings_is_cut_at_the_default_cap_with_the_marker":                      reGolden(specNoBatching, specBlankJoin),
	"instructions_300_kb_file_with_one_huge_first_section_is_cut_with_the_marker_and_the_rest_is_outlined":     reGolden(specNoBatching, specBlankJoin),
	"instructions_negative_max_bytes_keeps_the_whole_file":                                                     reGolden(specNoBatching, specBlankJoin),
	"instructions_false_in_config_injects_nothing":                                                             reGolden(specNoBatching),
	"instructions_path_in_config_replaces_discovery":                                                           reGolden(specNoBatching, specBlankJoin),
	"system_segments_order_append_layers_then_instructions_then_skills":                                        reGolden(specNoBatching, specBlankJoin),
	"instructions_empty_file_fails_the_turn":                                                                   reGolden(specFileSkipped, specBadFileSkip, specNoBatching, specView),
	"instructions_empty_and_invalid_files_in_a_chain_fail_the_turn":                                            reGolden(specFileSkipped, specBadFileSkip, specNoBatching, specView, specBlankJoin),
	"skill_with_an_uppercase_name_fails_the_turn":                                                              reGolden(specFileSkipped, specBadFileSkip, specNoBatching, specBlankJoin),
	"skill_named_unlike_its_directory_fails_the_turn":                                                          reGolden(specFileSkipped, specBadFileSkip, specNoBatching, specView),
	"task_profiles_skip_bad_files":                                                                             reGolden(specProfileSkip, specProfileFail, specOneResult, specView),
	"task_profile_file_with_model_inherit_and_color_is_a_profile":                                              reGolden(specProfileModel, specProfileColor, specTaskInputs, specChildReport),
	"task_profile_file_replaces_a_built_in_profile":                                                            reGolden(specProfileSwap, specTaskInputs, specChildReport),
	"two_slow_tool_calls_of_one_message_overlap_and_log_in_call_order":                                         sameAsServe(),
	"interrupt_cancels_every_running_tool_call_of_a_batch":                                                     sameAsServe(),
	"interrupt_records_the_result_of_a_call_that_finished_beside_a_held_one":                                   sameAsServe(),
	"retained_results_of_one_batch_keep_their_own_handles":                                                     sameAsServe(),
	"calls_on_one_file_run_in_call_order_within_a_batch":                                                       sameAsServe(),
	"plugin_hooks_run_for_each_call_of_a_batch":                                                                sameAsServe(),
	"file_tools_size_cap":                                                  reGolden(specFileCap),
	"bash_output_and_exit_status":                                          sameAsServe(),
	"read_file_returns_an_image":                                           reGolden(specToolImages),
	"a_tool_result_image_reaches_the_model_after_a_restart":                sameAsServe(),
	"a_missing_tool_result_image_leaves_the_text_for_the_model":            sameAsServe(),
	"write_guard_belongs_to_one_session":                                   sameAsServe(),
	"mcp_tool_action_refusals":                                             reGolden(specMCPNotice, specMCPText),
	"mcp_refused_call_hides_the_response_body":                             reGolden(specMCPNoLeak, specMCPText),
	"mcp_select_of_a_down_server_is_pending":                               reGolden(specMCPNotice),
	"mcp_search_ranks_the_tools":                                           sameAsServe(),
	"tool_result_retention_keeps_a_preview_and_reads_it_back":              sameAsServe(),
	"tool_result_over_the_session_budget_keeps_a_preview_with_a_notice":    sameAsServe(),
	"read_tool_result_bounds_the_default_budget":                           reGolden(specRetainBound),
	"tool_result_handles_continue_across_turns_and_a_restart":              reGolden(specRetainIndex, specCompactResult),
	"tool_result_retention_keeps_the_plugin_hooked_result":                 sameAsServe(),
	"a_child_without_read_tool_result_retains_nothing":                     reGolden(specNoReader, specTaskInputs, specChildReport),
	"auto_compact_on_threshold":                                            reGolden(specView),
	"bifrost_429_then_ok":                                                  reGolden(specItems, specView),
	"bifrost_context_overflow":                                             reGolden(specView, specOverflowFails),
	"bifrost_goal_met_first_turn":                                          reGolden(specItems),
	"bifrost_goal_not_met_then_met":                                        reGolden(specItems),
	"bifrost_max_tokens_continuation":                                      reGolden(specView, specItems, specContinuation),
	"bifrost_prompt_attachments":                                           reGolden(specItems),
	"bifrost_reasoning_and_effort":                                         reGolden(specItems, specUpdate),
	"bifrost_text_reply":                                                   reGolden(specItems),
	"bifrost_tool_error_marker":                                            reGolden(specItems),
	"bifrost_tool_round_trip":                                              reGolden(specItems),
	"bifrost_two_tool_calls_one_turn":                                      reGolden(specItems, specOneResult),
	"bootstrap_cold_then_resident_windows":                                 reGolden(specMessages, specBootstrapGone, specErrors),
	"bootstrap_cold_window_after_kill":                                     reGolden(specMessages, specBootstrapGone, specErrors),
	"builtin_commands_run_and_record":                                      reGolden(specCmdSwitch, specTypedReceipt, specCmdMenuRoutes),
	"busy_deferred_goal_with_max_turns":                                    deletedBy(specGoalDeferred),
	"child_crash_recovered":                                                reGolden(specView, specCrash, specChildReport, specChildAgent),
	"child_crash_reaches_a_busy_parent":                                    reGolden(specView, specCrash, specChildReport, specChildCrash, specChildLost, specChildWording, specChildAgent),
	"child_usage_limit_delivered":                                          reGolden(specTaskInputs, specChildReport, specChildReason, specChildNoGoal, specView, specChildAgent),
	"child_error_delivered":                                                reGolden(specTaskInputs, specChildReport, specChildReason, specChildNoGoal, specView, specChildAgent),
	"child_report_reaches_a_busy_parent_at_the_tool_boundary":              reGolden(specChildReport, specChildWording),
	"child_error_reaches_a_busy_parent_at_the_tool_boundary":               reGolden(specChildReport, specChildReason, specChildWording),
	"child_usage_limit_reaches_a_busy_parent":                              reGolden(specChildReport, specChildReason, specChildWording),
	"child_report_to_a_busy_parent_keeps_its_slot_in_the_next_turn":        reGolden(specChildReport, specChildWording, specChildPinned, specPinSlot),
	"child_report_to_a_busy_parent_stays_through_a_compaction":             reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult),
	"prompt_and_child_report_in_one_drain_are_two_messages":                reGolden(specChildReport, specChildWording, specChildPinned, specPinDrain),
	"task_log_of_a_child_shows_no_pinned_report":                           reGolden(specTaskInputs, specChildReport, specChildWording, specChildPinned, specPinReaders, specItems, specOneResult),
	"child_report_to_a_busy_parent_after_the_cut_of_a_compaction":          reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult, specPinCut),
	"child_report_to_a_busy_parent_folded_with_a_long_kept_tail":           reGolden(specChildReport, specChildWording, specChildPinned, specPinCompact, specCompactResult, specPinPaired),
	"child_report_to_a_busy_parent_stays_through_an_in_turn_compaction":    reGolden(specChildReport, specChildWording, specChildPinned, specPinInTurn, specOverflowFolds),
	"child_report_to_a_busy_parent_survives_a_restart":                     reGolden(specChildReport, specChildWording, specChildPinned, specPinRestart, specPinKept),
	"child_rate_limit_reaches_a_busy_parent":                               reGolden(specChildReport, specChildReason, specChildWording),
	"child_result_within_the_byte_limit_reaches_a_busy_parent":             reGolden(specChildReport, specChildLong, specChildWording),
	"child_usage_limit_with_a_long_hint_reaches_a_busy_parent":             reGolden(specChildReport, specChildReason, specChildHint, specChildWording),
	"child_long_result_handle_reads_back":                                  reGolden(specChildReport, specChildLong, specChildHandle, specChildWording),
	"claudecode_long_child_result_has_no_readable_handle":                  sameAsServe(),
	"child_long_error_reaches_a_busy_parent":                               reGolden(specChildReport, specChildReason, specChildBound, specChildWording),
	"child_long_result_reaches_a_busy_parent":                              reGolden(specChildReport, specChildLong, specChildWording),
	"claudecode_compact_delegated":                                         reGolden(specView, specCompactOwned, specCompactResult, specClaudeGauge, specRestartOpens),
	"claudecode_child_report_waits_for_the_next_turn":                      sameAsServe(),
	"claudecode_child_reports_share_the_next_turn":                         sameAsServe(),
	"claudecode_queued_prompt_and_child_report_share_a_turn":               reGolden(specQueue, specChildReport, specChildClaude),
	"claudecode_cli_gets_the_tools_of_the_engine_bridge":                   sameAsServe(),
	"claudecode_bridge_model_tool_offers_list_only":                        sameAsServe(),
	"claudecode_bridge_refuses_set_on_the_model_tool":                      sameAsServe(),
	"claudecode_configured_mcp_servers_reach_the_cli":                      sameAsServe(),
	"claudecode_context_window_from_model_usage":                           reGolden(specView, specClaudeGauge),
	"claudecode_compact_after_tokens_reads_zero":                           reGolden(specView, specClaudeGauge),
	"claudecode_compact_keeps_the_window":                                  reGolden(specView, specClaudeGauge),
	"claudecode_cli_exit_runs_once":                                        reGolden(specView, specClaudeOnce),
	"claudecode_error_result_fails_turn":                                   reGolden(specView, specClaudeOnce, specClaudeGauge),
	"claudecode_error_during_execution_pauses_a_goal_and_keeps_its_errors": sameAsServe(),
	"claudecode_credential_refusal_in_errors_fails_a_goal":                 sameAsServe(),
	"claudecode_history_bridge_after_native_turn":                          reGolden(specUpdate, specClaudeGauge),
	"claudecode_interrupt_mid_turn":                                        reGolden(specView),
	"claudecode_question_dismissed_by_compact":                             reGolden(specWaiting, specView, specCompactResult, specClaudeGauge),
	"claudecode_question_dismissed_by_next_prompt":                         reGolden(specWaiting, specView, specClaudeGauge),
	"claudecode_question_answer_run_takes_no_steer_input":                  reGolden(specWaiting, specView, specAnswerNoSteer, specAnswerReceipt, specClaudeGauge),
	"claudecode_question_parks_then_answer_resumes":                        reGolden(specWaiting, specView, specAnswerReceipt, specClaudeGauge),
	"claudecode_question_unknown_call_id_conflicts":                        reGolden(specWaiting, specView, specErrors, specAnswerReceipt, specClaudeGauge),
	"claudecode_prompt_attachments":                                        sameAsServe(),
	"claudecode_queued_prompt_injected_mid_turn":                           reGolden(specView, specClaudeGauge),
	"claudecode_rate_limit_event_reaches_subscription_usage":               reGolden(specView, specClaudeGauge),
	"claudecode_resume_across_turns":                                       sameAsServe(),
	"claudecode_resume_survives_restart":                                   sameAsServe(),
	"claudecode_subagent_frames_keep_parent":                               sameAsServe(),
	"claudecode_thinking_block_is_reasoning":                               reGolden(specExternal, specView, specClaudeGauge),
	"claudecode_turn_text_and_tool":                                        reGolden(specView, specClaudeGauge),
	"codex_http_mcp_tool_schema_is_sanitized":                              reGolden(specItems),
	"codex_http_reasoning_replays_on_tool_round_trip":                      reGolden(specItems),
	"codex_http_sse_text_turn":                                             reGolden(specItems),
	"codex_http_sse_tool_round_trip_resends_history":                       reGolden(specItems),
	"codex_ws_chain_miss_resends_full_history":                             reGolden(specItems, specWarm),
	"codex_ws_chains_two_turns":                                            reGolden(specItems, specWarm),
	"codex_ws_drop_mid_turn_resends_full_history":                          reGolden(specView, specItems),
	"codex_ws_effort_sets_reasoning_effort":                                reGolden(specView, specItems, specUpdate),
	"codex_ws_prewarm_warms_first_turn":                                    reGolden(specItems, specWarm),
	"codex_ws_reasoning_chains_tool_round_trip":                            reGolden(specItems, specWarm),
	"codex_ws_refused_falls_back_to_http":                                  reGolden(specItems, specWarm),
	"codex_ws_tool_round_trip":                                             reGolden(specItems, specWarm),
	"codex_ws_uncoded_chain_miss_resends_full_history":                     reGolden(specItems, specWarm),
	"codex_ws_usage_frame_reaches_session":                                 reGolden(specView, specItems),
	"codex_http_usage_headers_reach_session":                               reGolden(specView, specItems),
	"compact_manual":                                                       reGolden(specMessages, specBootstrapGone, specErrors),
	"compact_survives_restart":                                             reGolden(specMessages, specBootstrapGone, specErrors, specRestartOpens),
	"context_overflow":                                                     reGolden(specView, specOverflowFails),
	"deferred_goal_judges_finished_turn":                                   deletedBy(specGoalDeferred),
	"deferred_goal_with_max_turns":                                         deletedBy(specGoalDeferred),
	"driver_child_send_and_cancel":                                         reGolden(specTaskInputs, specChildNoGoal, specReceipt, specView, specChildAgent),
	"driver_clean_restart":                                                 reGolden(specView, specRestartOpens),
	"driver_compact":                                                       reGolden(specView, specCompactResult),
	"driver_queue_goal_and_end":                                            reGolden(specView, specReceipt, specClearGoal),
	"driver_resume_streams":                                                reGolden(specCursor, specBoxGlobal),
	"driver_settings_and_reads":                                            reGolden(specMessages, specBootstrapGone, specErrors),
	"end_then_send_runs_no_report_of_the_stopped_child":                    reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal),
	"end_then_open_before_the_child_turn_ends_runs_no_report":              reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal),
	"end_idle_parent_cancels_running_child":                                reGolden(specView, specEndTree, specTaskInputs, specChildNoGoal, specChildAgent),
	"end_session_semantics":                                                reGolden(specView, specErrors, specReceipt),
	"enqueue_joins_the_turn_at_the_tool_boundary":                          sameAsServe(),
	"enqueue_while_busy_runs_after":                                        sameAsServe(),
	"file_tools_read_edges":                                                sameAsServe(),
	"file_tools_roundtrip":                                                 sameAsServe(),
	"file_tools_search_edges":                                              sameAsServe(),
	"file_tools_write_edit_guards":                                         sameAsServe(),
	"goal_busy_send_is_queued":                                             reGolden(specView),
	"goal_cleared_before_first_turn":                                       deletedBy(specGoalDeferred),
	"goal_exhausts_max_turns":                                              reGolden(specView),
	"goal_met_first_turn":                                                  sameAsServe(),
	"goal_not_met_then_met":                                                sameAsServe(),
	"goal_provider_exhausted_parks":                                        deletedBy(specGoalDeferred),
	"goal_update_deferred_goal_waits_for_first_turn":                       deletedBy(specGoalDeferred),
	"goal_update_while_busy":                                               reGolden(specView),
	"history_survives_clean_restart":                                       sameAsServe(),
	"interrupt_drops_unfinished_text_then_queue_continues":                 sameAsServe(),
	"interrupt_idle_is_noop":                                               sameAsServe(),
	"journal_pages_follow_cursor":                                          reGolden(specCursor, specEventsRoute),
	"kill_mid_turn_then_continue":                                          sameAsServe(),
	"max_tokens_continuation":                                              reGolden(specView, specContinuation),
	"mcp_auto_default_threshold_defers_at_21_tools":                        sameAsServe(),
	"mcp_auto_default_threshold_stays_eager_at_20_tools":                   sameAsServe(),
	"mcp_auto_defers_over_threshold":                                       sameAsServe(),
	"mcp_auto_stays_eager_under_threshold":                                 sameAsServe(),
	"mcp_eager_lists_namespaced_tools":                                     sameAsServe(),
	"mcp_http_sse_reply":                                                   sameAsServe(),
	"mcp_instructions_in_system_prompt":                                    sameAsServe(),
	"mcp_lazy_call_without_select_loads_the_tool":                          sameAsServe(),
	"mcp_lazy_search_select_then_call":                                     sameAsServe(),
	"mcp_lazy_select_reports_each_name":                                    sameAsServe(),
	"mcp_non_text_results_become_text_and_blobs":                           reGolden(specToolImages),
	"mcp_paged_tool_list_is_merged":                                        sameAsServe(),
	"mcp_per_server_tool_loading_overrides_global":                         sameAsServe(),
	"mcp_resources_list_and_read":                                          reGolden(specMCPText),
	"mcp_resources_paged_list_is_merged":                                   sameAsServe(),
	"mcp_server_lost_mid_session_hides_the_endpoint":                       reGolden(specMCPText),
	"mcp_status_reports_connected_and_unavailable_servers":                 reGolden(specMCPNotice),
	"mcp_stdio_server_call":                                                sameAsServe(),
	"mcp_stdio_server_starts_in_configured_dir":                            sameAsServe(),
	"mcp_tool_call_result":                                                 sameAsServe(),
	"mcp_tool_error_and_rpc_error_reach_model":                             reGolden(specMCPText),
	"mcp_two_servers_share_a_tool_name":                                    sameAsServe(),
	"mcp_unavailable_at_start_then_connect":                                reGolden(specMCPNotice),
	"mcp_unavailable_connect_fails_with_classified_reason":                 reGolden(specMCPNotice, specMCPText),
	"messages_page_after_compaction":                                       reGolden(specMessages, specBootstrapGone, specErrors),
	"messages_page_windows":                                                reGolden(specMessages, specBootstrapGone, specErrors),
	"model_tool_false_removes_the_model_tool":                              sameAsServe(),
	"model_tool_lists_the_registry_and_sets_native":                        sameAsServe(),
	"model_tool_reports_lists_and_switches_the_model":                      sameAsServe(),
	"one_tool_round_trip":                                                  sameAsServe(),
	"openai_key_http_sse_text_turn":                                        reGolden(specItems),
	"persisted_queue_dispatches_after_deferred_arm":                        deletedBy(specGoalDeferred),
	"plugin_after_hook_sees_output":                                        sameAsServe(),
	"plugin_before_hook_rewrites_and_blocks":                               sameAsServe(),
	"plugin_chat_params_hook_sets_the_cap_and_sampling_of_a_model_call":    sameAsServe(),
	"plugin_shell_env_hook_sets_the_environment_of_a_bash_command":         sameAsServe(),
	"plugin_boxes_style_command_and_dir":                                   sameAsServe(),
	"plugin_crash_mid_call_session_continues":                              reGolden(specView),
	"plugin_event_and_after_hook_payloads":                                 sameAsServe(),
	"plugin_system_segment_in_every_request":                               sameAsServe(),
	"plugin_system_transform_reads_session_messages":                       sameAsServe(),
	"plugin_session_messages_carry_attachments":                            sameAsServe(),
	"plugin_tools_listed_and_run":                                          reGolden(specView),
	"provider_429_then_ok":                                                 reGolden(specView),
	"provider_usage_limit_fails_turn":                                      reGolden(specView),
	"provider_5xx_then_ok":                                                 reGolden(specView),
	"provider_error_text_is_masked_and_bounded":                            reGolden(specView, specErrorText, specGoalFailed, specNoParkedGoal),
	"queue_delete_while_busy":                                              reGolden(specView),
	"queue_survives_clean_restart_then_delete":                             reGolden(specView, specHandoffResume),
	"queue_survives_clean_restart_then_drains_with_next_prompt":            reGolden(specQueue, specHandoffResume, specView),
	"queued_input_runs_after_kill":                                         reGolden(specCrash, specCrashQueue),
	"queued_input_survives_kill":                                           deletedBy(specCrashQueue),
	"queued_prompt_runs_before_deferred_auto_arm":                          deletedBy(specGoalDeferred),
	"replay_after_kill_full_transcript":                                    reGolden(specMessages, specBootstrapGone, specErrors),
	"send_to_child_and_cancel_tree":                                        reGolden(specChildResend, specChildNoGoal, specReceipt, specTaskInputs, specView, specChildAgent),
	"settings_model_change_reaches_the_next_model_call_of_a_turn":          reGolden(specUpdate),
	"session_info_reports_the_session":                                     reGolden(specPromptSwitch),
	"session_info_reports_what_the_session_loaded":                         reGolden(specPromptSwitch),
	"session_info_reports_the_plugin_and_its_system_segment":               reGolden(specPromptSwitch),
	"session_settings_validation_and_persistence":                          reGolden(specNoProvider, specErrors, specUpdate, specView, specRestartOpens),
	"sse_resume_after_kill":                                                reGolden(specCursor, specBoxGlobal, specRestartOpens),
	"sse_resume_cursor":                                                    reGolden(specCursor, specBoxGlobal),
	"list_sessions_in_creation_order":                                      reGolden(specView, specListOrder, specRestartOpens),
	"status_and_list_cold_after_restart":                                   reGolden(specView, specListOrder, specStatusRoute, specRestartOpens),
	"steer_joins_the_turn_at_the_tool_boundary":                            sameAsServe(),
	"stream_stall":                                      reGolden(specView),
	"task_child_result_reaches_parent":                  reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_spawn_runs_the_child_on_its_model_and_effort": reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView, specChildAgent),
	"text_reply":                                        sameAsServe(),
	"tool_error_reaches_model":                          sameAsServe(),
	"two_tool_calls_one_turn":                           reGolden(specOneResult),
	"two_turns_keep_history":                            sameAsServe(),
	"usage_survives_a_mid_turn_restart":                 reGolden(specView, specHandoffResume),

	"end_is_refused_while_a_typed_command_runs":                       reGolden(specTypedReceipt, specErrors, specReceipt),
	"a_kill_interrupts_an_unfinished_command":                         reGolden(specTypedReceipt, specCmdRepeat, specCmdInterrupt, specReceipt),
	"banner_holds_its_place_when_a_turn_compacts_in_the_middle":       reGolden(specOverflowFolds, specBannerPrefix, specView),
	"codex_http_truncated_and_empty_responses_are_retried":            reGolden(specView, specItems, specRetryable),
	"codex_service_tier_reaches_the_request":                          reGolden(specView, specItems, specUpdate),
	"codex_settings_switch_to_another_provider_keeps_the_history":     reGolden(specView, specItems, specUpdate),
	"commands_menu_lists_builtin_and_prompt_commands":                 reGolden(specCmdMenuRoutes, specCmdUnsupport),
	"create_checks_the_provider":                                      reGolden(specNoProvider, specUnknownWindow, specErrors, specView),
	"create_without_a_model_takes_the_default_model":                  reGolden(specView),
	"create_takes_an_unknown_model_with_a_configured_window":          reGolden(specView),
	"goal_tool_actions_report_and_refuse":                             reGolden(specView, specGoalTranscript),
	"goal_tool_adjust_after_set_runs_the_adjusted_condition":          reGolden(specView, specGoalOwnTurn, specGoalAdjustPost),
	"goal_tool_adjust_keeps_the_turn_limit":                           pendingOn(specGoalAdjust),
	"goal_tool_refusals_copy_the_engine_wording":                      reGolden(specView, specGoalWording),
	"goal_tool_set_runs_the_condition_as_its_own_turn":                reGolden(specView, specGoalOwnTurn, specGoalTranscript),
	"input_receipts_and_conflicts":                                    reGolden(specReceipt, specSameBody, specOtherBody, specTurnMismatch, specErrors),
	"instructions_are_read_when_the_session_starts":                   reGolden(specPromptOnce),
	"interrupt_cuts_a_running_tool_then_queue_continues":              reGolden(specStopped, specInterrupt),
	"interrupt_during_retry_backoff_ends_the_turn":                    reGolden(specView, specStopped),
	"models_lists_the_configured_providers":                           reGolden(specModelsRoute, specView),
	"no_goal_evaluator_means_no_goal":                                 reGolden(specGoalNoEval, specErrors, specCmdFailed, specView),
	"plugin_inventory_reports_not_spawned_then_running":               reGolden(specView),
	"plugin_sees_the_model_of_each_call":                              reGolden(specUpdate),
	"restart_lets_a_running_tool_finish_and_cuts_the_next_alone_call": reGolden(specView, specHandoff),
	"retries_stop_after_prompt_retries":                               reGolden(specView, specRetryable),
	"settings_change_to_claude_code_mid_turn_fails_the_turn":          reGolden(specUpdate, specView, specMidTurnFails, specClaudeGauge),
	"typed_commands_record_their_outcome":                             reGolden(specTypedReceipt, specCmdRepeat, specCmdOps, specCmdFailed, specReceipt),
	"typed_compact_keeps_keep_turns_and_returns_the_range":            reGolden(specTypedReceipt, specCmdResult, specReceipt),
	"unknown_tool_call_gets_an_error_result":                          reGolden(specMCPText),
	"task_profile_sets_the_tools_model_and_prompt_of_the_child":       sameAsServe(),
	"task_spawn_fails_on_an_agent_name_repeated_in_one_dir":           reGolden(specRepeatName, specErrors, specView),
	"task_spawn_fails_on_an_agent_name_repeated_across_dirs":          reGolden(specRepeatName, specErrors, specView),
	"session_of_a_child_opens_after_an_agent_name_is_repeated":        reGolden(specTaskInputs, specChildReport, specChildNoGoal, specReceipt, specView, specChildAgent),
	"agent_defs_dirs_replace_the_default_profile_dir":                 reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_explore_and_plan_children_get_read_only_tools":              sameAsServe(),
	"task_refusals":                                                      reGolden(specTaskWording, specOneResult),
	"task_refusal_past_max_task_depth":                                   reGolden(specTaskWithheld, specMCPText, specChildNoGoal),
	"task_refusal_past_max_concurrent_tasks":                             reGolden(specLimitFails, specChildNoGoal),
	"task_status_and_log_of_a_failed_child":                              reGolden(specChildReport, specChildReason, specChildBound, specChildStatus, specItems, specOneResult),
	"task_status_and_log_of_a_settled_child":                             reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_cancel_and_send_to_a_running_child":                            reGolden(specTaskInputs, specChildWording, specChildNoGoal, specItems, specOneResult),
	"task_spawn_past_max_tree_tokens_is_refused":                         reGolden(specLimitFails, specChildNoGoal),
	"task_send_runs_a_settled_child_again":                               reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_two_sends_to_a_settled_child_need_one_slot":                    reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_action_refusals":                                               reGolden(specTaskInputs, specChildReport, specChildNoGoal, specItems, specOneResult),
	"task_tree_reaches_a_grandchild":                                     reGolden(specTaskInputs, specChildReport, specChildNoGoal),
	"task_cancel_of_a_child_stops_the_grandchild":                        pendingOn(specCancelReport),
	"task_tree_interrupt_stops_the_grandchild":                           reGolden(specTaskInputs, specChildReport, specChildNoGoal, specView, specChildAgent),
	"task_profile_keeps_the_plugin_tools_of_its_list":                    reGolden(specProfileKnown, specTaskInputs, specItems, specOneResult),
	"task_child_on_claude_code_gets_no_runtime_builtin":                  sameAsServe(),
	"claudecode_turn_gets_no_plugin_system_segment":                      sameAsServe(),
	"claudecode_question_dismissed_by_resolve":                           reGolden(specDismissed, specNoStart, specWaiting, specView, specClaudeGauge),
	"claudecode_question_dismissed_by_a_model_of_another_provider":       reGolden(specProviderSwap, specDismissed, specView, specErrors, specClaudeGauge),
	"claudecode_question_answer_bodies_that_are_refused":                 reGolden(specAnswerMap, specRequestRoute, specErrors, specView, specAnswerReceipt, specClaudeGauge),
	"claudecode_answered_call_with_no_result_gets_a_cut_off_result":      reGolden(specOneResult, specWaiting, specView, specAnswerReceipt, specClaudeGauge),
	"claudecode_question_sibling_call_gets_a_result_when_the_turn_parks": reGolden(specOneResult, specDismissed, specWaiting, specView, specClaudeGauge),
	"usage_survives_a_kill_mid_turn":                                     reGolden(specView, specCrash, specCrashMarker),

	"auto_compaction_estimates_the_context_when_no_call_reports_prompt_tokens":            sameAsServe(),
	"auto_compaction_waits_for_the_reading_to_fall_after_an_empty_summary":                sameAsServe(),
	"auto_compaction_estimates_the_context_when_the_newest_turn_reports_no_prompt_tokens": sameAsServe(),
	"auto_compaction_runs_again_after_a_model_change_moves_the_window":                    sameAsServe(),
	"auto_compaction_waits_for_the_reading_to_fall_before_it_runs_again":                  sameAsServe(),
	"mcp_instructions_are_cut_at_4000_runes":                                              sameAsServe(),
	"mcp_instruction_tool_names_stop_at_2048_bytes":                                       sameAsServe(),
	"mcp_catalog_lists_200_deferred_tools_then_counts_the_rest":                           sameAsServe(),
	"a_negative_compaction_threshold_is_the_default_threshold":                            reGolden(specView, specThreshold),
	"auto_compaction_with_a_failed_summary_keeps_the_history":                             reGolden(specView, specFailedSummary),
	"restart_during_auto_compaction_runs_the_queued_input_on_the_next_owner":              reGolden(specView, specOpenStarts),
	"interrupt_stops_an_auto_compaction":                                                  reGolden(specView, specErrors, specInterruptTable, specCompactBusy),
	"auto_compaction_uses_the_window_of_the_session_model":                                reGolden(specView, specWindow),
	"context_overflow_compacts_and_runs_the_turn_again":                                   reGolden(specView, specOverflowFolds),
	"context_overflow_after_the_compaction_fails_the_turn":                                reGolden(specView, specOverflowFolds, specOverflowTwice),
	"a_stalled_summary_fails_the_overflowed_turn":                                         reGolden(specView, specOverflowFails),
	"compact_during_a_turn_is_session_busy":                                               reGolden(specView, specErrors, specCompactBusy),
	"provider_usage_limit_fails_the_turn_and_holds_the_queue":                             reGolden(specView, specExhaustedHolds, specExhaustedQueue, specRestartOpens),
	"a_failed_turn_runs_the_next_queued_input":                                            reGolden(specView, specFailedRuns),
	"session_usage_counts_every_model_call_but_the_evaluation":                            reGolden(specView, specCompactResult),
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
	"codex_ws_restart_prewarms_with_a_tool_the_log_selected":                              reGolden(specItems, specWarm),
	"codex_ws_prewarm_carries_the_plugin_system_segment":                                  reGolden(specItems, specWarm),
	"task_profile_of_a_grandchild_keeps_the_tools_its_parent_allows":                      sameAsServe(),
	"a_failed_summary_keeps_its_usage_in_the_session":                                     reGolden(specView, specOverflowFails),

	"claudecode_history_bridge_after_a_mid_turn_model_change":             reGolden(specUpdate, specView, specMidTurnFails, specClaudeGauge),
	"claudecode_history_bridge_after_a_turn_whose_cli_never_started":      reGolden(specUpdate, specView, specClaudeGauge),
	"claudecode_restart_mid_turn_stops_the_cli_and_resumes":               reGolden(specView, specExternal, specHandoffResume, specOneResult, specClaudeGauge),
	"claudecode_crash_mid_turn_waits_for_input":                           reGolden(specView, specExternal, specCrash, specCrashQueue, specCrashMarker, specHistoryBridge, specClaudeGauge),
	"claudecode_mirror_continues_a_turn_that_the_cli_took":                reGolden(specView, specBackendState, specMirrorBlob, specHandoffResume, specClaudeGauge),
	"claudecode_mirror_restart_before_a_transcript_starts_the_turn_again": reGolden(specView, specBackendState, specMirrorBlob, specHandoffResume, specClaudeGauge),
	"claudecode_mirror_resumes_after_a_restart":                           reGolden(specBackendState, specMirrorBlob),
	"claudecode_mirror_crash_before_a_transcript_starts_a_new_session":    reGolden(specView, specBackendState, specMirrorBlob, specCrash, specCrashQueue, specClaudeGauge),

	"claudecode_interrupt_keeps_the_usage_of_the_result_after_the_signal":         reGolden(specView, specStopped, specClaudeGauge),
	"claudecode_interrupt_closes_a_tool_call_that_the_cli_left_open":              reGolden(specView, specStopped, specOneResult, specClaudeGauge),
	"claudecode_interrupt_closes_a_tool_call_that_the_cli_printed_on_the_signal":  reGolden(specView, specStopped, specOneResult, specClaudeGauge),
	"claudecode_interrupt_keeps_a_tool_result_that_the_cli_printed_on_the_signal": reGolden(specView, specStopped, specClaudeGauge),
	"claudecode_interrupt_ends_completed_when_the_cli_finishes_on_the_signal":     reGolden(specView, specClaudeGauge),
	"claudecode_interrupt_ignores_a_placeholder_result":                           sameAsServe(),
	"claudecode_interrupt_of_a_cli_that_exits_with_no_frame":                      reGolden(specView, specStopped, specClaudeGauge),

	"claudecode_runs_in_the_work_dir":                               sameAsServe(),
	"claudecode_append_system_prompt_reaches_the_cli_as_one_value":  sameAsServe(),
	"claudecode_compact_result_with_no_local_command_ends_the_turn": reGolden(specView, specCompactOwned, specClaudeGauge),
	"claudecode_subagent_and_main_frames_keep_their_wire_order":     reGolden(specOneResult),
	"claudecode_queued_notification_result_does_not_end_the_turn":   reGolden(specView, specClaudeGauge),
	"claudecode_compact_refuses_keep_turns":                         reGolden(specView, specKeepOwned, specErrors, specClaudeGauge),
	"claudecode_cli_compaction_is_logged":                           reGolden(specView, specCompactOwned, specClaudeGauge),

	"process_http_lifecycle":           sameAsServe(),
	"process_http_unknown_name_is_404": reGolden(specProcessUnknown, specNoRoute, specErrors),
	"git_changes_uncommitted_scope_reports_modified_deleted_and_untracked_files": sameAsServe(),
	"git_changes_branch_scope_diffs_the_work_tree_against_the_default_branch":    sameAsServe(),
	"git_changes_uncommitted_scope_on_an_unborn_repo_reports_files_as_added":     sameAsServe(),
	"git_changes_refusals": reGolden(specGitOracle, specErrors),

	"commands_menu_lists_prompt_commands_under_a_symlinked_ancestor": reGolden(specCmdMenuRoutes, specCmdUnsupport),
}
