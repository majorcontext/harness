# Serve command test reduction report

## Coverage map

| # | Behavior group | Surviving tests |
|---|---|---|
| 1 | Typed, untyped, escaped, unknown, invalid, and unsupported command resolution | `command.TestResolveTable`, `TestResolveUnknownIsNotText`, `TestResolveArgErrorNamesTheTypedAlias`; `server.TestTypedCommandOutcomes`, `TestCommandOutcomeMapping` |
| 2 | Mid-turn and raced refusal | `server.TestMidTurnCompactRefusedNotQueued`, `TestRacedMidTurnCompactRecordsRefused` |
| 3 | Managed-child failure and enqueue resolution order | `server.TestTypedCompactOnManagedChildRecordsFailed`, `TestEnqueueTypedCommandResolvesBeforeManagedChildGuard` |
| 4 | Durable enqueue deduplication and watermark across restart/snapshot | `engine.TestRecordCommandDurableDedupesSeqAcrossRestart`, `TestCommandSurvivesSnapshotAnchoredLoad`, `TestCommandFoldTornSeqLastWriterWins`; existing queue-watermark restart tests remain in `engine/queue_durable_test.go` |
| 5 | Interrupted-command repair | `engine.TestRepairInterruptedCommands`, `server.TestBootMarksAcceptedCommandInterruptedCarriesClientRef` |
| 6 | Terminal metadata and `client_ref` propagation | `engine.TestRecordCommandTerminalKeepsCreatedAtAndAnchor`, `TestRecordCommandTerminalInheritsClientRef`; `server.TestClientRefCarriesOnTypedCommand`, `TestBootMarksAcceptedCommandInterruptedCarriesClientRef` |
| 7 | Residency pin, eviction, delete refusal, and release | `server.TestMutableSessionColdInsertPinsBeforeSweep`, `TestCommandTerminalWritesLandOnLiveSessionAfterEviction`, `TestMutableSessionPinReleasedAfterCommand`, `TestHandleEndRefusesWhilePinned` |
| 8 | Commands stay out of history; snapshot reads remain consistent | `engine.TestCommandRecordNeverEntersHistory`, `TestHistoryAndCommandsOneSnapshot`, `TestCommandSurvivesSnapshotAnchoredLoad` |
| 9 | Compaction re-anchor and duplicate message IDs | `engine.TestCommandReanchoredOnCompact`; `TestFoldedPageUsesSurvivingRecordOccurrence` |
| 10 | Page/bootstrap windows, empty anchor, torn sequence, and malformed records outside the window | `engine.TestMessagePageCarriesCommandsInWindow`, `TestMessagePageShowsLatestFoldedCommandStatus`, `TestMessagePageTornSeqFoldsToLatestCommand`, `TestTailPageIgnoresAMalformedCommandOutsideTheWindow`, `TestFoldedPageIgnoresAMalformedCommandOutsideTheWindow`; `server.TestTranscriptBootstrapCarriesCommands`, `TestMessagePageFallbackCarriesCommands` |
| 11 | Sidecar/index command decoding stays metadata-only | `engine.TestSessionIndexSidecarCarriesNoCommandData`, `TestSessionIndexRefoldIgnoresCommandPayloadShape` |
| 12 | `prompt_async` cursor precedes the accepted event | `server.TestCommandPromptAsyncSeqPrecedesAcceptedEvent` |
| 13 | `client_ref` on all routes; ordinary prompt drop and validation | `server.TestClientRefCarriesOnTypedCommand`, `TestClientRefDroppedForOrdinaryPrompt`, `TestClientRefValidation` |
| 14 | Child enqueue command ordering | `server.TestEnqueueTypedCommandResolvesBeforeManagedChildGuard`, `TestEnqueueCommandSeqRunsOnce` |
| 15 | Slim native result, over-cap summary, delegated result, and skip result | `server.TestCompactCommandResultIsSlim`, `TestCompactEndpointReportsSkipReason`; `engine.TestClaudeCodeCompactTurn` |
| 16 | Bounded capture, invalid JSON, drain refusal, panic recovery, and 503 admission | `server.TestCommandResponseWriterCapsBufferedBody`, `TestCommandOutcomeMapping`, `TestTypedCommandRefusedWhileDraining`, `TestRunCommandHandlerPanicRecordsFailed` |
| 17 | Serve-mode operation and support tables are total, with stable reasons | `server.TestServeModeOpsTotal`, `TestCommandsServeSupportTotal` |
| 18 | `ArgsError` and command wire shape | `command.TestResolveArgErrorNamesTheTypedAlias`, `message.TestCommandRecordWireShape` |

Resolution tables and outcome assertions check expected records and reject absent or surplus outcomes. The page and transcript tests also compare returned command records to the expected window, not only their presence.

## Line counts

The brief recorded 3,562 test lines added against the branch base. The current working diff adds 2,417 test lines, a net reduction of 1,145 lines (32.1%). This is 1,117 lines above the stretch target of about 1,300. Further reduction would remove distinct existing branch coverage; the coverage checklist takes priority.

## Verification

- `gofmt -l` on all 16 changed test files: clean.
- `go test -race ./...`: pass.
- `go vet ./...`: pass.
- `go test -race ./server -run TestBootMarksAcceptedCommandInterruptedCarriesClientRef -count=1`: pass after correcting the test's double-close cleanup.
