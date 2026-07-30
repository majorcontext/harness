// REAL end-to-end verification of tools/monitor/index.html (see its own
// header comment's hand-test checklist for the behaviors this automates, and
// AGENTS.md for the monitor's role): drives the ACTUAL page — byte-for-byte
// the same file committed at tools/monitor/index.html (checked below), no
// mock DOM shortcuts — against a REAL running harness box (tools/monitor/
// e2e's Stub — same wiring as `harness serve`, plus a couple of scripted
// providers so turns don't need a real model API key), using jsdom + Node's
// own, UNMOCKED fetch. Nothing in this file simulates HTTP/SSE traffic;
// every request below is a real socket round-trip to the servers
// e2e_test.go started, including a REAL "bash" tool call (a short `sleep`)
// executed by the real engine — not a mocked delay.
//
// Expects three arguments: <box_base> <monitor_base> <token> (see
// tools/monitor/e2e/stub.go's Start). Exits non-zero on any failed
// assertion, printing the failure to stderr. Requires "jsdom" (see
// tools/monitor/e2e/package.json). Run directly with:
//   go run ./tools/monitor/e2e/... is not provided (unlike tools/hub/e2e's
//   hubverify) — this package's only entry point is e2e_test.go, which
//   starts the stub and drives this script itself. To poke at it by hand,
//   write a small one-off `main` calling e2e.Start(), print the returned
//   Stub fields, then:
//     node tools/monitor/e2e/real_e2e.mjs <box_base> <monitor_base> <token>
//
// Mirrors tools/hub/e2e/real_e2e.mjs's structure and conventions throughout
// (the jsdom setup, the fetch/AbortController polyfilling, the byte-for-byte
// served-file check, the PASS/console.error-per-assertion narration, the
// forced process.exit at the end).
import { JSDOM } from "jsdom";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const [, , boxBase, monitorBase, token, unauthBase] = process.argv;
if (!boxBase || !monitorBase || !token || !unauthBase) {
  console.error("usage: node real_e2e.mjs <box_base> <monitor_base> <token> <unauth_base>");
  process.exit(2);
}
console.error("box:", boxBase, "monitor:", monitorBase, "unauth:", unauthBase);

const here = dirname(fileURLToPath(import.meta.url));
const committedIndexHTML = readFileSync(join(here, "..", "index.html"), "utf8");

// Provider keys — must match the consts in stub.go exactly (see its Prov*
// block); duplicated here as plain strings rather than parsed out of the Go
// source because this script has no Go tooling available to it.
const ProvQuickIdle = "e2e-quick-idle";
const ProvToolBoard = "e2e-tool-board";
const ProvToolDetail = "e2e-tool-detail";
const ProvStallStale = "e2e-stall-stale";
const ProvStallDedup = "e2e-stall-dedup";
const ProvStreamError = "e2e-stream-error";
const ProvReconnectGap = "e2e-reconnect-gap";
const ProvLiveCap = "e2e-live-cap";
// Must match stub.go's StreamErrorText/ReconnectGapReply exactly — same
// duplication-by-hand reasoning as the Prov* keys above (this script has no
// Go tooling).
const STREAM_ERROR_TEXT = "simulated upstream failure: connection reset by peer";
const RECONNECT_GAP_REPLY = "reconnect-gap turn landed";

// TUNING shrinks the monitor's staleness thresholds (production QUIET_MS/
// STALL_MS are 15000/60000 — see index.html) down to something a real,
// bounded bash `sleep` can cross inside a CI-sane test budget, shrinks
// DETAIL_LIVE_EVENTS_CAP (production 500) down to something a single
// scripted turn's handful of live events comfortably crosses, and WIDENS
// BACKOFF_MIN (production 500ms) so the reconnect-gap-heal scenario has a
// deterministically generous window between a real server-side kill/
// restart and the page's own next reconnect attempt, rather than depending
// on exact wall-clock luck to land its race. Read by index.html's
// window.__monitorTuning seam, set below via JSDOM's beforeParse so it
// lands before the page's inline <script> ever runs.
//
// The gap between QUIET_MS and STALL_MS must be comfortably wider than the
// board's own 1s ticker (index.html's TICK_MS): while a session sits mid-
// tool-call with no new events, staleness is only ever RE-EVALUATED on that
// periodic tick (there is nothing else to trigger a re-render) — a gap
// narrower than one tick period means the "quiet" tier can fall entirely
// between two samples and never be observed, even though it was real.
const TUNING = { QUIET_MS: 200, STALL_MS: 1800, DETAIL_LIVE_EVENTS_CAP: 2, BACKOFF_MIN: 4000, BACKOFF_MAX: 10000, POLL_MS: 300 };

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// waitFor polls `fn` (sync or async) until it returns a truthy value,
// throwing with `label` if `timeoutMs` elapses first — the one primitive
// nearly every scenario below is built from, since almost everything here
// is "wait for a REAL server round trip (a turn, an SSE event, a fetch) to
// land, then check the real DOM."
async function waitFor(fn, { timeoutMs = 8000, intervalMs = 25, label = "condition" } = {}) {
  const start = Date.now();
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() - start > timeoutMs) throw new Error("timed out waiting for: " + label);
    await sleep(intervalMs);
  }
}

