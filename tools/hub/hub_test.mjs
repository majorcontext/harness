// Unit tests for the pure helpers in tools/hub/index.html.
//
// Same extraction trick as tools/inspector/inspector_test.mjs: read
// index.html, pull out the region between the TESTABLE-BEGIN/END markers,
// and evaluate it in a node:vm sandbox exposing only Date and JSON. Keeps
// the hub a build-free single file while its logic stays unit-tested.
//
// Run: node --test tools/hub/

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(here, "index.html"), "utf8");

const begin = "/* TESTABLE-BEGIN";
const end = "/* TESTABLE-END */";
const bi = html.indexOf(begin);
const ei = html.indexOf(end);
assert.ok(bi >= 0 && ei > bi, "TESTABLE markers must be present in index.html");
const afterBegin = html.indexOf("*/", bi) + 2;
const source = html.slice(afterBegin, ei);

const sandbox = { Date, JSON };
vm.createContext(sandbox);
vm.runInContext(source, sandbox);

// plain rebuilds a value produced inside the vm sandbox into this module's
// own realm: object/array literals evaluated by vm-compiled source carry
// the sandbox's Object/Array prototypes, which assert's strict deepEqual
// treats as unequal to this-realm literals even when structurally
// identical. A JSON round-trip run in THIS realm (not the sandbox's)
// produces plain, this-realm objects — the same trick tools/inspector's
// test suite uses (see its "collect"/"plain" helpers) for cross-realm
// comparisons.
function plain(v) {
  return v === undefined ? v : JSON.parse(JSON.stringify(v));
}
const {
  shortId,
  prettyJSON,
  partsText,
  maxSeq,
  createSSEParser,
  fmtRelative,
  sessionBadge,
  goalSnippet,
  lastTurnSummary,
  fmtTokens,
  usageSummary,
  reduceGoal,
  goalFromSession,
  encodeHubState,
  decodeHubState,
  notifyForEvent,
  sortSessions,
  sessionTimeRank,
  sessionPriorityRank,
  countByState,
  randomSlug,
  createCoalescer,
  planAppend,
  isPinnedToBottom,
  shouldResort,
  boxCardSignature,
  sessionRowSignature,
  buildLineages,
  collectActiveGoals,
  goalPauseTreatment,
  rearmNeeded,
  canRedispatch,
  normalizeProcesses,
  normalizePorts,
  processStateBadge,
  portLinksFor,
  parsePortURLLines,
  mergePortURLs,
  sanitizePortURLs,
} = sandbox;

/* ---------- fmtRelative ---------- */

test("fmtRelative: just now for sub-10s", () => {
  const now = Date.parse("2024-01-01T00:00:10Z");
  assert.equal(fmtRelative("2024-01-01T00:00:05Z", now), "just now");
  assert.equal(fmtRelative("2024-01-01T00:00:10Z", now), "just now");
});

test("fmtRelative: seconds, minutes, hours, days", () => {
  const now = Date.parse("2024-01-02T00:00:00Z");
  assert.equal(fmtRelative("2024-01-01T23:59:30Z", now), "30s ago");
  assert.equal(fmtRelative("2024-01-01T23:55:00Z", now), "5m ago");
  assert.equal(fmtRelative("2024-01-01T18:00:00Z", now), "6h ago");
  assert.equal(fmtRelative("2023-12-30T00:00:00Z", now), "3d ago");
});

test("fmtRelative: falls back to a locale date past a week", () => {
  const now = Date.parse("2024-02-01T00:00:00Z");
  const got = fmtRelative("2024-01-01T00:00:00Z", now);
  assert.doesNotMatch(got, /ago$/);
  assert.notEqual(got, "");
});

test("fmtRelative: empty/invalid input yields empty string", () => {
  assert.equal(fmtRelative("", 0), "");
  assert.equal(fmtRelative(null, 0), "");
  assert.equal(fmtRelative("not a date", 0), "");
});

test("fmtRelative: future timestamps clamp to just now, never negative", () => {
  const now = Date.parse("2024-01-01T00:00:00Z");
  assert.equal(fmtRelative("2024-01-01T00:05:00Z", now), "just now");
});

/* ---------- sessionBadge ---------- */

test("sessionBadge: goal.active always wins regardless of status", () => {
  assert.equal(sessionBadge({ status: "idle", goal: { active: true } }), "goal-running");
  assert.equal(sessionBadge({ status: "busy", goal: { active: true } }), "goal-running");
});

test("sessionBadge: busy without an active goal", () => {
  assert.equal(sessionBadge({ status: "busy" }), "busy");
  assert.equal(sessionBadge({ status: "busy", goal: { active: false } }), "busy");
});

test("sessionBadge: idle default", () => {
  assert.equal(sessionBadge({ status: "idle" }), "idle");
  assert.equal(sessionBadge({}), "idle");
  assert.equal(sessionBadge(null), "idle");
});

/* ---------- goalSnippet ---------- */

test("goalSnippet: short condition passes through untouched", () => {
  assert.equal(goalSnippet("ship it"), "ship it");
});

test("goalSnippet: truncates long conditions with an ellipsis", () => {
  const long = "x".repeat(200);
  const got = goalSnippet(long, 80);
  assert.equal(got.length, 80);
  assert.ok(got.endsWith("…"));
});

test("goalSnippet: empty/missing condition is empty string", () => {
  assert.equal(goalSnippet(""), "");
  assert.equal(goalSnippet(null), "");
});

/* ---------- lastTurnSummary ---------- */

test("lastTurnSummary: completed", () => {
  assert.equal(lastTurnSummary({ outcome: "completed" }), "completed");
});

test("lastTurnSummary: error includes the error text", () => {
  assert.equal(lastTurnSummary({ outcome: "error", error: "boom" }), "error: boom");
});

test("lastTurnSummary: context_exhausted without error text", () => {
  assert.equal(lastTurnSummary({ outcome: "context_exhausted" }), "context_exhausted");
});

