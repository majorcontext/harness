package engine

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file is the fail-closed guard TestSnapshotCarriesClaudeCodeSessionID
// and TestSnapshotCarriesClaudeCodeCost exist beside: those two tests each
// pin ONE fold-only field the snapshot forgot (Session.claudeCodeCLISessionID/
// claudeCodeHistoryWatermark, then claudeCodeSessionCostUSD/haveClaudeCodeCost)
// after a live audit found them missing. But TestSnapshotCarriesEveryFoldedField
// (snapshot_test.go) only checks against foldState, a HAND-MAINTAINED struct
// literal — a field a fold sets that is missing from BOTH sessionSnapshot and
// foldState passes that test silently, which is exactly how the claude-code
// fields slipped through in the first place.
//
// TestEverySessionFieldIsClassifiedForSnapshotting closes that gap
// structurally instead of by remembering to update another hand-maintained
// list: it reflects over the Session struct itself (engine.go) and requires
// EVERY field to appear in EXACTLY ONE of the two classification sets below.
// A field in neither is a compile-clean, test-red failure — the author must
// explicitly decide "snapshot it" or "exclude it, and say why" before the
// field can exist at all. It cannot prove a field in snapshottedSessionFields
// is actually wired correctly into captureSnapshotLocked/restoreSnapshot
// (that correctness is what TestSnapshotCarriesEveryFoldedField and the two
// targeted claude-code tests are for) — it only proves no field was left
// unclassified, which is the specific gap that let two fields go missing
// silently.

// snapshottedSessionFields is every Session field (engine.go) that
// snapshot.go's captureSnapshotLocked/restoreSnapshot round-trip through a
// sessionSnapshot. Adding a field here without also wiring it into both of
// those functions leaves TestSnapshotCarriesEveryFoldedField (and, for the
// four claude-code fields specifically, TestSnapshotCarriesClaudeCodeSessionID/
// TestSnapshotCarriesClaudeCodeCost) to catch the omission — this map only
// asserts the field was CONSIDERED, not that the wiring is correct.
var snapshottedSessionFields = map[string]bool{
	"model":                      true,
	"effort":                     true,
	"serviceTier":                true,
	"history":                    true,
	"usage":                      true,
	"lastUsage":                  true,
	"haveLastUsage":              true,
	"goalActive":                 true,
	"goalCondition":              true,
	"compactCount":               true,
	"lastCompactedAt":            true,
	"promptQueue":                true,
	"promptQueueNextID":          true,
	"enqueueSeq":                 true,
	"toolResults":                true,
	"toolResultNextID":           true,
	"toolResultBytes":            true,
	"mcpSelected":                true,
	"spawnedChildIDs":            true,
	"taskNotifications":          true,
	"turnUnsettled":              true,
	"committedOutcome":           true,
	"claudeCodeCLISessionID":     true,
	"claudeCodeHistoryWatermark": true,
	"claudeCodeSessionCostUSD":   true,
	"haveClaudeCodeCost":         true,
}