// ---- direct box API helpers: these talk to the box the way some OTHER
// client (a goal loop, a CLI session, a teammate) would — never through the
// monitor page — which is exactly what the monitor's board is meant to
// observe passively. The monitor's OWN composer POST path is exercised
// separately, through the real page, by the composer scenarios below. ----

async function boxFetch(path, opts = {}) {
  const headers = Object.assign({ Authorization: "Bearer " + token }, opts.headers || {});
  return fetch(boxBase + path, Object.assign({}, opts, { headers }));
}

async function createSession(providerKey) {
  const resp = await boxFetch("/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    // message.ModelRef marshals/unmarshals as a single "provider/model"
    // string (see message/model.go), not a {provider, model} object.
    body: JSON.stringify({ model: providerKey + "/m1" }),
  });
  assert.equal(resp.status, 201, "create session status");
  const body = await resp.json();
  assert.ok(body.id, "created session must carry an id");
  return body.id;
}

async function promptAsync(id, text) {
  return boxFetch("/session/" + encodeURIComponent(id) + "/prompt_async", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ parts: [{ type: "text", text }] }),
  });
}

// waitIdle long-polls the box's own GET /session/{id}/wait?until=idle — a
// real, documented part of the public HTTP API (not a Go-test-only helper;
// see server/wait.go), woken by the same durable-event fanout the SSE
// stream itself rides. Using it here means every scenario below waits on
// the SERVER's own authoritative state, never on a guessed sleep duration.
async function waitIdle(id, timeoutS = 15) {
  const resp = await boxFetch("/session/" + encodeURIComponent(id) + "/wait?until=idle&timeout_s=" + timeoutS);
  assert.equal(resp.status, 200, "wait status");
  return resp.json();
}

// ---- MutationObserver-based sequence capture: turns can move through
// several phases within a single synchronous SSE-chunk-processing pass (a
// fast turn's session.status busy -> text.delta -> tool.start can all land
// in one `read()` resolution), so a plain setInterval poll can silently
// miss an intermediate state entirely. MutationObserver instead queues one
// record per REAL mutation, in order, regardless of how fast they happened,
// and delivers the whole queue at the next microtask checkpoint — so
// reconstructing the full value history from the records is exact, not
// sampled. ----

// watchAttrSequence reconstructs an attribute's full value history from
// MutationRecord.oldValue: record[i].oldValue is, by definition, the
// attribute's value the instant BEFORE record i's mutation — i.e. the same
// as the sequence's i-th value — and the value after the LAST mutation is
// simply the element's current attribute value (read at the end, once
// everything has quiesced).
function watchAttrSequence(w, el, attrName) {
  const records = [];
  const obs = new w.MutationObserver((recs) => {
    for (const r of recs) records.push(r);
  });
  obs.observe(el, { attributes: true, attributeFilter: [attrName], attributeOldValue: true });
  return {
    stop() {
      obs.disconnect();
    },
    values() {
      const vs = records.map((r) => r.oldValue);
      vs.push(el.getAttribute(attrName));
      return vs;
    },
  };
}

// watchTextSequence does the analogous reconstruction for an element's
// textContent, via childList instead of characterData: per the DOM spec,
// `el.textContent = x` always performs "string replace all" — it removes
// every existing child and, if x is non-empty, appends exactly ONE brand-new
// Text node — so each record's addedNodes[0] is a distinct, never-touched-
// again node holding exactly the value assigned at that moment. That is
// simpler and more direct than the oldValue reconstruction above (no need
// to shift-by-one or read a "current" value at the end).
function watchTextSequence(w, el) {
  const records = [];
  const obs = new w.MutationObserver((recs) => {
    for (const r of recs) records.push(r);
  });
  obs.observe(el, { childList: true });
  return {
    stop() {
      obs.disconnect();
    },
    values() {
      return records.map((r) => (r.addedNodes[0] ? r.addedNodes[0].data : "")).filter((v) => v !== "");
    },
  };
}

function findRow(doc, sessionId) {
  const rows = [...doc.querySelectorAll("#sessions .session")];
  return rows.find((r) => {
    const sid = r.querySelector(".sid");
    return sid && sid.textContent === sessionId;
  });
}

// openDetailViaClick clicks the REAL board row <a href="#..."> element —
// the actual user gesture — and falls back to setting location.hash
// directly (still firing the SAME hashchange -> applyRoute() path
// index.html itself wires up) only if that click's default same-document
// hash-navigation activation doesn't materialize, a known gap in some jsdom
// versions for anchor click semantics that has nothing to do with the
// monitor page's own behavior.
async function openDetailViaClick(w, doc, row) {
  row.click();
  await sleep(20);
  if (!doc.body.classList.contains("showing-detail")) {
    w.location.hash = row.getAttribute("href");
  }
  await waitFor(() => doc.body.classList.contains("showing-detail"), { label: "detail view opened via row click" });
}

function operatorTexts(doc) {
  return [...doc.querySelectorAll("#transcript .msg.user .body p")].map((n) => n.textContent);
}
function assistantTexts(doc) {
  return [...doc.querySelectorAll("#transcript .msg:not(.user):not(.reasoning) .body p")].map((n) => n.textContent);
}
function turnMarkCount(doc) {
  return doc.querySelectorAll("#transcript .turn-mark").length;
}