test("lastTurnSummary: absent/empty last_turn is empty string", () => {
  assert.equal(lastTurnSummary(null), "");
  assert.equal(lastTurnSummary({}), "");
});

/* ---------- fmtTokens / usageSummary ---------- */

test("fmtTokens: compact thousands/millions", () => {
  assert.equal(fmtTokens(0), "0");
  assert.equal(fmtTokens(999), "999");
  assert.equal(fmtTokens(1000), "1k");
  assert.equal(fmtTokens(1234), "1.2k");
  assert.equal(fmtTokens(1500000), "1.5M");
});

test("usageSummary: formats input/output tokens", () => {
  assert.equal(usageSummary({ input_tokens: 1234, output_tokens: 567 }), "1.2k in / 567 out");
  assert.equal(usageSummary(null), "");
});

/* ---------- reduceGoal (extended with goal.stalled) ---------- */

test("reduceGoal: full lifecycle including a retryable stall", () => {
  let g = reduceGoal(null, { type: "goal.set", goal_condition: "ship it" });
  assert.equal(g.condition, "ship it");
  assert.equal(g.active, true);

  g = reduceGoal(g, {
    type: "goal.stalled",
    goal_reason: "provider overloaded",
    goal_attempt: 2,
    goal_retryable: true,
    goal_retryable_class: "overloaded",
    goal_waiting: true,
  });
  assert.equal(g.active, true, "a stall is non-terminal");
  assert.equal(g.attempt, 2);
  assert.equal(g.retryable, true);
  assert.equal(g.retryableClass, "overloaded");
  assert.equal(g.waiting, true);

  g = reduceGoal(g, { type: "goal.achieved", goal_reason: "done", goal_turns: 4 });
  assert.equal(g.active, false);
  assert.equal(g.achieved, true);
  assert.equal(g.attempt, 0, "achieved resets the stall counter");
  assert.equal(g.turns, 4);
});

test("reduceGoal: goal.eval resets stall fields", () => {
  let g = reduceGoal(null, { type: "goal.set", goal_condition: "x" });
  g = reduceGoal(g, { type: "goal.stalled", goal_attempt: 1, goal_retryable: true, goal_waiting: true });
  assert.equal(g.attempt, 1);
  g = reduceGoal(g, { type: "goal.eval", goal_met: false, goal_reason: "not yet", goal_turn: 1 });
  assert.equal(g.attempt, 0);
  assert.equal(g.retryable, false);
  assert.equal(g.waiting, false);
  assert.equal(g.reason, "not yet");
});

test("reduceGoal: non-goal events are ignored, returning the same reference", () => {
  const prev = { condition: "x", active: true };
  assert.equal(reduceGoal(prev, { type: "message" }), prev);
});

/* ---------- encodeHubState / decodeHubState ---------- */

test("encodeHubState/decodeHubState: round-trips boxes and view", () => {
  const s = {
    boxes: [{ id: "b1", name: "box one", base: "http://localhost:4096", token: "t0k" }],
    view: { box: "b1", session: "ses_1" },
    notify: true,
  };
  const frag = encodeHubState(s);
  assert.ok(frag.startsWith("s="));
  const decoded = plain(decodeHubState(frag));
  assert.deepEqual(decoded.boxes, s.boxes.map(b => ({ ...b, port_urls: {} })));
  assert.deepEqual(decoded.view, s.view);
  assert.equal(decoded.notify, true);
});

test("encodeHubState/decodeHubState: round-trips a box's port_urls map", () => {
  const s = {
    boxes: [{ id: "b1", name: "n", base: "http://x", token: "t", port_urls: { "3000": "https://a.example" } }],
    view: {}, notify: false,
  };
  const decoded = plain(decodeHubState(encodeHubState(s)));
  assert.deepEqual(decoded.boxes[0].port_urls, { "3000": "https://a.example" });
});

test("decodeHubState: a garbage port_urls value (array, non-object, non-string values) sanitizes to {}", () => {
  const raw = JSON.stringify({ boxes: [{ id: "b1", base: "http://x", token: "t", port_urls: ["oops"] }] });
  const b64 = Buffer.from(raw, "utf8").toString("base64");
  const decoded = plain(decodeHubState("s=" + b64));
  assert.deepEqual(decoded.boxes[0].port_urls, {});
});

test("encodeHubState/decodeHubState: round-trips through a full #-prefixed hash", () => {
  const s = { boxes: [{ id: "b", name: "b", base: "http://x", token: "t" }], view: {}, notify: false };
  const decoded = decodeHubState("#" + encodeHubState(s));
  assert.equal(decoded.boxes.length, 1);
  assert.equal(decoded.boxes[0].base, "http://x");
});

test("encodeHubState: strips trailing slashes from base URLs", () => {
  const s = { boxes: [{ id: "b", name: "b", base: "http://x/////", token: "t" }], view: {}, notify: false };
  const decoded = decodeHubState(encodeHubState(s));
  assert.equal(decoded.boxes[0].base, "http://x");
});

test("encodeHubState: round-trips a long multi-paragraph unicode condition inside view", () => {
  const cond = "ship it 🚀 — with ünïcödé, and\nnewlines, and ".repeat(50);
  const s = { boxes: [], view: { draftCondition: cond }, notify: false };
  const decoded = decodeHubState(encodeHubState(s));
  assert.equal(decoded.view.draftCondition, cond);
});

test("decodeHubState: tolerant of garbage fragments", () => {
  for (const bad of ["", "#", "garbage", "#garbage!!!", "#s=", "#s=not-valid-base64!!!", "#s=" + "%".repeat(20), null, undefined]) {
    const decoded = plain(decodeHubState(bad));
    assert.deepEqual(decoded.boxes, []);
    assert.deepEqual(decoded.view, {});
    assert.equal(decoded.notify, false);
  }
});

