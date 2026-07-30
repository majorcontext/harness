# Hub Monitor View — Architecture

> **For Claude:** implementation via superpowers:subagent-driven-development, one task per section under "Build sequence". Implementers MUST invoke the `.agents/skills` design skills (better-layout, better-typography, better-ui, better-accessibility) before writing UI code, and AGENTS.md's "UI design language" section is binding throughout.

**Goal:** a passive, glanceable answer to "what is this box — and every session on it — doing *right now*?" without interrupting anything: a read-only Monitor view inside the hub that derives live per-session activity from the event stream the hub already consumes.

**Why a hub view and not a third app:** the repo has two build-free frontends that already duplicate the SSE parser, auth plumbing, and state handling between them. A standalone monitor would mint a third copy of all three. The hub already owns multi-box fetch-streaming with `?from=` resume, URL-fragment state, the binding design language, a pure-helper test harness, and a Go-driven jsdom e2e — the monitor is a projection of data the hub already has in memory, plus one new derivation layer. The inspector stays what it is: the deep single-session debugger.

**Vanilla only, per house rules:** no build step, no dependencies, inline `<style>`/`<script>` in `tools/hub/index.html`, pure helpers inside the `TESTABLE-BEGIN/END` markers, tested by `hub_test.mjs`'s existing vm-extraction pattern.

---

## 1. The derivation layer (pure, tested — the heart of the feature)

New pure reducer inside the TESTABLE region:

```js
// reduceActivity(prev, ev) → activity
// activity = {
//   phase: "idle" | "streaming" | "tool" | "between",  // see mapping below
//   tool: {name, sinceSeq, sinceAt} | null,             // current in-flight tool
//   turn: {startedAt, toolCalls, events} | null,        // current turn accumulation
//   lastEventAt,                                        // wall-clock of last event seen
//   lastOutcome,                                        // from turn.end (carried while idle)
// }
```

Event mapping (fold over the same per-box stream `connectBoxStream` already delivers):
- `session.status busy` → open a turn (`phase: "between"`, turn started).
- `text.delta` / `reasoning.delta` → `phase: "streaming"` (provider call in flight).
- `tool.start` → `phase: "tool"`, set `tool` (name + timestamps); `tool.end` → clear tool, `phase: "between"`, `turn.toolCalls++`.
- `message` → bump `turn.events`; `turn.end` → record outcome; `session.status idle` → close turn, `phase: "idle"`.
- Every event updates `lastEventAt`.

Second pure helper, the client-side analog of the serve-side in-flight watchdog:

```js
// staleness(activity, now) → "live" | "quiet" | "stalled"
// busy session with no event for > QUIET_MS (15s) → "quiet";  > STALL_MS (60s) → "stalled".
// idle sessions are never stalled. Thresholds are exported consts (tests pin them).
```

Third: `formatElapsed(ms)` (compact `4s` / `2m10s` / `1h03m`) if the hub doesn't already have one — check before adding.

All three are pure functions of `(state, event | now)` — no DOM, no timers — and get exhaustive `hub_test.mjs` coverage including out-of-order edge cases (tool.end without start after a mid-turn connect; deltas while a tool is in flight, which happens on parallel tool streams — last-writer-wins on phase is acceptable and documented).

**Seeding gap (same one the inspector's goal chip solved):** a monitor connecting mid-turn never saw `session.status busy`. Seed each session's activity from the poll data the hub already fetches (`GET /session` composite state: running ⇒ open a turn with unknown start). Document that elapsed shows `—` until a real timestamp is observed — the design language's "every telemetry value is real data" rule forbids fabricating a start time.

## 2. The view

- **Fragment state:** `view.mode: "monitor"` added to the `#s=` codec (absent = existing behavior; codec change is additive and backward-compatible — old links decode unchanged). Toggle lives in the existing header nav. `history.replaceState` as always.
- **Layout:** a dense board — one column per connected box, box header = identity line straight from `/health`'s new fields (`harness <version> · session_sync=<mode> · up <elapsed>`) + composite health; under it one row per session: short id, composite state chip, activity phase + ticking elapsed, current tool name (monospace, truncated middle), turn tool-call count, queue depth (from the poll), goal chip (reuse the existing helper/classes), last outcome.
- **Semantics of color (the only two allowed):** hazard red = `stalled`, `session.error`, parked/paused-for-failure, health down. Terminal green = `streaming`/`tool` (live provider or tool activity), goal achieved. Everything else is the neutral substrate. `quiet` renders as dimmed text, not a color.
- **Ticking:** one `setInterval` (1s) re-renders elapsed/staleness only — cheap text updates against real timestamps; no per-row timers.
- **Read-only:** zero mutating controls in this view. Click a session row → existing session view (mode flips back); click a box header → existing box view. Keyboard: rows focusable, Enter follows — better-accessibility applies.
- **Empty/degraded states:** box unreachable renders exactly like the hub's existing "inactive" treatment (it is a normal lifecycle state, not an alarm); no sessions = one dim line. No spinners — staleness IS the freshness indicator.

## 3. What this deliberately does NOT do

- **No server changes.** Current-turn state stays client-derived; if a second consumer (canaries) ever needs it, that's the `current_turn` server field discussed separately — not this.
- **No new network machinery.** Same streams, same 5s poll, same resume cursors the hub already runs. The monitor adds zero additional load on boxes.
- **No third app, no shared-CSS refactor, no renamed classes** (load-bearing, per AGENTS.md).

## 4. Build sequence

1. **Pure helpers + tests**: `reduceActivity`, `staleness`, `formatElapsed` (if absent) in the TESTABLE region; `hub_test.mjs` cases incl. mid-turn-connect seeding, out-of-order tool events, threshold boundaries. Red-first.
2. **Wire the fold into `connectBoxStream`'s event dispatch** (one call per event per session, stored on the box runtime next to existing per-session state); seed from poll data.
3. **The view**: fragment-codec addition + codec tests; board rendering; tick loop; navigation; design-language pass with the better-* skills invoked; keyboard/focus per better-accessibility.
4. **e2e**: extend `tools/hub/e2e` — drive a real server through a scripted turn (busy → tool.start → tool.end → idle) and assert the monitor row's phase/tool/outcome transitions in jsdom.
5. **CI gap fix (found during this survey):** add `node --test tools/hub/*_test.mjs` to ci.yml next to the inspector step — `hub_test.mjs` is documented as a check in AGENTS.md and assumed by the e2e's comments, but never runs in CI today.

Gates per task: full Go suite (the e2e rides `go test`), both node test files, gofmt/vet, no internal refs.