// jsdom does not implement the HTML Popover API; the monitor page never
// calls it, but AbortController IS load-bearing (index.html's connectStream
// uses a real AbortController per stream attempt) — jsdom's own
// AbortController produces AbortSignal instances real undici fetch rejects
// as foreign, so swap in Node's. Same category of fix as tools/hub/e2e's
// real_e2e.mjs: supply a browser capability jsdom lacks, never alter the
// product code to suit the test environment.
function installGlobals(w) {
  w.fetch = fetch;
  w.AbortController = AbortController;
  w.__monitorTuning = TUNING;
}

// openEmbeddedPage drives a FRESH page load of the box's own real GET
// /monitor route (never the separate static monitorBase server — see
// stub.go's Stub.UnauthBase/MonitorPage doc comments) at `url`, which may
// carry its own hash (e.g. "#t=..."): a brand-new JSDOM instance, own
// localStorage, own location — exactly what a genuinely fresh browser tab
// opening that link would see, unlike the single shared `dom`/`w` the rest
// of this file drives across many sequential scenarios (which accumulates
// localStorage/hash state those scenarios themselves rely on). Fetches the
// real bytes from `url` itself (not the already-cached committedIndexHTML)
// so this is a genuine end-to-end check of the embedded route serving
// working, connectable HTML — not a simulation of it.
async function openEmbeddedPage(url) {
  const html = await (await fetch(url)).text();
  const dom = new JSDOM(html, {
    url,
    runScripts: "dangerously",
    resources: "usable",
    pretendToBeVisual: true,
    beforeParse: installGlobals,
  });
  return { dom, w: dom.window, doc: dom.window.document };
}