test("decodeHubState: tolerant of well-formed JSON with the wrong shape", () => {
  const frag = encodeHubState(42);
  const decoded = plain(decodeHubState(frag));
  assert.deepEqual(decoded.boxes, []);
});

test("decodeHubState: drops malformed box entries but keeps good ones", () => {
  const raw = JSON.stringify({
    boxes: [
      { id: "b1", name: "good", base: "http://good", token: "t" },
      { id: "b2", name: "missing token" },
      "not an object",
      null,
    ],
  });
  const b64 = Buffer.from(raw, "utf8").toString("base64");
  const decoded = decodeHubState("s=" + b64);
  assert.equal(decoded.boxes.length, 1);
  assert.equal(decoded.boxes[0].name, "good");
});

/* ---------- notifyForEvent ---------- */

test("notifyForEvent: goal achieved", () => {
  const n = notifyForEvent({ type: "goal.achieved", goal_reason: "done" });
  assert.ok(n);
  assert.match(n.title, /achieved/i);
  assert.equal(n.body, "done");
});

test("notifyForEvent: turn.end error", () => {
  const n = notifyForEvent({ type: "turn.end", outcome: "error", error: "boom" });
  assert.ok(n);
  assert.match(n.title, /fail|error/i);
  assert.equal(n.body, "boom");
});

test("notifyForEvent: turn.end completed is not notify-worthy", () => {
  assert.equal(notifyForEvent({ type: "turn.end", outcome: "completed" }), null);
});

test("notifyForEvent: unrelated events are not notify-worthy", () => {
  assert.equal(notifyForEvent({ type: "text.delta" }), null);
  assert.equal(notifyForEvent(null), null);
});

/* ---------- sortSessions / sessionTimeRank / sessionPriorityRank ----------
   Replaces the old sortByLastActivity, which degenerated to server/
   insertion order whenever last_activity_at was null/absent (new
   Date(null) - new Date(null) is NaN, and Array#sort's behavior on a
   comparator that returns NaN is unspecified/effectively "leave it alone")
   — an operator-reported bug where a box on an older harness binary
   returns last_activity_at: null for every session, burying the one
   actively running session at the bottom of the fleet list. */

test("sortSessions: newest last_activity_at first within the same state, without mutating input", () => {
  const input = [
    { id: "a", status: "idle", last_activity_at: "2024-01-01T00:00:00Z" },
    { id: "b", status: "idle", last_activity_at: "2024-03-01T00:00:00Z" },
    { id: "c", status: "idle", last_activity_at: "2024-02-01T00:00:00Z" },
  ];
  const out = sortSessions(input);
  assert.deepEqual(plain(out.map(s => s.id)), ["b", "c", "a"]);
  assert.equal(input[0].id, "a", "input must not be mutated");
});

test("sortSessions: active states sort before idle regardless of timestamps", () => {
  const input = [
    { id: "idle-newer", status: "idle", last_activity_at: "2024-06-01T00:00:00Z" },
    { id: "busy-older", status: "busy", last_activity_at: "2024-01-01T00:00:00Z" },
    { id: "goal-oldest", status: "idle", last_activity_at: "2023-01-01T00:00:00Z", goal: { active: true } },
  ];
  const out = sortSessions(input);
  assert.deepEqual(plain(out.map(s => s.id)), ["goal-oldest", "busy-older", "idle-newer"]);
});

test("sortSessions: all last_activity_at null falls back to created_at, never degenerating to input order", () => {
  // The reported bug: an older harness binary omits last_activity_at for
  // every session. The actively running (busy) session must still land at
  // the top, not the bottom, purely from its state — with created_at as
  // the within-state tiebreaker.
  const input = [
    { id: "idle-newer", status: "idle", last_activity_at: null, created_at: "2024-03-01T00:00:00Z" },
    { id: "idle-older", status: "idle", last_activity_at: null, created_at: "2024-01-01T00:00:00Z" },
    { id: "running", status: "busy", last_activity_at: null, created_at: "2024-02-01T00:00:00Z" },
  ];
  const out = sortSessions(input);
  assert.deepEqual(plain(out.map(s => s.id)), ["running", "idle-newer", "idle-older"]);
});

test("sortSessions: mixed nulls — a session with neither timestamp sorts last within its state", () => {
  const input = [
    { id: "no-timestamps", status: "idle" },
    { id: "has-last-activity", status: "idle", last_activity_at: "2024-01-01T00:00:00Z" },
    { id: "has-created-only", status: "idle", created_at: "2023-01-01T00:00:00Z" },
  ];
  const out = sortSessions(input);
  assert.deepEqual(plain(out.map(s => s.id)), ["has-last-activity", "has-created-only", "no-timestamps"]);
});

test("sortSessions: is a total order — never throws/NaNs on a fully-timestampless, mixed-state fixture", () => {
  const fixture = [
    { id: "a", status: "idle" },
    { id: "b", status: "busy" },
    { id: "c", status: "idle", goal: { active: true } },
    { id: "d", status: "busy", last_activity_at: null, created_at: null },
    { id: "e" },
  ];
  const out = sortSessions(fixture);
  assert.equal(out.length, fixture.length);
  // goal-running first, then busy (stable-ish among busy by whatever
  // recency they have), then idle/unknown-status last.
  assert.equal(out[0].id, "c");
  assert.deepEqual(plain(out.slice(1, 3).map(s => s.id).sort()), ["b", "d"]);
});

test("sessionTimeRank: prefers last_activity_at, falls back to created_at, then -Infinity", () => {
  assert.equal(sessionTimeRank({ last_activity_at: "2024-01-01T00:00:00Z" }), Date.parse("2024-01-01T00:00:00Z"));
  assert.equal(sessionTimeRank({ created_at: "2023-01-01T00:00:00Z" }), Date.parse("2023-01-01T00:00:00Z"));
  assert.equal(sessionTimeRank({}), -Infinity);
  assert.equal(sessionTimeRank(null), -Infinity);
  assert.equal(sessionTimeRank({ last_activity_at: "not a date" }), -Infinity);
});