// snapshotExcludedSessionFields is every other Session field, each mapped
// to a one-line reason it is deliberately NOT part of the snapshot round
// trip. A new field lands here only when it genuinely belongs to one of
// these categories — session identity/config, a runtime-only handle or
// lock, journal/snapshot bookkeeping the loader computes directly, a lazy
// disk cache, or state a full replay itself never reconstructs either (so
// snapshotting it would violate the snapshot-equals-full-replay invariant
// sessionSnapshot's own doc comment states, not merely omit an
// optimization). When in doubt, it belongs in snapshottedSessionFields
// instead.
var snapshotExcludedSessionFields = map[string]string{
	"ID":                        "session identity, set directly by NewSession/LoadSession before any header/fold/restore runs",
	"cfg":                       "Config value: header-derived subfields (WorkDir, ParentSession, TaskParentID, ...) are replayed unconditionally from the recSession header regardless of anchor; the rest is construction-time config (live callbacks, SessionDir, ...), not fold state",
	"tools":                     "the registered tool set, (re)constructed by session setup from cfg and runtime capability checks, not a journal fold target",
	"mu":                        "sync.Mutex, runtime-only",
	"createdAt":                 "header-derived; replayed unconditionally from the recSession header regardless of anchor (see sessionSnapshot's own doc comment, \"Header-derived state ... is NOT here\")",
	"subscriptionUsage":         "explicitly process-local only per its own doc comment; never folded into cumulative state or replayed by LoadSession",
	"logFile":                   "*os.File, runtime-only handle",
	"logStarted":                "runtime bookkeeping about whether the log file exists; set directly by LoadSession/ensureLog, not a fold",
	"lastPersistErr":            "runtime error cache; never durable state, a fresh load starts with none",
	"recordsWritten":            "journal-head bookkeeping the loader computes directly from the journal's own line count, not fold state a record's payload carries",
	"snapshotSeq":               "the snapshot writer's own anchor bookkeeping; set directly by the loader/writer, not restored from a snapshot payload",
	"snapshotting":              "coalescing flag for the in-flight snapshot write, runtime-only",
	"lastSnapshotErr":           "runtime error cache for the snapshot writer itself",
	"snapshotWG":                "sync.WaitGroup, runtime-only",
	"snapshotWrites":            "atomic counter, runtime-only diagnostics",
	"snapshotInFlight":          "atomic counter, runtime-only diagnostics",
	"snapshotConcurrentPeak":    "atomic counter, runtime-only diagnostics",
	"replayedRecords":           "the loader's own decoded-record counter, not fold state",
	"durableDebt":               "in-flight deferred-durable-write counter; snapshotSafeLocked refuses to capture while it is non-zero, so it is always 0 at any actual anchor",
	"index":                     "the metadata-index fold; LoadSession marks it broken on a snapshot load and lets it self-heal on the next write (see LoadSession's own comment: \"Snapshotting the index fold itself is a possible follow-up; nothing here may guess at it\")",
	"logSize":                   "journal byte-length bookkeeping, set by ensureLog/the loader, not fold state",
	"indexFile":                 "*os.File, runtime-only handle",
	"lastIndexErr":              "runtime error cache for the index sidecar writer",
	"instrLoaded":               "lazy load-once cache gate, populated from disk on the first Prompt, not journal state",
	"instrSeg":                  "lazy load-once cache payload, same pattern as instrLoaded",
	"instrErr":                  "lazy load-once cache error, same pattern as instrLoaded",
	"instrPath":                 "lazy load-once cache payload, same pattern as instrLoaded",
	"turn":                      "explicitly excluded by sessionSnapshot's own doc comment: no record carries it, so a full replay reports turn=0 too — capturing it would violate the snapshot-equals-full-replay invariant, not merely skip an optimization",
	"lastSystem":                "same explicit exclusion as turn, same doc comment, same reasoning",
	"pendingContinuationNudge":  "one-shot scratch state for the single in-flight Prompt call, cleared before that call returns; never persisted",
	"skills":                    "lazy discovery cache, same load-once pattern as instrLoaded",
	"skillsLoaded":              "lazy discovery cache gate, same pattern as instrLoaded",
	"skillsSeg":                 "lazy discovery cache payload, same pattern as instrLoaded",
	"skillsErr":                 "lazy discovery cache error, same pattern as instrLoaded",
	"goalGen":                   "explicitly documented \"Deliberately runtime-only: never persisted ... never restored on LoadSession\"",
	"goalParked":                "explicitly documented \"Deliberately runtime-only: never persisted, never folded by LoadSession\"",
	"goalParkedReason":          "same explicit exclusion as goalParked, same doc comment",
	"goalParkedAttempts":        "same explicit exclusion as goalParked, same doc comment",
	"toolExecCount":             "runtime-only retry-safety counter for the current goal-loop attempt; not persisted or folded by LoadSession",
	"compactHysteresis":         "explicitly documented \"Deliberately NOT persisted: a reload re-evaluates from scratch\"",
	"contextWindowExplicit":     "derived once at construction from cfg.ContextWindowTokens/the model, re-derived identically by newSession/LoadSession on every load path; not fold state",
	"contextWindowSource":       "same derivation as contextWindowExplicit, same reasoning",
	"contextWindowErr":          "same derivation as contextWindowExplicit; set/cleared by construction and SetModel, recomputed the same way on any load",
	"toolConcurrency":           "resolved once in newSession from Config.ToolConcurrency, read-only afterward; not fold state",
	"readBudget":                "resolved once in newSession from Config.ToolReadBudgetBytes; not fold state",
	"deferredQueueRecords":      "in-flight deferred-durable-write buffer; snapshotSafeLocked refuses to capture while it is non-empty, so it is always empty at any actual anchor",
	"claudeCodeQueueWake":       "atomic.Pointer wake channel for a currently-running turn's stdin pump; nil after any load, never persisted",
	"readHashes":                "explicitly documented \"Deliberately in-memory and per-live-Session only: never persisted, never folded by LoadSession\"",
	"taskNotificationsInFlight": "in-turn checkout state; nil after ANY load (a full replay never populates it either, since checkout only happens during a live turn) — the snapshot's own TaskNotifications field already carries these entries back into the plain taskNotifications queue on restore",
	"agentDefsLoaded":           "lazy discovery cache (triggered by the task tool's first call), same load-once pattern as instrLoaded/skillsLoaded",
	"agentDefs":                 "lazy discovery cache payload, same pattern as agentDefsLoaded",
	"agentDefsErr":              "lazy discovery cache error, same pattern as agentDefsLoaded",
	"startupPrewarm":            "runtime-only startup task handle; loaded sessions never resume or restore prewarm",
	"startupPrewarmResolution":  "runtime-only first-turn metric state; loaded sessions never resume or restore prewarm",
	"startupPrewarmEligible":    "fresh-session construction gate; loaded sessions deliberately remain ineligible",
}

