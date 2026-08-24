// Unit tests for the pure helpers in tools/inspector/index.html.
//
// The inspector is a single self-contained HTML file with no build step, so
// there is nothing to import. Instead we read index.html, extract the region
// between the /* TESTABLE-BEGIN */ and /* TESTABLE-END */ markers, and evaluate
// it in a node:vm sandbox exposing only Date and JSON. This keeps the page
// build-free while making its parser + helpers reproducibly testable.
//
// Run: node --test tools/inspector/

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
// Start extraction after the BEGIN comment's closing */ so the comment body
// (which itself contains no code) is not part of the evaluated source.
const afterBegin = html.indexOf("*/", bi) + 2;
const source = html.slice(afterBegin, ei);

// Function declarations at the top level of a vm script become properties of
// the sandbox's global object; read them straight off the context.
const sandbox = { Date, JSON };
vm.createContext(sandbox);
vm.runInContext(source, sandbox);
const {
  shortId,
  prettyJSON,
  sortSessions,
  partsText,
  maxSeq,
  createSSEParser,
  reduceGoal,
  goalFromSession,
} = sandbox;

// collect gathers every frame the parser dispatches for the given chunks.
// Frames are rebuilt as plain objects in this realm: the parser creates them
// inside the vm sandbox, and deepStrictEqual rejects cross-realm objects even
// when their structure is identical.
function collect(chunks) {
  const frames = [];
  const feed = createSSEParser(f => frames.push({ id: f.id, data: f.data }));
  for (const c of chunks) feed(c);
  return frames;
}

test("shortId trims prefixed ids to prefix + 6 hex", () => {
  assert.equal(shortId("sess_0123456789abcdef"), "sess_012345");
  assert.equal(shortId("deadbeefcafef00d"), "deadbeef");
  assert.equal(shortId(""), "");
  assert.equal(shortId(null), "");
});

test("prettyJSON formats objects, JSON strings, and passes through junk", () => {
  assert.equal(prettyJSON({ a: 1 }), '{\n  "a": 1\n}');
  assert.equal(prettyJSON('{"a":1}'), '{\n  "a": 1\n}');
  assert.equal(prettyJSON("not json"), "not json");
  assert.equal(prettyJSON(null), "");
  assert.equal(prettyJSON(undefined), "");
});

test("sortSessions orders newest created_at first without mutating input", () => {
  const input = [
    { id: "a", created_at: "2024-01-01T00:00:00Z" },
    { id: "b", created_at: "2024-03-01T00:00:00Z" },
    { id: "c", created_at: "2024-02-01T00:00:00Z" },
  ];
  const out = sortSessions(input);
  assert.deepEqual(out.map(s => s.id), ["b", "c", "a"]);
  assert.equal(input[0].id, "a"); // original order preserved
});

test("partsText joins text parts and ignores non-text / non-arrays", () => {
  assert.equal(
    partsText([
      { type: "text", text: "one" },
      { type: "image" },
      { type: "text", text: "two" },
      null,
    ]),
    "one\ntwo",
  );
  assert.equal(partsText("nope"), "");
  assert.equal(partsText([]), "");
});

test("maxSeq returns the largest numeric seq, else 0", () => {
  assert.equal(maxSeq([{ seq: 3 }, { seq: 9 }, { seq: 5 }]), 9);
  assert.equal(maxSeq([{ seq: 3 }, {}, { seq: "x" }]), 3);
  assert.equal(maxSeq([]), 0);
});

// plain rebuilds a cross-realm goal object into this realm's fields, so field
// assertions are reference-safe (same reason the collect helper rebuilds frames).
function plain(g) {
  return { condition: g.condition, active: g.active, achieved: g.achieved, reason: g.reason };
}

test("reduceGoal folds a full goal lifecycle from journal events", () => {
  let g = reduceGoal(null, { type: "goal.set", goal_condition: "ship it" });
  assert.deepEqual(plain(g), { condition: "ship it", active: true, achieved: false, reason: "" });

  g = reduceGoal(g, { type: "goal.eval", goal_met: false, goal_reason: "not yet" });
  assert.deepEqual(plain(g), { condition: "ship it", active: true, achieved: false, reason: "not yet" });

  g = reduceGoal(g, { type: "goal.achieved", goal_reason: "done now" });
  assert.deepEqual(plain(g), { condition: "ship it", active: false, achieved: true, reason: "done now" });
});

test("goalFromSession seeds chip state from the session record", () => {
  // Bootstrap gap: a goal.set predating connection is never replayed
  // (stream cursor starts at the global high-water seq), so selecting a
  // session must seed the chip from its JSON goal field.
  assert.equal(goalFromSession(null), null);
  assert.equal(goalFromSession({}), null);
  assert.equal(goalFromSession({ goal: { active: true } }), null); // no condition
  assert.deepEqual(
    plain(goalFromSession({ goal: { condition: "ship", active: true, last_reason: "not yet" } })),
    { condition: "ship", active: true, achieved: false, reason: "not yet" },
  );
  assert.deepEqual(
    plain(goalFromSession({ goal: { condition: "ship", active: false, achieved: true, last_reason: "done" } })),
    { condition: "ship", active: false, achieved: true, reason: "done" },
  );
});