test("sessionPriorityRank: goal-running < busy < idle", () => {
  assert.ok(sessionPriorityRank({ status: "idle", goal: { active: true } }) < sessionPriorityRank({ status: "busy" }));
  assert.ok(sessionPriorityRank({ status: "busy" }) < sessionPriorityRank({ status: "idle" }));
});

/* ---------- countByState ---------- */

test("countByState: tallies sessions by derived badge", () => {
  const sessions = [
    { status: "busy" },
    { status: "idle" },
    { status: "idle", goal: { active: true } },
    { status: "busy", goal: { active: true } },
  ];
  assert.deepEqual(plain(countByState(sessions)), { idle: 1, busy: 1, "goal-running": 2 });
});

/* ---------- shared helpers carried over from the inspector ---------- */

test("shortId / prettyJSON / partsText / maxSeq behave as in the inspector", () => {
  assert.equal(shortId("sess_0123456789abcdef"), "sess_012345");
  assert.equal(prettyJSON({ a: 1 }), '{\n  "a": 1\n}');
  assert.equal(partsText([{ type: "text", text: "hi" }]), "hi");
  assert.equal(maxSeq([{ seq: 1 }, { seq: 9 }]), 9);
});

/* ---------- SSE parser (shared verbatim with the inspector) ---------- */

test("createSSEParser: basic frame", () => {
  const frames = [];
  const feed = createSSEParser(f => frames.push({ id: f.id, data: f.data }));
  feed("data: hello\n\n");
  assert.deepEqual(frames, [{ id: null, data: "hello" }]);
});

test("createSSEParser: chunk boundary mid-frame", () => {
  const frames = [];
  const feed = createSSEParser(f => frames.push({ id: f.id, data: f.data }));
  feed("data: hel");
  feed("lo\n\n");
  assert.deepEqual(frames, [{ id: null, data: "hello" }]);
});

/* ---------- randomSlug ---------- */

test("randomSlug: deterministic under a fixed rand, suffix 00 at rand 0", () => {
  // rand always 0 → first adjective, first noun, num 0 → padded "-00".
  const a = randomSlug(() => 0);
  const b = randomSlug(() => 0);
  assert.equal(a, b, "same rand yields same slug");
  assert.match(a, /^[a-z]+-[a-z]+-00$/);
});

test("randomSlug: two-digit zero-padded suffix", () => {
  // rand just under 1 → last adjective/noun, num 99.
  const slug = randomSlug(() => 0.999);
  assert.match(slug, /^[a-z]+-[a-z]+-99$/);
});

test("randomSlug: always matches the documented shape over many draws", () => {
  let seed = 1;
  const rand = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
  for (let i = 0; i < 200; i++) {
    assert.match(randomSlug(rand), /^[a-z]+-[a-z]+-\d{2}$/);
  }
});

test("randomSlug: both word segments are lowercase across many draws", () => {
  let seed = 7;
  const rand = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
  for (let i = 0; i < 200; i++) {
    const [adj, noun] = randomSlug(rand).split("-");
    assert.match(adj, /^[a-z]+$/);
    assert.match(noun, /^[a-z]+$/);
  }
});

/* ---------- createCoalescer ---------- */

test("createCoalescer: many calls in one tick invoke fn once", () => {
  const queued = [];
  let runs = 0;
  const kick = createCoalescer(cb => queued.push(cb), () => runs++);
  for (let i = 0; i < 5000; i++) kick();
  assert.equal(queued.length, 1, "one scheduled tick despite 5000 calls");
  queued.shift()();
  assert.equal(runs, 1);
});

test("createCoalescer: re-arms after the scheduled tick runs", () => {
  const queued = [];
  let runs = 0;
  const kick = createCoalescer(cb => queued.push(cb), () => runs++);
  kick(); queued.shift()();
  kick(); kick(); queued.shift()();
  assert.equal(runs, 2);
  assert.equal(queued.length, 0);
});

test("createCoalescer: a call from inside fn schedules a fresh tick", () => {
  const queued = [];
  let runs = 0;
  let kick;
  kick = createCoalescer(cb => queued.push(cb), () => { runs++; if (runs === 1) kick(); });
  kick();
  queued.shift()();          // runs fn, which re-kicks
  assert.equal(queued.length, 1, "inner kick scheduled a new tick");
  queued.shift()();
  assert.equal(runs, 2);
});

/* ---------- planAppend (keyed append-only diff planning) ---------- */

test("planAppend: returns only items whose key is unseen, preserving order", () => {
  const existing = new Set(["m:1", "m:2"]);
  const items = [{ id: "1" }, { id: "2" }, { id: "3" }, { id: "4" }];
  const got = planAppend(existing, items, x => "m:" + x.id);
  assert.deepEqual(plain(got.map(x => x.id)), ["3", "4"]);
});

test("planAppend: accepts a plain array/iterable of existing keys, not just a Set", () => {
  const got = planAppend(["m:1"], [{ id: "1" }, { id: "2" }], x => "m:" + x.id);
  assert.deepEqual(plain(got.map(x => x.id)), ["2"]);
});

test("planAppend: skips items whose keyFn returns null/undefined", () => {
  const got = planAppend([], [{ id: "1" }, { id: null }, { id: "2" }], x => x.id == null ? null : "m:" + x.id);
  assert.deepEqual(plain(got.map(x => x.id)), ["1", "2"]);
});

test("planAppend: does not mutate the existingKeys Set passed in", () => {
  const existing = new Set(["m:1"]);
  planAppend(existing, [{ id: "2" }], x => "m:" + x.id);
  assert.deepEqual([...existing], ["m:1"]);
});