// TestEverySessionFieldIsClassifiedForSnapshotting is the fail-closed net:
// every field reflect.TypeOf(Session{}) reports must be in EXACTLY ONE of
// snapshottedSessionFields or snapshotExcludedSessionFields. A newly added
// Session field that is in neither fails this test immediately, forcing
// the author to make an explicit "snapshot it or exclude it, and say why"
// decision — the exact decision that was skipped for claudeCodeCLISessionID/
// claudeCodeHistoryWatermark/claudeCodeSessionCostUSD/haveClaudeCodeCost
// before this guard existed.
func TestEverySessionFieldIsClassifiedForSnapshotting(t *testing.T) {
	typ := reflect.TypeOf(Session{})

	live := make(map[string]bool, typ.NumField())
	var unclassified, both []string
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		live[name] = true
		_, snapped := snapshottedSessionFields[name]
		_, excluded := snapshotExcludedSessionFields[name]
		switch {
		case snapped && excluded:
			both = append(both, name)
		case !snapped && !excluded:
			unclassified = append(unclassified, name)
		}
	}

	if len(both) > 0 {
		sort.Strings(both)
		t.Errorf("Session field(s) %s are in BOTH snapshottedSessionFields and snapshotExcludedSessionFields (engine/snapshot_field_coverage_test.go) — a field is either snapshotted or excluded, never both",
			strings.Join(both, ", "))
	}
	if len(unclassified) > 0 {
		sort.Strings(unclassified)
		t.Errorf("Session field(s) %s are not classified in snapshottedSessionFields or snapshotExcludedSessionFields (engine/snapshot_field_coverage_test.go). "+
			"Add the new field to snapshottedSessionFields if captureSnapshotLocked/restoreSnapshot (snapshot.go) must round-trip it through a snapshot-anchored load, "+
			"or to snapshotExcludedSessionFields with a one-line reason if it is deliberately not snapshot state (runtime-only, header-derived, a lazy cache, journal bookkeeping, ...). "+
			"This is the guard against the bug class that shipped without a snapshot field for claudeCodeCLISessionID/claudeCodeHistoryWatermark/claudeCodeSessionCostUSD/haveClaudeCodeCost.",
			strings.Join(unclassified, ", "))
	}

	// The reverse direction: a classification entry naming a field that no
	// longer exists on Session (a rename, or a field removed outright)
	// would otherwise sit there forever, silently vacuous.
	var stale []string
	for name := range snapshottedSessionFields {
		if !live[name] {
			stale = append(stale, name)
		}
	}
	for name := range snapshotExcludedSessionFields {
		if !live[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("classification names field(s) %s that no longer exist on Session (engine/snapshot_field_coverage_test.go) — remove the stale entry",
			strings.Join(stale, ", "))
	}
}