async function main() {
  // ---- 0. The monitor's static file server must be serving the EXACT
  // committed file (production wiring, not a stale copy) — this checks the
  // on-disk file the test's static server points at is the real one, the
  // same guarantee tools/hub/e2e's real_e2e.mjs checks via its go:embed'd
  // handler. The box's own REAL GET /monitor route (server.Options.
  // MonitorPage, added since this comment was first written) is checked
  // separately, further down, by the embeddedConnectPlan scenarios loading
  // it directly. ----
  const servedHTML = await (await fetch(monitorBase + "/")).text();
  assert.equal(servedHTML, committedIndexHTML, "the monitor's static server must serve tools/monitor/index.html byte-for-byte");
  console.error("PASS: monitor static server serves the committed index.html byte-for-byte");

  const dom = new JSDOM(servedHTML, {
    url: monitorBase + "/",
    runScripts: "dangerously",
    resources: "usable",
    pretendToBeVisual: true,
    beforeParse: installGlobals,
  });
  const w = dom.window;
  const doc = w.document;

  // ---- 1. Connect + identity (scenario 1). ----
  assert.ok(doc.getElementById("connect-panel"), "connect panel must render before any connection");
  assert.ok(!doc.body.classList.contains("connected"), "must not start connected");

  doc.getElementById("base-url").value = boxBase;
  doc.getElementById("run-token").value = "definitely-the-wrong-token";
  doc.getElementById("connect-form").dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));
  await waitFor(() => doc.getElementById("connect-err").textContent.trim().length > 0, { label: "inline error for a wrong token" });
  assert.equal(doc.getElementById("connect-err").textContent, "run token was rejected", "wrong-token error text");
  assert.ok(!doc.body.classList.contains("connected"), "a rejected token must not connect");
  console.error("PASS: a wrong run token surfaces an inline connect error, no /health-side failure");

  doc.getElementById("run-token").value = token;
  doc.getElementById("connect-form").dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));
  await waitFor(() => doc.body.classList.contains("connected"), { label: "real connect with the right token" });

  const versionText = doc.getElementById("hdr-version").textContent;
  assert.ok(versionText.startsWith("harness ") && versionText !== "harness —", "identity line must render a real /health version: " + versionText);
  const syncText = doc.getElementById("hdr-sync").textContent;
  assert.ok(syncText.startsWith("session_sync "), "identity line must render a real /health session_sync: " + syncText);
  assert.ok(doc.querySelector(".board-empty"), "board must render empty before any session exists: " + doc.getElementById("sessions").textContent);
  await waitFor(() => doc.getElementById("conn-text").textContent === "streaming", { label: "SSE stream opens after connect" });
  console.error("PASS: real connect — identity line (" + versionText + ", " + syncText + "), empty board, stream open");

  // ---- 2. Board transitions (scenario 2): a real scripted turn with
  // streaming text AND a real, briefly-blocking bash tool call. ----
  const boardID = await createSession(ProvToolBoard);
  const boardRow = await waitFor(() => findRow(doc, boardID), { label: "board row for " + boardID });
  assert.ok(boardRow.classList.contains("idle"), "a freshly created session's row starts idle: " + boardRow.className);

  const boardClassSeq = watchAttrSequence(w, boardRow, "class");
  const boardPhaseSeq = watchTextSequence(w, boardRow.querySelector(".phase"));

  assert.equal((await promptAsync(boardID, "run the board-transition turn")).status, 202, "prompt_async accepted");
  await waitIdle(boardID, 15);
  await waitFor(() => boardRow.classList.contains("idle"), { label: "board row settling back to idle" });

  boardClassSeq.stop();
  boardPhaseSeq.stop();
  const boardClasses = boardClassSeq.values();
  const boardPhases = boardPhaseSeq.values();
  assert.ok(boardClasses.some((c) => c && c.split(" ").includes("live")), "row must have gone 'live' at some point: " + JSON.stringify(boardClasses));
  assert.ok(boardPhases.includes("tool"), "phase word must have shown 'tool' for the real bash call: " + JSON.stringify(boardPhases));
  assert.ok(boardPhases.includes("streaming") || boardPhases.includes("between"), "phase word must have shown streaming text deltas: " + JSON.stringify(boardPhases));
  assert.equal(boardRow.querySelector(".detail").textContent, "completed", "row settles on the turn's real outcome");
  console.error("PASS: board transitions — phase words seen " + JSON.stringify(boardPhases) + ", css tiers seen " + JSON.stringify(boardClasses));

  // ---- 3. Staleness (scenario 3): a real, longer bash sleep against the
  // shrunk test-only QUIET_MS/STALL_MS crosses both tiers live. ----
  const staleID = await createSession(ProvStallStale);
  const staleRow = await waitFor(() => findRow(doc, staleID), { label: "board row for " + staleID });
  const staleClassSeq = watchAttrSequence(w, staleRow, "class");

  assert.equal((await promptAsync(staleID, "run the staleness turn")).status, 202);
  await waitFor(() => staleRow.classList.contains("bad"), { timeoutMs: 6000, label: "row reaching the stalled ('bad') tier" });
  await waitIdle(staleID, 15);
  await waitFor(() => staleRow.classList.contains("idle"), { label: "row settling idle after the stalled turn resolves" });

  staleClassSeq.stop();
  const staleClasses = staleClassSeq.values();
  assert.ok(staleClasses.some((c) => c && c.split(" ").includes("live")), "must have been live: " + JSON.stringify(staleClasses));
  assert.ok(staleClasses.some((c) => c && c.split(" ").includes("quiet")), "must have crossed the quiet tier: " + JSON.stringify(staleClasses));
  assert.ok(staleClasses.some((c) => c && c.split(" ").includes("bad")), "must have crossed the stalled tier: " + JSON.stringify(staleClasses));
  console.error("PASS: staleness tiers observed live: " + JSON.stringify(staleClasses));

  // ---- 4a. Detail history (scenario 4): open the now-completed board
  // session via a real row click; operator/assistant/tool entries render,
  // the completed fold starts collapsed. ----
  await openDetailViaClick(w, doc, boardRow);
  await waitFor(() => doc.querySelectorAll("#transcript .msg, #transcript details.toolfold").length > 0, { label: "transcript populated from real history" });
  assert.ok(operatorTexts(doc).includes("run the board-transition turn"), "operator entry with the original prompt text: " + JSON.stringify(operatorTexts(doc)));
  assert.ok(assistantTexts(doc).some((t) => t.includes("all set")), "assistant final reply text present: " + JSON.stringify(assistantTexts(doc)));
  const completedFold = doc.querySelector("#transcript details.toolfold");
  assert.ok(completedFold, "a tool fold must be present in history");
  assert.equal(completedFold.open, false, "a completed fold discovered from history starts collapsed");
  assert.ok(completedFold.querySelector(".tool-summary").textContent.includes("bash"), "fold names the real bash tool: " + completedFold.querySelector(".tool-summary").textContent);
  console.error("PASS: detail history — operator/assistant/tool entries render, completed fold collapsed");

  // ---- 4b. A running tool fold observed LIVE (task gap b): open detail
  // while the tool is genuinely still executing. ----
  const liveToolID = await createSession(ProvToolDetail);
  const liveToolRow = await waitFor(() => findRow(doc, liveToolID), { label: "board row for " + liveToolID });
  assert.equal((await promptAsync(liveToolID, "trigger the live running tool fold")).status, 202);
  await waitFor(() => liveToolRow.querySelector(".phase").textContent === "tool", { timeoutMs: 4000, label: "board observes the tool phase before opening detail" });

  await openDetailViaClick(w, doc, liveToolRow);
  const runningFold = await waitFor(() => doc.querySelector("#transcript details.toolfold"), { label: "running tool fold rendered in detail" });
  assert.equal(runningFold.open, true, "a running tool fold must render OPEN");
  assert.ok(runningFold.querySelector(".t-elapsed").textContent.startsWith("running"), "fold summary says running: " + runningFold.querySelector(".t-elapsed").textContent);
  console.error("PASS: a running tool fold is visible, open, while the real tool executes");

  await waitIdle(liveToolID, 15);
  await waitFor(() => !runningFold.querySelector(".t-elapsed").textContent.startsWith("running"), { label: "fold settles to completed live" });
  assert.equal(runningFold.open, true, "a fold must never be force-collapsed by a later patch, per index.html's syncTranscript contract");
  console.error("PASS: the SAME fold transitions to completed live, without being force-collapsed");

  // ---- 4c. Composer send into a BUSY session (task gap a): prompt.queued
  // optimistic entry, then dedup once the durable message lands. ----
  const dedupID = await createSession(ProvStallDedup);
  const dedupRow = await waitFor(() => findRow(doc, dedupID), { label: "board row for " + dedupID });
  assert.equal((await promptAsync(dedupID, "start the occupant turn")).status, 202);
  await waitFor(() => dedupRow.querySelector(".phase").textContent === "tool", { timeoutMs: 4000, label: "session busy in its tool phase before the composer send" });

  await openDetailViaClick(w, doc, dedupRow);
  const queuedText = "composer message sent while busy";
  const composerInput = doc.getElementById("composer-input");
  const composerForm = doc.getElementById("composer-form");
  composerInput.value = queuedText;
  composerForm.dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));

  await waitFor(() => operatorTexts(doc).filter((t) => t === queuedText).length === 1, { timeoutMs: 4000, label: "optimistic prompt.queued operator entry" });
  console.error("PASS: composer send into a BUSY session renders the optimistic prompt.queued entry");

  await waitIdle(dedupID, 15); // the occupant turn's own idle (may be fleeting — see below)
  await waitIdle(dedupID, 15); // the auto-dispatched queued turn's idle; a no-op if the first call already caught the final state
  await waitFor(() => dedupRow.classList.contains("idle"), { label: "row settling idle after the queued turn drains" });
  // The durable message the engine actually delivers for an injected queued
  // prompt is TEMPLATE-WRAPPED ("OPERATOR MESSAGES (address these, then
  // continue the task): 1. <text>") — not byte-identical to the optimistic
  // placeholder's raw ev.queue_text — so dedup is proven by substring
  // containment settling to exactly one match, and the raw placeholder text
  // disappearing outright (transcriptModel's FIFO removal — see its
  // "message" case), not by an exact-string count.
  const finalTexts = operatorTexts(doc);
  assert.equal(finalTexts.filter((t) => t === queuedText).length, 0, "the optimistic placeholder must be gone once the durable message lands: " + JSON.stringify(finalTexts));
  assert.equal(finalTexts.filter((t) => t.includes(queuedText)).length, 1, "the durable (wrapped) message must REPLACE the optimistic entry exactly once, never duplicate it: " + JSON.stringify(finalTexts));
  console.error("PASS: the durable message replaced the optimistic entry with no duplicate");

  // ---- 4d. Composer send on an idle session runs a normal turn. ----
  const idleComposeID = await createSession(ProvQuickIdle);
  const idleComposeRow = await waitFor(() => findRow(doc, idleComposeID), { label: "board row for " + idleComposeID });
  await openDetailViaClick(w, doc, idleComposeRow);
  assert.equal(turnMarkCount(doc), 0, "a freshly created, never-prompted session must open with no turn marks: " + turnMarkCount(doc));
  const idleText = "composer message into an idle session";
  composerInput.value = idleText;
  composerForm.dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));
  await waitFor(() => operatorTexts(doc).includes(idleText), { timeoutMs: 4000, label: "operator entry for an idle-session composer send" });
  await waitIdle(idleComposeID, 15);
  await waitFor(() => assistantTexts(doc).some((t) => t.includes("hello")), { timeoutMs: 4000, label: "assistant reply for the idle-session composer send" });
  // Regression for the double turn-mark bug: on the real wire this turn
  // arrives as session.status(busy) -> a "message" event for the operator's
  // own prompt -> assistant deltas. Both transcriptModel's session.status
  // case and foldMessage's user-role branch used to mark the SAME turn
  // independently, rendering two "turn 1" separators for one turn. Exactly
  // one new marker for one turn is the observable this e2e originally
  // missed (it counted operator/assistant text, never turn-marks).
  assert.equal(turnMarkCount(doc), 1, "one composer-sent turn must render exactly one turn-mark, not two: " + turnMarkCount(doc));
  console.error("PASS: composer send on an idle session runs a normal turn (message event, not prompt.queued), marking exactly one turn");

  // ---- 4e. Composer send against an unknown session: real non-2xx error
  // text surfaces inline. ----
  const bogusID = "does-not-exist-" + Date.now();
  w.location.hash = "#s=" + encodeURIComponent(bogusID);
  await sleep(20);
  if (doc.getElementById("detail-sid-full").textContent !== bogusID) {
    // Same jsdom-navigation-support fallback as openDetailViaClick: drive
    // the page's own applyRoute() directly (a plain top-level function
    // declaration in a classic, non-module <script>, so it is a real
    // property of `window`) if the programmatic location.hash assignment's
    // "hashchange" didn't fire.
    w.applyRoute();
  }
  await waitFor(() => doc.getElementById("detail-sid-full").textContent === bogusID, { label: "detail view opens for the bogus session id" });
  composerInput.value = "this send should fail";
  composerForm.dispatchEvent(new w.Event("submit", { bubbles: true, cancelable: true }));
  await waitFor(() => doc.getElementById("composer-err").textContent.trim().length > 0, { timeoutMs: 4000, label: "composer error text for an unknown session" });
  const composerErrText = doc.getElementById("composer-err").textContent;
  assert.ok(composerErrText.includes("no such session"), "composer error should surface the server's real error text: " + composerErrText);
  console.error("PASS: composer send against an unknown session surfaces the server's real non-2xx error text: " + composerErrText);

  // ---- 4f. Detail transcript error entry (task gap: a failed turn used to
  // render NOTHING for the failure itself). A REAL provider stream failure
  // (ProvStreamError's scriptedStream.Next() returns a genuine Go error, not
  // a simulated event — see stub.go's errorTurns) drives
  // server/handlers.go's runPrompt into its session.error +
  // turn.end(outcome:"error") default branch. The detail view is opened
  // BEFORE the prompt is sent so this observes the LIVE transition (not
  // history already at rest) — the board row settles on the real "error"
  // outcome, the transcript renders a critical error entry carrying the
  // server's real (sanitized) error text, and the live chip drops to idle
  // well inside a bound tighter than index.html's 5s GET /session poll
  // interval, proving it is NOT waiting on that poll. ----
  const errorID = await createSession(ProvStreamError);
  const errorRow = await waitFor(() => findRow(doc, errorID), { label: "board row for " + errorID });
  await openDetailViaClick(w, doc, errorRow);

  assert.equal((await promptAsync(errorID, "trigger a real provider stream error")).status, 202, "prompt_async accepted");
  await waitFor(() => doc.querySelector("#transcript .msg.error"), { label: "a critical error entry rendered in the detail transcript" });
  const errorEntryText = doc.querySelector("#transcript .msg.error .body p").textContent;
  assert.ok(errorEntryText.includes(STREAM_ERROR_TEXT), "the error entry must carry the server's REAL error text, not a placeholder: " + errorEntryText);
  console.error("PASS: a real provider stream failure renders a critical error entry with the server's real error text: " + errorEntryText);

  const chip = doc.getElementById("detail-livechip");
  await waitFor(() => chip.hidden === true, { timeoutMs: 3000, label: "the live chip settling to idle well under the 5s poll interval, driven by the live session.status(idle) event" });
  await waitFor(() => errorRow.classList.contains("idle"), { label: "board row settling idle after the failed turn" });
  assert.equal(errorRow.querySelector(".detail").textContent, "error", "row shows the real turn.end outcome after a stream failure: " + errorRow.querySelector(".detail").textContent);
  console.error("PASS: the live chip settles to idle promptly (no poll dependency), and the board row shows the real 'error' outcome");

  // ---- 4g. detail liveEvents buffer cap (finding 3 — PERF): a real
  // scripted turn streams 6 text deltas (stub.go's capTurns), each with no
  // seq of its own (see keepsLiveEventAfterReconcile's doc comment) —
  // comfortably overshooting the tuned DETAIL_LIVE_EVENTS_CAP (2).
  // handleDetailEvent must trigger reconcileDetail() once the buffer
  // crosses that cap; this asserts BOTH halves — that liveEvents actually
  // grew past the cap while the turn streamed (proving this scenario
  // exercises something real — read via liveEventsPeakLength, NOT a
  // liveEventsLength() polling loop: a real turn with no bash sleep can
  // cross the cap and get reconciled back down within a single JS
  // microtask, entirely between two poll samples, so the peak counter is
  // the only way to reliably observe the spike happened at all), and that
  // liveEvents itself shrinks back down once reconcileDetail's GET /message
  // re-fetch resolves (proving the trigger actually fires and its filter
  // actually trims the buffer, not just that the turn eventually
  // finished). ----
  const capID = await createSession(ProvLiveCap);
  const capRow = await waitFor(() => findRow(doc, capID), { label: "board row for " + capID });
  await openDetailViaClick(w, doc, capRow);
  assert.equal((await promptAsync(capID, "trigger the live-events buffer cap")).status, 202, "prompt_async accepted");

  // Polled, not a single-shot check right after waitIdle: waitIdle resolves
  // via a DIRECT box long-poll, independent of whether THIS page's own SSE
  // stream has finished delivering everything yet — under heavier system
  // load, a one-shot check immediately after waitIdle can race that
  // delivery lag and observe an artificially low peak. Polling here (before
  // waitIdle, not after) waits out that lag instead of racing it.
  const capPeak = await waitFor(() => {
    const peak = w.__monitorDebug.liveEventsPeakLength();
    return peak !== null && peak > TUNING.DETAIL_LIVE_EVENTS_CAP ? peak : null;
  }, { timeoutMs: 4000, label: "liveEvents actually crosses the tuned cap (" + TUNING.DETAIL_LIVE_EVENTS_CAP + ") at its peak while the turn streams" });
  console.error("PASS: liveEvents crossed the tuned buffer cap (peak " + capPeak + ") while a real scripted turn streamed");

  await waitIdle(capID, 15);
  await waitFor(() => {
    const n = w.__monitorDebug.liveEventsLength();
    // A small slack margin above the raw cap: reconcileDetail's own
    // in-flight guard (detailState.reconciling) means the LAST couple of
    // events pushed while a reconcile was already outstanding may still be
    // sitting in the buffer until the NEXT trigger — the assertion is
    // "shrank back down near the cap", not "never even momentarily one
    // or two over it".
    return n !== null && n <= TUNING.DETAIL_LIVE_EVENTS_CAP + 2;
  }, { timeoutMs: 4000, label: "liveEvents shrinks back down after reconcileDetail's buffer-cap trigger resolves" });
  console.error("PASS: liveEvents shrank back down after reconcileDetail's buffer-cap trigger — the PERF fix (finding 3) closes the loop");

  // ---- 5. Reconnect (scenario 5): a real server-side kill/restart of the
  // box's HTTP layer flips the header honestly and resumes. ----
  w.location.hash = "#";
  await waitFor(() => !doc.body.classList.contains("showing-detail"), { label: "back to the board before the reconnect scenario" });
  await waitFor(() => doc.getElementById("conn-text").textContent === "streaming", { label: "streaming before the kill" });

  assert.equal((await fetch(monitorBase + "/__control/kill", { method: "POST" })).status, 200, "control-plane kill");
  await waitFor(() => doc.getElementById("conn-text").textContent === "reconnecting…", { timeoutMs: 5000, label: "header flips to reconnecting after a real server-side kill" });
  console.error("PASS: header honestly flips to reconnecting… after a real server-side kill");

  assert.equal((await fetch(monitorBase + "/__control/restart", { method: "POST" })).status, 200, "control-plane restart");
  await waitFor(() => doc.getElementById("conn-text").textContent === "streaming", { timeoutMs: 8000, label: "stream resumes after a real server-side restart" });
  console.error("PASS: stream resumes after a real server-side restart");

  // Prove the resumed stream is REAL, not just cosmetic header text: a
  // brand-new session created after the restart must still arrive live.
  const postReconnectID = await createSession(ProvQuickIdle);
  await waitFor(() => findRow(doc, postReconnectID), { timeoutMs: 4000, label: "a post-reconnect session.created event reaching the resumed stream" });
  console.error("PASS: a session created after the restart arrived over the resumed stream — reconnect is genuine, not cosmetic");

  // ---- 5b. Reconnect gap heals the detail transcript (finding 1 —
  // MEDIUM): pollOnce silently advances state.lastSeq past whatever a poll
  // snapshot's own maxSeq reports, regardless of whether THIS page's own
  // SSE connection actually delivered everything up to that point — so the
  // NEXT connectStream() resumes from an already-advanced cursor and never
  // redelivers what it missed. The board self-heals for free (every 5s poll
  // REPLACES state.sessions wholesale); the detail view has no equivalent —
  // liveEvents is purely additive. This scenario opens a detail view, kills
  // the box, then — since nothing can be driven while the box is actually
  // down — restarts it and IMMEDIATELY fires the turn's prompt via a RAW
  // direct fetch to the box (never through the monitor page), racing the
  // page's own reconnect timer. TUNING.BACKOFF_MIN is widened specifically
  // so this race is reliably won (a generous multi-second window) rather
  // than depending on exact wall-clock timing luck. waitIdle below
  // long-polls the BOX directly, proving the turn is fully durable on the
  // server regardless of whether the page has reconnected yet — then the
  // assertion is: once the page's own stream DOES resume, reconcileDetail's
  // stream-re-establish trigger must backfill the ENTIRE turn into the
  // detail transcript, which had a chance to observe none of it live. ----
  const gapID = await createSession(ProvReconnectGap);
  const gapRow = await waitFor(() => findRow(doc, gapID), { label: "board row for " + gapID });
  await openDetailViaClick(w, doc, gapRow);
  // Let the initial (empty — a freshly created, never-prompted session)
  // history fetch actually complete before killing, so the kill below
  // tests ONLY the reconnect-gap/reconcile path, not a coincidental race
  // with enterDetail's own unrelated initial fetch.
  await waitFor(() => {
    const note = doc.querySelector("#transcript .transcript-note:not(.err)");
    return !!note && note.textContent === "no messages yet";
  }, { timeoutMs: 4000, label: "initial (empty) history loads before the reconnect-gap kill" });
  assert.equal(turnMarkCount(doc), 0, "the reconnect-gap session must open with no turn marks before the gap turn runs");

  assert.equal((await fetch(monitorBase + "/__control/kill", { method: "POST" })).status, 200, "control-plane kill (reconnect-gap scenario)");
  await waitFor(() => doc.getElementById("conn-text").textContent === "reconnecting…", { timeoutMs: 5000, label: "header flips to reconnecting before the gap turn runs" });

  assert.equal((await fetch(monitorBase + "/__control/restart", { method: "POST" })).status, 200, "control-plane restart (reconnect-gap scenario)");
  const gapPromptText = "trigger the reconnect-gap turn";
  assert.equal((await promptAsync(gapID, gapPromptText)).status, 202, "prompt_async accepted (raw fetch, no monitor page involvement)");
  await waitIdle(gapID, 15);
  console.error("PASS: the reconnect-gap turn completed durably on the server while the monitor page's own stream was still down/reconnecting");

  await waitFor(() => doc.getElementById("conn-text").textContent === "streaming", { timeoutMs: 8000, label: "the monitor's own stream eventually resumes" });

  // The proof: the detail transcript — fed ONLY from liveEvents, and this
  // page's stream was down for the ENTIRE turn — must still end up showing
  // it in full once reconcileDetail's stream-re-establish trigger backfills
  // it from a fresh GET /message snapshot.
  await waitFor(() => operatorTexts(doc).includes(gapPromptText), { timeoutMs: 5000, label: "the operator's prompt text appears in the transcript after reconcile heals the gap" });
  await waitFor(() => assistantTexts(doc).some((t) => t.includes(RECONNECT_GAP_REPLY)), { timeoutMs: 5000, label: "the assistant's reply appears in the transcript after reconcile heals the gap" });
  assert.equal(turnMarkCount(doc), 1, "the reconciled turn renders exactly one turn-mark, not zero (lost) or duplicated: " + turnMarkCount(doc));
  console.error("PASS: reconcileDetail healed the reconnect gap — the detail transcript shows the FULL turn (operator prompt + assistant reply) it could never have observed live, marked exactly once");

  const finalLiveEventsLength = w.__monitorDebug.liveEventsLength();
  assert.ok(finalLiveEventsLength !== null && finalLiveEventsLength < 20, "liveEvents stays bounded after the reconnect-gap reconcile, not accumulating a permanent backlog: " + finalLiveEventsLength);
  console.error("PASS: liveEvents stays bounded after the reconnect-gap reconcile: " + finalLiveEventsLength);

  // ---- 6. embeddedConnectPlan — the "frictionless local" behavior, driven
  // against the box's REAL GET /monitor route (never the static server),
  // each on its OWN fresh page load (openEmbeddedPage: a brand-new JSDOM
  // instance, empty localStorage, no state carried over from any scenario
  // above). ----

  // ---- 6a. An Unauthenticated box (stub.go's unauthBase — RunToken "" +
  // Unauthenticated true) served embedded: the auto-attempt against
  // location.origin with NO token at all must still succeed (an empty
  // Authorization header is fine — see server.Server.authorized), landing
  // straight on the board with nothing typed. ----
  {
    const { dom: unauthDom, doc: unauthDoc } = await openEmbeddedPage(unauthBase + "/monitor");
    await waitFor(() => unauthDoc.body.classList.contains("connected"), { label: "Unauthenticated embedded page auto-connects with zero typing" });
    assert.equal(unauthDoc.getElementById("run-token").value, "", "no token was ever typed or needed");
    assert.equal(unauthDoc.getElementById("base-url-label").hidden, true, "the base field stays hidden — host is this box's own origin");
    assert.ok(unauthDoc.getElementById("hdr-version").textContent.startsWith("harness "), "the identity line must reflect a REAL connected /health response: " + unauthDoc.getElementById("hdr-version").textContent);
    unauthDom.window.close();
    console.error("PASS: an Unauthenticated embedded box auto-connects to the board with NO token entry, zero fields");
  }

  // ---- 6b. A tokened box (boxBase), page loaded with a real "#t=<token>"
  // capability URL: auto-connects with NO manual entry, and the token is
  // scrubbed from the visible URL (extractFragmentToken/history.
  // replaceState) the instant it's adopted. ----
  {
    const { dom: tDom, w: tw, doc: tdoc } = await openEmbeddedPage(boxBase + "/monitor#t=" + encodeURIComponent(token));
    await waitFor(() => tdoc.body.classList.contains("connected"), { label: "a tokened box auto-connects via a #t= capability URL" });
    assert.ok(!tw.location.hash.includes("t="), "the token must be scrubbed from the URL the instant it's adopted: " + tw.location.hash);
    assert.equal(tw.localStorage.getItem("harness.monitor.token"), token, "the fragment token must be adopted into the SAME localStorage key manual entry uses");
    tDom.window.close();
    console.error("PASS: a #t= capability URL auto-connects a tokened box with no typing, and the token is scrubbed from the URL");
  }

  // ---- 6c. A tokened box, loaded with NO token anywhere (no #t=, no
  // stored token — genuinely fresh): the silent auto-attempt fails (401),
  // revealing a TOKEN-ONLY panel (host is known — it's this origin — so
  // the base field stays absent/hidden even once revealed). Typing the
  // real token and submitting still connects normally, proving the
  // fallback panel is fully functional, not just visually reduced. ----
  {
    const { dom: noTokDom, w: noTokW, doc: noTokDoc } = await openEmbeddedPage(boxBase + "/monitor");
    await waitFor(() => noTokDoc.getElementById("connect-err").textContent.trim().length > 0, { label: "the silent auto-attempt with no token fails against a tokened box" });
    assert.ok(noTokDoc.getElementById("connect-err").textContent.includes("rejected"), "the real 401 rejection text must surface: " + noTokDoc.getElementById("connect-err").textContent);
    assert.equal(noTokDoc.getElementById("base-url-label").hidden, true, "host is known (this origin) — the base field stays hidden even in the fallback panel");
    assert.ok(!noTokDoc.body.classList.contains("connected"), "must not be connected yet");

    noTokDoc.getElementById("run-token").value = token;
    noTokDoc.getElementById("connect-form").dispatchEvent(new noTokW.Event("submit", { bubbles: true, cancelable: true }));
    await waitFor(() => noTokDoc.body.classList.contains("connected"), { label: "manually typing the real token into the fallback token-only panel connects normally" });
    noTokDom.window.close();
    console.error("PASS: a tokened box with no token available falls back to a token-only panel (host absent), which is fully functional");
  }

  dom.window.close();
  console.error("ALL REAL END-TO-END CHECKS PASSED");
}

main()
  .then(() => process.exit(0))
  .catch((e) => {
    console.error(e);
    process.exit(1);
  });