test("planAppend: empty items yields empty output", () => {
  assert.deepEqual(plain(planAppend(new Set(), [], x => x)), []);
  assert.deepEqual(plain(planAppend(new Set(), null, x => x)), []);
});

/* ---------- isPinnedToBottom ---------- */

test("isPinnedToBottom: exactly at bottom is pinned", () => {
  assert.equal(isPinnedToBottom(100, 200, 100), true); // 200-100-100=0
});

test("isPinnedToBottom: within default threshold counts as pinned", () => {
  assert.equal(isPinnedToBottom(90, 200, 100), true); // gap=10 <= 24
});

test("isPinnedToBottom: scrolled well above the threshold is not pinned", () => {
  assert.equal(isPinnedToBottom(0, 500, 100), false); // gap=400
});

test("isPinnedToBottom: custom threshold is honored", () => {
  assert.equal(isPinnedToBottom(50, 200, 100), false, "gap=50 > default 24");
  assert.equal(isPinnedToBottom(50, 200, 100, 60), true, "gap=50 <= custom 60");
});

/* ---------- shouldResort ---------- */

test("shouldResort: always true when there is no prior sort timestamp", () => {
  assert.equal(shouldResort(null, 1000), true);
  assert.equal(shouldResort(undefined, 1000), true);
});

test("shouldResort: false before the damping interval elapses", () => {
  assert.equal(shouldResort(1000, 1500), false); // 500ms < 1000ms default
  assert.equal(shouldResort(1000, 1999), false);
});

test("shouldResort: true once the damping interval has elapsed", () => {
  assert.equal(shouldResort(1000, 2000), true);
  assert.equal(shouldResort(1000, 5000), true);
});

test("shouldResort: honors a custom interval", () => {
  assert.equal(shouldResort(1000, 1300, 200), true);
  assert.equal(shouldResort(1000, 1100, 200), false);
});

/* ---------- boxCardSignature ---------- */

test("boxCardSignature: identical inputs produce identical signatures", () => {
  const a = { health: "ok", name: "n", base: "http://x", vcsRevision: "abc123", expanded: true, counts: { idle: 1, busy: 0, "goal-running": 0 } };
  const b = { health: "ok", name: "n", base: "http://x", vcsRevision: "abc123", expanded: true, counts: { idle: 1, busy: 0, "goal-running": 0 } };
  assert.equal(boxCardSignature(a), boxCardSignature(b));
});

test("boxCardSignature: differs when health changes", () => {
  const base = { health: "ok", name: "n", base: "http://x", vcsRevision: "abc", expanded: false, counts: {} };
  assert.notEqual(boxCardSignature(base), boxCardSignature({ ...base, health: "down" }));
});

test("boxCardSignature: differs when expanded toggles", () => {
  const base = { health: "ok", name: "n", base: "http://x", vcsRevision: "abc", expanded: false, counts: {} };
  assert.notEqual(boxCardSignature(base), boxCardSignature({ ...base, expanded: true }));
});

test("boxCardSignature: differs when counts change", () => {
  const base = { health: "ok", name: "n", base: "http://x", vcsRevision: "abc", expanded: false, counts: { idle: 1 } };
  assert.notEqual(boxCardSignature(base), boxCardSignature({ ...base, counts: { idle: 2 } }));
});

/* ---------- sessionRowSignature ---------- */

test("sessionRowSignature: identical sessions produce identical signatures", () => {
  const s = { id: "s1", status: "busy", model: "gpt", usage: { input_tokens: 1, output_tokens: 2 }, last_activity_at: "2024-01-01T00:00:00Z" };
  assert.equal(sessionRowSignature(s, false), sessionRowSignature({ ...s }, false));
});

test("sessionRowSignature: differs when selected state changes", () => {
  const s = { id: "s1", status: "idle" };
  assert.notEqual(sessionRowSignature(s, false), sessionRowSignature(s, true));
});

test("sessionRowSignature: differs when badge-relevant fields change", () => {
  const s = { id: "s1", status: "idle" };
  assert.notEqual(sessionRowSignature(s, false), sessionRowSignature({ ...s, status: "busy" }, false));
});

test("sessionRowSignature: differs when last_activity_at changes", () => {
  const s = { id: "s1", status: "idle", last_activity_at: "2024-01-01T00:00:00Z" };
  assert.notEqual(sessionRowSignature(s, false), sessionRowSignature({ ...s, last_activity_at: "2024-01-01T00:00:05Z" }, false));
});

test("sessionRowSignature: differs when goal condition/active changes", () => {
  const s = { id: "s1", status: "idle", goal: { condition: "ship it", active: true } };
  assert.notEqual(sessionRowSignature(s, false), sessionRowSignature({ ...s, goal: { condition: "ship it", active: false } }, false));
});

test("sessionRowSignature: differs when last_turn changes", () => {
  const s = { id: "s1", status: "idle", last_turn: { outcome: "completed" } };
  assert.notEqual(sessionRowSignature(s, false), sessionRowSignature({ ...s, last_turn: { outcome: "error", error: "boom" } }, false));
});

/* ---------- buildLineages ---------- */

test("buildLineages: a session with no parent_session renders as today (singleton)", () => {
  const sessions = [{ id: "s1", created_at: "2024-01-01T00:00:00Z" }];
  const groups = plain(buildLineages(sessions));
  assert.equal(groups.length, 1);
  assert.equal(groups[0].tip.id, "s1");
  assert.deepEqual(groups[0].earlier, []);
});

test("buildLineages: a chain groups under its most recent tip", () => {
  const sessions = [
    { id: "a", created_at: "2024-01-01T00:00:00Z" },
    { id: "b", parent_session: "a", created_at: "2024-01-02T00:00:00Z" },
    { id: "c", parent_session: "b", created_at: "2024-01-03T00:00:00Z" },
  ];
  const groups = plain(buildLineages(sessions));
  assert.equal(groups.length, 1);
  const g = groups[0];
  assert.equal(g.tip.id, "c");
  assert.deepEqual(g.earlier.map(s => s.id), ["b", "a"]);
});

