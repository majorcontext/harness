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
  encodeHubState,
  decodeHubState,
  notifyForEvent,
  sortByLastActivity,
  countByState,
  randomSlug,
  createCoalescer,
  planAppend,
  isPinnedToBottom,
  shouldResort,
  boxCardSignature,
  sessionRowSignature,
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
  assert.deepEqual(decoded.boxes, s.boxes);
  assert.deepEqual(decoded.view, s.view);
  assert.equal(decoded.notify, true);
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

/* ---------- sortByLastActivity / countByState ---------- */

test("sortByLastActivity: newest last_activity_at first, without mutating input", () => {
  const input = [
    { id: "a", last_activity_at: "2024-01-01T00:00:00Z" },
    { id: "b", last_activity_at: "2024-03-01T00:00:00Z" },
    { id: "c", last_activity_at: "2024-02-01T00:00:00Z" },
  ];
  const out = sortByLastActivity(input);
  assert.deepEqual(out.map(s => s.id), ["b", "c", "a"]);
  assert.equal(input[0].id, "a");
});

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
