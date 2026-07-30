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

const [, , boxBase, monitorBase, token] = process.argv;
if (!boxBase || !monitorBase || !token) {
  console.error("usage: node real_e2e.mjs <box_base> <monitor_base> <token>");
  process.exit(2);
}
console.error("box:", boxBase, "monitor:", monitorBase);

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

// TUNING shrinks the monitor's staleness thresholds (production QUIET_MS/
// STALL_MS are 15000/60000 — see index.html) down to something a real,
// bounded bash `sleep` can cross inside a CI-sane test budget. Read by
// index.html's window.__monitorTuning seam, set below via JSDOM's
// beforeParse so it lands before the page's inline <script> ever runs.
//
// The gap between QUIET_MS and STALL_MS must be comfortably wider than the
// board's own 1s ticker (index.html's TICK_MS): while a session sits mid-
// tool-call with no new events, staleness is only ever RE-EVALUATED on that
// periodic tick (there is nothing else to trigger a re-render) — a gap
// narrower than one tick period means the "quiet" tier can fall entirely
// between two samples and never be observed, even though it was real.
const TUNING = { QUIET_MS: 200, STALL_MS: 1800 };

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

async function main() {
  // ---- 0. The monitor's static file server must be serving the EXACT
  // committed file (production wiring, not a stale copy) — the monitor has
  // no Go-embedded handler of its own (see stub.go's package doc comment),
  // so this checks the on-disk file the test's static server points at is
  // the real one, the same guarantee tools/hub/e2e's real_e2e.mjs checks via
  // its go:embed'd handler. ----
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

  dom.window.close();
  console.error("ALL REAL END-TO-END CHECKS PASSED");
}

main()
  .then(() => process.exit(0))
  .catch((e) => {
    console.error(e);
    process.exit(1);
  });