test("buildLineages: a parent_session pointing outside the given set (cross-box lineage) is an orphan — no grouping", () => {
  const sessions = [
    { id: "a", parent_session: "ses_on_another_box", created_at: "2024-01-01T00:00:00Z" },
    { id: "b", created_at: "2024-01-02T00:00:00Z" },
  ];
  const groups = plain(buildLineages(sessions)).sort((x, y) => x.tip.id.localeCompare(y.tip.id));
  assert.equal(groups.length, 2);
  assert.deepEqual(groups[0].earlier, []);
  assert.deepEqual(groups[1].earlier, []);
});

test("buildLineages: tolerates a cycle without hanging or throwing", () => {
  const sessions = [
    { id: "x", parent_session: "y", created_at: "2024-01-01T00:00:00Z" },
    { id: "y", parent_session: "x", created_at: "2024-01-02T00:00:00Z" },
  ];
  const groups = plain(buildLineages(sessions));
  assert.equal(groups.length, 1);
  assert.equal(groups[0].tip.id, "y");
  assert.deepEqual(groups[0].earlier.map(s => s.id), ["x"]);
});

test("buildLineages: a longer 3-cycle is still tolerated", () => {
  const sessions = [
    { id: "p", parent_session: "q", created_at: "2024-01-01T00:00:00Z" },
    { id: "q", parent_session: "r", created_at: "2024-01-02T00:00:00Z" },
    { id: "r", parent_session: "p", created_at: "2024-01-03T00:00:00Z" },
  ];
  const groups = plain(buildLineages(sessions));
  assert.equal(groups.length, 1);
  assert.equal(groups[0].sessions ? undefined : groups[0].tip.id, "r");
});

test("buildLineages: multiple independent lineages plus loners all present", () => {
  const sessions = [
    { id: "a1", created_at: "2024-01-01T00:00:00Z" },
    { id: "a2", parent_session: "a1", created_at: "2024-01-02T00:00:00Z" },
    { id: "loner", created_at: "2024-01-01T00:00:00Z" },
  ];
  const groups = plain(buildLineages(sessions));
  assert.equal(groups.length, 2);
  const withEarlier = groups.find(g => g.earlier.length > 0);
  assert.ok(withEarlier);
  assert.equal(withEarlier.tip.id, "a2");
  const loner = groups.find(g => g.tip.id === "loner");
  assert.ok(loner);
  assert.deepEqual(loner.earlier, []);
});

test("buildLineages: empty/garbage input yields no groups, never throws", () => {
  assert.deepEqual(plain(buildLineages([])), []);
  assert.deepEqual(plain(buildLineages(null)), []);
  assert.deepEqual(plain(buildLineages([null, { id: "" }, { notAnId: 1 }])), []);
});

/* ---------- collectActiveGoals ---------- */

test("collectActiveGoals: only sessions with an active goal are collected, across boxes", () => {
  const boxes = [
    { id: "b1", name: "box-one", sessions: [
      { id: "s1", goal: { active: true, condition: "ship it" } },
      { id: "s2", goal: { active: false, condition: "done" } },
      { id: "s3" },
    ] },
    { id: "b2", name: "box-two", sessions: [
      { id: "s4", goal: { active: true, condition: "fix bug", paused: true, pause_reason: "provider-backoff" } },
    ] },
  ];
  const got = plain(collectActiveGoals(boxes));
  assert.deepEqual(got.map(g => g.sessionId).sort(), ["s1", "s4"]);
  const s4 = got.find(g => g.sessionId === "s4");
  assert.equal(s4.boxId, "b2");
  assert.equal(s4.boxName, "box-two");
  assert.equal(s4.paused, true);
  assert.equal(s4.pauseReason, "provider-backoff");
});

test("collectActiveGoals: paused/restart entries sort first (most needs-attention)", () => {
  const boxes = [
    { id: "b1", name: "b1", sessions: [
      { id: "running", goal: { active: true, condition: "a" } },
      { id: "needs-rearm", goal: { active: true, condition: "b", paused: true, pause_reason: "restart" } },
    ] },
  ];
  const got = plain(collectActiveGoals(boxes));
  assert.equal(got[0].sessionId, "needs-rearm");
});

test("collectActiveGoals: empty/garbage input yields empty array", () => {
  assert.deepEqual(plain(collectActiveGoals([])), []);
  assert.deepEqual(plain(collectActiveGoals(null)), []);
  assert.deepEqual(plain(collectActiveGoals([{ id: "b1" }, null])), []);
});

/* ---------- goalPauseTreatment ---------- */

test("goalPauseTreatment: not paused yields null", () => {
  assert.equal(goalPauseTreatment(false, ""), null);
  assert.equal(goalPauseTreatment(false, "restart"), null);
});

test("goalPauseTreatment: provider-backoff is calm, not an error", () => {
  const t = goalPauseTreatment(true, "provider-backoff");
  assert.equal(t.calm, true);
  assert.match(t.label, /waiting out provider weather/);
});

test("goalPauseTreatment: restart is not calm (needs a prominent CTA)", () => {
  const t = goalPauseTreatment(true, "restart");
  assert.equal(t.calm, false);
  assert.match(t.label, /re-arm/i);
});

test("goalPauseTreatment: unrecognized reason still renders (never throws), not calm", () => {
  const t = goalPauseTreatment(true, "some-future-reason");
  assert.equal(t.calm, false);
  assert.equal(t.reason, "some-future-reason");
});

test("goalPauseTreatment: worker_failure (Round 7) renders via the generic fallback, not calm", () => {
  // worker_failure has no dedicated branch in goalPauseTreatment — it falls
  // through to the same generic, forward-compatible bucket "some-future-
  // reason" above exercises. That is intentional: a new reason string must
  // never crash the hub, and "paused" is an accurate-enough label even
  // without a bespoke one.
  const t = goalPauseTreatment(true, "worker_failure");
  assert.equal(t.calm, false);
  assert.equal(t.reason, "worker_failure");
  assert.equal(t.label, "paused");
});