test("reduceGoal marks a cleared goal inactive without achievement", () => {
  let g = reduceGoal(null, { type: "goal.set", goal_condition: "x" });
  g = reduceGoal(g, { type: "goal.cleared" });
  assert.equal(g.active, false);
  assert.equal(g.achieved, false);
  assert.equal(g.condition, "x");
});

test("reduceGoal ignores non-goal events and does not mutate prior state", () => {
  const prev = { condition: "x", active: true, achieved: false, reason: "" };
  const out = reduceGoal(prev, { type: "message" });
  assert.equal(out, prev); // returned unchanged (same reference)
  // A goal event returns a fresh object, leaving the input untouched.
  const next = reduceGoal(prev, { type: "goal.eval", goal_reason: "r" });
  assert.notEqual(next, prev);
  assert.equal(prev.reason, ""); // input not mutated
  assert.equal(next.reason, "r");
});

test("SSE parser: single frame with data", () => {
  const f = collect(["data: hello\n\n"]);
  assert.deepEqual(f, [{ id: null, data: "hello" }]);
});

test("SSE parser: multi-line data joined with \\n", () => {
  const f = collect(["data: a\ndata: b\ndata: c\n\n"]);
  assert.deepEqual(f, [{ id: null, data: "a\nb\nc" }]);
});

test("SSE parser: id line is captured", () => {
  const f = collect(["id: 42\ndata: x\n\n"]);
  assert.deepEqual(f, [{ id: "42", data: "x" }]);
});

test("SSE parser: comment / heartbeat lines are ignored", () => {
  const f = collect([": keep-alive\ndata: x\n\n"]);
  assert.deepEqual(f, [{ id: null, data: "x" }]);
});

test("SSE parser: only a comment dispatches nothing", () => {
  assert.deepEqual(collect([": ping\n\n"]), []);
});

test("SSE parser: CRLF line endings are handled", () => {
  const f = collect(["id: 7\r\ndata: hi\r\n\r\n"]);
  assert.deepEqual(f, [{ id: "7", data: "hi" }]);
});

test("SSE parser: only one leading space is stripped from a value", () => {
  const f = collect(["data:  two-spaces\n\n"]);
  assert.deepEqual(f, [{ id: null, data: " two-spaces" }]);
});

test("SSE parser: colons inside JSON values survive", () => {
  const payload = '{"url":"http://x:8080","ratio":"3:2:1"}';
  const f = collect(["data: " + payload + "\n\n"]);
  assert.equal(f.length, 1);
  assert.equal(f[0].data, payload);
  assert.deepEqual(JSON.parse(f[0].data), {
    url: "http://x:8080",
    ratio: "3:2:1",
  });
});

test("SSE parser: two frames in one chunk", () => {
  const f = collect(["data: one\n\ndata: two\n\n"]);
  assert.deepEqual(f, [
    { id: null, data: "one" },
    { id: null, data: "two" },
  ]);
});

test("SSE parser: chunk boundary splits mid-line", () => {
  const f = collect(["data: hel", "lo\n\n"]);
  assert.deepEqual(f, [{ id: null, data: "hello" }]);
});

test("SSE parser: chunk boundary splits mid-frame (before blank line)", () => {
  const f = collect(["data: hello\n", "\n"]);
  assert.deepEqual(f, [{ id: null, data: "hello" }]);
});

test("SSE parser: chunk boundary splits between two frames", () => {
  const f = collect(["data: one\n\nda", "ta: two\n\n"]);
  assert.deepEqual(f, [
    { id: null, data: "one" },
    { id: null, data: "two" },
  ]);
});

test("SSE parser: single byte-at-a-time feed reassembles frame", () => {
  const whole = "id: 5\ndata: ab\n\n";
  const f = collect([...whole]); // one character per chunk
  assert.deepEqual(f, [{ id: "5", data: "ab" }]);
});

// --- WHATWG SSE spec: the last-event-ID buffer persists across dispatched
// frames. It is updated only by a new id: line, never cleared when a frame
// omits id:. (Finding 2 — these were the failing, then fixed, assertions.)

test("SSE parser: id persists to a subsequent id-less frame", () => {
  const f = collect(["id: 100\ndata: first\n\n", "data: second\n\n"]);
  assert.deepEqual(f, [
    { id: "100", data: "first" },
    { id: "100", data: "second" }, // id carried over per spec
  ]);
});

test("SSE parser: a new id line replaces the persisted id", () => {
  const f = collect([
    "id: 100\ndata: first\n\n",
    "data: second\n\n",
    "id: 200\ndata: third\n\n",
    "data: fourth\n\n",
  ]);
  assert.deepEqual(f, [
    { id: "100", data: "first" },
    { id: "100", data: "second" },
    { id: "200", data: "third" },
    { id: "200", data: "fourth" },
  ]);
});