/* ---------- rearmNeeded ---------- */

test("rearmNeeded: no condition at all -> none", () => {
  assert.equal(rearmNeeded(false, "", false, ""), "none");
  assert.equal(rearmNeeded(true, "", false, ""), "none");
});

test("rearmNeeded: ended goal (not active) -> normal", () => {
  assert.equal(rearmNeeded(false, "ship it", false, ""), "normal");
});

test("rearmNeeded: active, not paused -> none (still running)", () => {
  assert.equal(rearmNeeded(true, "ship it", false, ""), "none");
});

test("rearmNeeded: active + paused/restart -> prominent", () => {
  assert.equal(rearmNeeded(true, "ship it", true, "restart"), "prominent");
});

test("rearmNeeded: active + paused/provider-backoff -> none (self-clearing, no CTA)", () => {
  assert.equal(rearmNeeded(true, "ship it", true, "provider-backoff"), "none");
});

test("rearmNeeded: active + paused/worker_failure -> normal (loop exited, resumes only on next ordinary activity, which may never arrive on a goal-only box -- operator needs a CTA)", () => {
  assert.equal(rearmNeeded(true, "ship it", true, "worker_failure"), "normal");
});

test("rearmNeeded: active + paused/unrecognized future reason -> none (not special-cased, unlike worker_failure)", () => {
  assert.equal(rearmNeeded(true, "ship it", true, "some-future-reason"), "none");
});

/* ---------- canRedispatch ---------- */

test("canRedispatch: idle session (ended/errored) can be re-dispatched", () => {
  assert.equal(canRedispatch({ id: "s1", status: "idle" }), true);
  assert.equal(canRedispatch({ id: "s1", status: "idle", last_turn: { outcome: "error", error: "boom" } }), true);
});

test("canRedispatch: busy or goal-running sessions cannot be re-dispatched", () => {
  assert.equal(canRedispatch({ id: "s1", status: "busy" }), false);
  assert.equal(canRedispatch({ id: "s1", status: "idle", goal: { active: true, condition: "x" } }), false);
});

/* ---------- normalizePorts / normalizeProcesses / processStateBadge / portLinksFor ---------- */

test("normalizePorts: absent/null ports render as absent, never crash", () => {
  assert.deepEqual(plain(normalizePorts(undefined)), []);
  assert.deepEqual(plain(normalizePorts(null)), []);
});

test("normalizePorts: tolerates an array of numbers or strings", () => {
  assert.deepEqual(plain(normalizePorts([3000, "5432"])), ["3000", "5432"]);
});

test("normalizePorts: tolerates an object-keyed shape", () => {
  assert.deepEqual(plain(normalizePorts({ "3000": {} })), ["3000"]);
});

test("normalizeProcesses: absent/404 (non-array) body yields no strip", () => {
  assert.deepEqual(plain(normalizeProcesses(undefined)), []);
  assert.deepEqual(plain(normalizeProcesses(null)), []);
  assert.deepEqual(plain(normalizeProcesses({ error: "not found" })), []);
});

test("normalizeProcesses: reads name/state/ready/exit_code/log off ProcessInfo.status, tolerating a missing ports field", () => {
  const list = [
    { name: "dev", origin: "config", command: ["pnpm", "dev"], status: { name: "dev", state: "ready", ready: true, log: "/x/.harness/proc/dev.log" } },
  ];
  const got = plain(normalizeProcesses(list));
  assert.equal(got.length, 1);
  assert.equal(got[0].name, "dev");
  assert.equal(got[0].state, "ready");
  assert.equal(got[0].ready, true);
  assert.equal(got[0].log, "/x/.harness/proc/dev.log");
  assert.deepEqual(got[0].ports, []);
});

test("normalizeProcesses: a ports field (parallel-branch addition), when present, is picked up", () => {
  const list = [
    { name: "dev", status: { state: "ready", ports: [3000] } },
  ];
  assert.deepEqual(plain(normalizeProcesses(list))[0].ports, ["3000"]);
});

test("normalizeProcesses: skips entries with no name at all rather than throwing", () => {
  assert.deepEqual(plain(normalizeProcesses([{}, null, { status: {} }])), []);
});

test("processStateBadge: ready is green, starting is amber, exited is red with code", () => {
  assert.equal(processStateBadge("ready").cls, "ready");
  assert.equal(processStateBadge("starting").cls, "starting");
  const exited = processStateBadge("exited", 1);
  assert.equal(exited.cls, "exited");
  assert.match(exited.label, /1/);
});

test("processStateBadge: unknown/empty state never throws", () => {
  assert.equal(processStateBadge("").cls, "unknown");
  assert.equal(processStateBadge(undefined).cls, "unknown");
});

test("portLinksFor: only ports with a known URL in the box's port_urls map are returned", () => {
  const got = plain(portLinksFor(["3000", "5432"], { "3000": "https://x.example" }));
  assert.deepEqual(got, [{ port: "3000", url: "https://x.example" }]);
});

test("portLinksFor: no ports reported, or no matching port_urls entry -> empty, never crash", () => {
  assert.deepEqual(plain(portLinksFor([], { "3000": "https://x.example" })), []);
  assert.deepEqual(plain(portLinksFor(["3000"], undefined)), []);
  assert.deepEqual(plain(portLinksFor(undefined, undefined)), []);
});

/* ---------- parsePortURLLines / mergePortURLs ---------- */

test("parsePortURLLines: parses port=url pairs, one per line", () => {
  const got = plain(parsePortURLLines("3000=https://a.example\n5432=https://b.example\n"));
  assert.deepEqual(got, { "3000": "https://a.example", "5432": "https://b.example" });
});

test("parsePortURLLines: tolerates blank lines and garbage", () => {
  const got = plain(parsePortURLLines("\n  \nnotaport\n3000=https://a.example\n=missingport\n"));
  assert.deepEqual(got, { "3000": "https://a.example" });
});

test("sanitizePortURLs: a plain string map passes through unchanged", () => {
  assert.deepEqual(plain(sanitizePortURLs({ "3000": "https://a.example" })), { "3000": "https://a.example" });
});

test("sanitizePortURLs: garbage (array, null, non-string values) sanitizes to {}", () => {
  assert.deepEqual(plain(sanitizePortURLs(["oops"])), {});
  assert.deepEqual(plain(sanitizePortURLs(null)), {});
  assert.deepEqual(plain(sanitizePortURLs({ "3000": 42 })), {});
});

test("mergePortURLs: incoming wins on collision, tolerates null/undefined", () => {
  assert.deepEqual(plain(mergePortURLs({ "3000": "old" }, { "3000": "new", "4000": "x" })), { "3000": "new", "4000": "x" });
  assert.deepEqual(plain(mergePortURLs(null, { "3000": "x" })), { "3000": "x" });
  assert.deepEqual(plain(mergePortURLs({ "3000": "x" }, null)), { "3000": "x" });
  assert.deepEqual(plain(mergePortURLs(null, null)), {});
});

/* ---------- reduceGoal: goal.paused + paused/pauseReason fields ---------- */

test("reduceGoal: goal.paused (restart, boot-time) sets paused/pauseReason and keeps the goal active", () => {
  const g = reduceGoal(null, { type: "goal.paused", goal_condition: "ship it", goal_paused: true, goal_pause_reason: "restart" });
  assert.equal(g.active, true);
  assert.equal(g.paused, true);
  assert.equal(g.pauseReason, "restart");
  assert.equal(g.condition, "ship it");
});

test("reduceGoal: goal.stalled carrying goal_paused (provider-backoff) sets paused/pauseReason", () => {
  const g = reduceGoal(null, { type: "goal.stalled", goal_paused: true, goal_pause_reason: "provider-backoff", goal_retryable: true, goal_waiting: true });
  assert.equal(g.paused, true);
  assert.equal(g.pauseReason, "provider-backoff");
});

test("reduceGoal: a non-paused goal.stalled clears any prior pause", () => {
  const paused = reduceGoal(null, { type: "goal.stalled", goal_paused: true, goal_pause_reason: "provider-backoff" });
  const cleared = reduceGoal(paused, { type: "goal.stalled", goal_paused: false });
  assert.equal(cleared.paused, false);
  assert.equal(cleared.pauseReason, "");
});

test("reduceGoal: goal.set/goal.eval/goal.achieved/goal.cleared reset paused", () => {
  const paused = reduceGoal(null, { type: "goal.paused", goal_condition: "x", goal_paused: true, goal_pause_reason: "restart" });
  assert.equal(reduceGoal(paused, { type: "goal.set", goal_condition: "y" }).paused, false);
  assert.equal(reduceGoal(paused, { type: "goal.eval", goal_met: false }).paused, false);
  assert.equal(reduceGoal(paused, { type: "goal.achieved" }).paused, false);
  assert.equal(reduceGoal(paused, { type: "goal.cleared" }).paused, false);
});

/* ---------- reduceGoal: goal.parked (Round 7, worker-failure pause) ---------- */

test("reduceGoal: goal.parked sets paused/pauseReason (worker_failure), folds attempt/retryable like goal.stalled, and keeps the goal active", () => {
  let g = reduceGoal(null, { type: "goal.set", goal_condition: "ship it" });
  g = reduceGoal(g, {
    type: "goal.parked", goal_reason: "provider server_error errors exhausted the retry budget",
    goal_attempt: 12, goal_retryable: true, goal_retryable_class: "server_error",
    goal_paused: true, goal_pause_reason: "worker_failure",
  });
  assert.equal(g.active, true, "a park never clears the goal");
  assert.equal(g.paused, true);
  assert.equal(g.pauseReason, "worker_failure");
  assert.equal(g.attempt, 12);
  assert.equal(g.retryable, true);
  assert.equal(g.retryableClass, "server_error");
  assert.equal(g.waiting, false, "a park is never still waiting — it already gave up on this turn");
  assert.equal(g.reason, "provider server_error errors exhausted the retry budget");
});

test("reduceGoal: goal.parked defaults pause_reason to worker_failure when absent, never crashes on missing fields", () => {
  const g = reduceGoal(null, { type: "goal.parked", goal_paused: true });
  assert.equal(g.paused, true);
  assert.equal(g.pauseReason, "worker_failure");
  assert.equal(g.attempt, 0);
  assert.equal(g.retryable, false);
});

test("reduceGoal: goal.set/goal.eval/goal.achieved/goal.cleared reset a worker_failure pause too", () => {
  const parked = reduceGoal(null, { type: "goal.parked", goal_paused: true, goal_pause_reason: "worker_failure" });
  assert.equal(reduceGoal(parked, { type: "goal.set", goal_condition: "y" }).paused, false);
  assert.equal(reduceGoal(parked, { type: "goal.eval", goal_met: false }).paused, false);
  assert.equal(reduceGoal(parked, { type: "goal.achieved" }).paused, false);
  assert.equal(reduceGoal(parked, { type: "goal.cleared" }).paused, false);
});

test("goalFromSession: seeds paused/pauseReason from the session's goal", () => {
  const g = goalFromSession({ goal: { condition: "x", active: true, paused: true, pause_reason: "restart" } });
  assert.equal(g.paused, true);
  assert.equal(g.pauseReason, "restart");
});

test("goalFromSession: absent paused/pause_reason render as absent, never crash", () => {
  const g = goalFromSession({ goal: { condition: "x", active: true } });
  assert.equal(g.paused, false);
  assert.equal(g.pauseReason, "");
});
