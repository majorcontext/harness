// Unit tests for the pure helpers in tools/monitor/index.html.
//
// The monitor is a single self-contained HTML file with no build step, so
// there is nothing to import. Instead we read index.html, extract the region
// between the /* TESTABLE-BEGIN */ and /* TESTABLE-END */ markers, and
// evaluate it in a node:vm sandbox exposing only Date and JSON. This keeps
// the page build-free while making its parser + helpers reproducibly
// testable. Copied from tools/inspector/inspector_test.mjs's extraction
// preamble, adjusted to this file's path.
//
// Run: node --test tools/monitor/

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

// Function declarations (and `var`-declared bindings, unlike `const`/`let`,
// which live in the vm context's lexical environment rather than as
// properties of it) at the top level of a vm script become properties of the
// sandbox's global object; read them straight off the context.
const sandbox = { Date, JSON };
vm.createContext(sandbox);
vm.runInContext(source, sandbox);
const {
  createSSEParser,
  maxSeq,
  partsText,
  summarizeArgs,
  toolLabel,
  fmtElapsed,
  fmtAgo,
  hostLabel,
  route,
  unroute,
  QUIET_MS,
  STALL_MS,
  staleness,
  reduceActivity,
  seedActivity,
  boardModel,
  transcriptModel,
  HISTORY_WINDOW,
  historyWindow,
  adaptHistory,
  countKind,
  liveMessageCount,
  entryKey,
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

// reify deep-clones a value that crossed the vm sandbox boundary back into
// this realm's plain Object/Array graph. node:assert's strict deepEqual
// treats same-shaped values from different realms as unequal (their
// [[Prototype]]s differ) — the same cross-realm gotcha `collect` above works
// around for SSE frames. JSON-safe here: every helper under test returns
// plain data (strings/numbers/booleans/null/arrays/objects), so a
// stringify/parse round trip reconstructs it natively in this realm.
function reify(v) {
  return v === undefined ? v : JSON.parse(JSON.stringify(v));
}

/* ---------- createSSEParser (copied base cases from inspector) ---------- */

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

test("SSE parser: heartbeat comment interleaved with real frames", () => {
  const f = collect(["data: one\n\n: keep-alive\n\ndata: two\n\n"]);
  assert.deepEqual(f, [
    { id: null, data: "one" },
    { id: null, data: "two" },
  ]);
});

test("SSE parser: CRLF line endings are handled", () => {
  const f = collect(["id: 7\r\ndata: hi\r\n\r\n"]);
  assert.deepEqual(f, [{ id: "7", data: "hi" }]);
});

test("SSE parser: colons inside JSON values survive", () => {
  const payload = '{"type":"tool.start","url":"http://x:8080"}';
  const f = collect(["data: " + payload + "\n\n"]);
  assert.equal(f.length, 1);
  assert.deepEqual(JSON.parse(f[0].data), { type: "tool.start", url: "http://x:8080" });
});

test("SSE parser: chunk boundary splits mid-frame", () => {
  const f = collect(["data: hel", "lo\n\n"]);
  assert.deepEqual(f, [{ id: null, data: "hello" }]);
});

test("SSE parser: id persists to a subsequent id-less frame", () => {
  const f = collect(["id: 100\ndata: first\n\n", "data: second\n\n"]);
  assert.deepEqual(f, [
    { id: "100", data: "first" },
    { id: "100", data: "second" },
  ]);
});

/* ---------- maxSeq ---------- */

test("maxSeq returns the largest numeric seq, else 0", () => {
  assert.equal(maxSeq([{ seq: 3 }, { seq: 9 }, { seq: 5 }]), 9);
  assert.equal(maxSeq([{ seq: 3 }, {}, { seq: "x" }]), 3);
  assert.equal(maxSeq([]), 0);
});

/* ---------- partsText / summarizeArgs / toolLabel ---------- */

test("partsText joins text parts and ignores non-text / non-arrays", () => {
  assert.equal(
    partsText([{ type: "text", text: "one" }, { type: "image" }, { type: "text", text: "two" }, null]),
    "one\ntwo",
  );
  assert.equal(partsText("nope"), "");
  assert.equal(partsText([]), "");
});

test("summarizeArgs extracts a recognizable field from object or JSON-string arguments", () => {
  assert.equal(summarizeArgs({ command: "go test ./server/ -race" }), "go test ./server/ -race");
  assert.equal(summarizeArgs('{"command":"gofmt -l ."}'), "gofmt -l .");
  assert.equal(summarizeArgs({ pattern: "TODO" }), "TODO");
  assert.equal(summarizeArgs({ nothingRecognizable: 1 }), "");
  assert.equal(summarizeArgs("not json"), "");
  assert.equal(summarizeArgs(null), "");
  assert.equal(summarizeArgs([1, 2]), "");
});

test("toolLabel joins name and summary, or falls back to name / generic tool", () => {
  assert.equal(toolLabel("Bash", "go test ./..."), "Bash · go test ./...");
  assert.equal(toolLabel("Bash", ""), "Bash");
  assert.equal(toolLabel("", ""), "tool");
});

/* ---------- fmtElapsed / fmtAgo ---------- */

test("fmtElapsed formats seconds/minutes/hours like the mockup ticker", () => {
  assert.equal(fmtElapsed(0), "0s");
  assert.equal(fmtElapsed(41), "41s");
  assert.equal(fmtElapsed(59), "59s");
  assert.equal(fmtElapsed(60), "1m 00s");
  assert.equal(fmtElapsed(154), "2m 34s");
  assert.equal(fmtElapsed(3600), "1h 0m");
  assert.equal(fmtElapsed(8047), "2h 14m");
  assert.equal(fmtElapsed(null), "—");
});

test("fmtAgo renders a coarse ago string", () => {
  assert.equal(fmtAgo(5000), "5s ago");
  assert.equal(fmtAgo(12 * 60 * 1000), "12m ago");
  assert.equal(fmtAgo((60 * 60 + 3 * 60) * 1000), "1h 3m ago");
  assert.equal(fmtAgo(2 * 60 * 60 * 1000), "2h ago");
  assert.equal(fmtAgo(null), "—");
});

/* ---------- hostLabel ---------- */

test("hostLabel strips the scheme and cuts at the first path/query/fragment", () => {
  assert.equal(hostLabel("http://localhost:4096"), "localhost:4096");
  assert.equal(hostLabel("https://box.example.com/"), "box.example.com");
  assert.equal(hostLabel("http://127.0.0.1:4096/session?x=1#y"), "127.0.0.1:4096");
});

test("hostLabel tolerates missing scheme, and empty/missing input", () => {
  assert.equal(hostLabel("localhost:4096"), "localhost:4096");
  assert.equal(hostLabel(""), "");
  assert.equal(hostLabel(null), "");
  assert.equal(hostLabel(undefined), "");
});

/* ---------- route / unroute ---------- */

test("route/unroute round-trip base + session id", () => {
  const state = { base: "http://localhost:4096", sessionId: "ses_01ky9fjq2wexvq8rn0m4tdq2m" };
  const hash = route(state);
  assert.equal(hash, "#b=http%3A%2F%2Flocalhost%3A4096&s=ses_01ky9fjq2wexvq8rn0m4tdq2m");
  assert.deepEqual(reify(unroute(hash)), state);
});

test("route omits absent fields; unroute of '#' yields nulls", () => {
  assert.equal(route({}), "#");
  assert.deepEqual(reify(unroute("#")), { base: null, sessionId: null });
  assert.deepEqual(reify(unroute("")), { base: null, sessionId: null });
  assert.equal(route({ base: "http://x" }), "#b=http%3A%2F%2Fx");
});

test("unroute tolerates junk hashes without throwing", () => {
  assert.deepEqual(reify(unroute(undefined)), { base: null, sessionId: null });
  assert.deepEqual(reify(unroute(null)), { base: null, sessionId: null });
  assert.deepEqual(reify(unroute("not-a-hash-at-all")), { base: null, sessionId: null });
  assert.deepEqual(reify(unroute("#&&&")), { base: null, sessionId: null });
  assert.deepEqual(reify(unroute("#x=1&y")), { base: null, sessionId: null });
  // Malformed percent-encoding must not throw; it degrades to the raw text.
  assert.deepEqual(reify(unroute("#b=%zz&s=ok")), { base: "%zz", sessionId: "ok" });
});

/* ---------- staleness / QUIET_MS / STALL_MS ---------- */

test("QUIET_MS and STALL_MS are exported with the documented values", () => {
  assert.equal(QUIET_MS, 15000);
  assert.equal(STALL_MS, 60000);
});

test("staleness: idle phase is never quiet or stalled, regardless of silence", () => {
  const idleActivity = { phase: "idle", tool: null, turn: null, lastEventAt: 0, lastOutcome: null };
  assert.equal(staleness(idleActivity, 10_000_000), "idle");
  assert.equal(staleness(null, 1000), "idle");
});

test("staleness: boundaries are pinned exactly at 15000ms and 60000ms", () => {
  const base = { phase: "tool", tool: { name: "Bash", sinceAt: 0 }, turn: { startedAt: 0, toolCalls: 1 }, lastEventAt: 0, lastOutcome: null };
  assert.equal(staleness(base, 14_999), "live");
  assert.equal(staleness(base, 15_000), "quiet");   // exactly QUIET_MS: quiet, not live
  assert.equal(staleness(base, 59_999), "quiet");
  assert.equal(staleness(base, 60_000), "stalled"); // exactly STALL_MS: stalled, not quiet
  assert.equal(staleness(base, 60_001), "stalled");
});

/* ---------- reduceActivity (RED-FIRST: written before the implementation) ---------- */

test("reduceActivity: null prev is safe and starts from an idle base", () => {
  const a = reduceActivity(null, { type: "session.status", status: "busy" }, 1000);
  assert.equal(a.phase, "between");
  assert.deepEqual(reify(a.turn), { startedAt: 1000, toolCalls: 0 });
  assert.equal(a.tool, null);
  assert.equal(a.lastEventAt, 1000);
});

test("reduceActivity: an unrecognized event bumps lastEventAt and otherwise passes prev through", () => {
  const prev = { phase: "streaming", tool: null, turn: { startedAt: 0, toolCalls: 0 }, lastEventAt: 0, lastOutcome: null };
  const a = reduceActivity(prev, { type: "goal.eval" }, 500);
  assert.equal(a.phase, "streaming");
  assert.equal(a.lastEventAt, 500);
});

test("reduceActivity: text.delta and reasoning.delta set phase streaming and open a turn if none is open", () => {
  let a = reduceActivity(null, { type: "text.delta", text: "hi" }, 100);
  assert.equal(a.phase, "streaming");
  assert.deepEqual(reify(a.turn), { startedAt: 100, toolCalls: 0 });
  a = reduceActivity(a, { type: "reasoning.delta", text: "thinking" }, 150);
  assert.equal(a.phase, "streaming");
  assert.deepEqual(reify(a.turn), { startedAt: 100, toolCalls: 0 }); // unchanged, not re-opened
});

test("reduceActivity: tool.start sets phase tool, records name/argsSummary/sinceAt, increments toolCalls", () => {
  const busy = reduceActivity(null, { type: "session.status", status: "busy" }, 0);
  const a = reduceActivity(busy, {
    type: "tool.start",
    tool_call: { call_id: "call_1", name: "Bash", arguments: { command: "go test ./server/ -race" } },
  }, 41_000);
  assert.equal(a.phase, "tool");
  assert.deepEqual(reify(a.tool), { name: "Bash", argsSummary: "go test ./server/ -race", sinceAt: 41_000 });
  assert.equal(a.turn.toolCalls, 1);
  assert.equal(a.turn.startedAt, 0); // the turn's own start is untouched by the tool starting
});

test("reduceActivity: tool.end closes the tool and returns to the between phase", () => {
  const busy = reduceActivity(null, { type: "session.status", status: "busy" }, 0);
  const withTool = reduceActivity(busy, { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: {} } }, 10);
  const after = reduceActivity(withTool, { type: "tool.end", tool_call: { call_id: "c1" }, output: [], is_error: false }, 20);
  assert.equal(after.phase, "between");
  assert.equal(after.tool, null);
});

test("reduceActivity: tool.end without a matching start is ignored, not a crash", () => {
  const busy = reduceActivity(null, { type: "session.status", status: "busy" }, 0);
  assert.equal(busy.tool, null);
  const after = reduceActivity(busy, { type: "tool.end", tool_call: { call_id: "never-started" }, output: [] }, 10);
  assert.equal(after.phase, "between"); // unchanged from prev, not incorrectly flipped
  assert.equal(after.tool, null);
  assert.equal(after.lastEventAt, 10); // still counts as activity
});

test("reduceActivity: session.status idle closes the turn and clears the current tool", () => {
  const busy = reduceActivity(null, { type: "session.status", status: "busy" }, 0);
  const withTool = reduceActivity(busy, { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: {} } }, 10);
  const idle = reduceActivity(withTool, { type: "session.status", status: "idle" }, 20);
  assert.equal(idle.phase, "idle");
  assert.equal(idle.tool, null);
  assert.equal(idle.turn, null);
});

test("reduceActivity: turn.end records lastOutcome without altering phase/turn", () => {
  const busy = reduceActivity(null, { type: "session.status", status: "busy" }, 0);
  const done = reduceActivity(busy, { type: "turn.end", outcome: "completed" }, 30);
  assert.equal(done.lastOutcome, "completed");
  assert.equal(done.phase, "between");
});

test("reduceActivity: mid-turn seed (seedActivity) then live events accumulate correctly", () => {
  // A monitor connecting mid-turn only has poll data: state busy, no start
  // time. seedActivity opens a turn with a null startedAt (never fabricated),
  // and subsequent live events must not invent one either.
  const seeded = seedActivity({ id: "s1", state: "busy", last_activity_at: new Date(500).toISOString(), last_turn: null });
  assert.equal(seeded.phase, "between");
  assert.deepEqual(reify(seeded.turn), { startedAt: null, toolCalls: 0 });
  assert.equal(seeded.lastEventAt, 500);

  const withTool = reduceActivity(seeded, {
    type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "ls" } },
  }, 900);
  assert.equal(withTool.phase, "tool");
  assert.equal(withTool.turn.startedAt, null); // still unknown — never fabricated
  assert.equal(withTool.turn.toolCalls, 1);
  assert.deepEqual(reify(withTool.tool), { name: "Bash", argsSummary: "ls", sinceAt: 900 });
});

test("seedActivity: idle session seeds an idle activity carrying the last outcome", () => {
  const seeded = seedActivity({
    id: "s1", state: "idle",
    last_activity_at: new Date(12345).toISOString(),
    last_turn: { outcome: "completed" },
  });
  assert.equal(seeded.phase, "idle");
  assert.equal(seeded.turn, null);
  assert.equal(seeded.tool, null);
  assert.equal(seeded.lastEventAt, 12345);
  assert.equal(seeded.lastOutcome, "completed");
});

test("seedActivity: goal-running state seeds a running (between) activity", () => {
  const seeded = seedActivity({ id: "s1", state: "goal-running", last_activity_at: new Date(0).toISOString() });
  assert.equal(seeded.phase, "between");
  assert.deepEqual(reify(seeded.turn), { startedAt: null, toolCalls: 0 });
});

test("seedActivity: missing/invalid last_activity_at never fabricates a lastEventAt", () => {
  assert.equal(seedActivity({ state: "idle" }).lastEventAt, null);
  assert.equal(seedActivity({ state: "idle", last_activity_at: "not-a-date" }).lastEventAt, null);
  assert.equal(seedActivity(null).phase, "idle");
});

/* ---------- boardModel ---------- */

function activityMap(entries) {
  return new Map(Object.entries(entries));
}

test("boardModel: queue count is suppressed at 0 and shown when > 0", () => {
  const sessions = [
    { id: "a", state: "idle", queued: 0, last_activity_at: new Date(0).toISOString() },
    { id: "b", state: "idle", queued: 3, last_activity_at: new Date(0).toISOString() },
  ];
  const rows = boardModel(sessions, activityMap({}), 0);
  const byId = Object.fromEntries(rows.map(r => [r.id, r]));
  assert.equal(byId.a.extra, null);
  assert.equal(byId.b.extra, "3 queued");
});

test("boardModel: sorts active sessions before idle, and is stable within equal recency", () => {
  const now = 100_000;
  const sessions = [
    { id: "idle-1", state: "idle", queued: 0, last_activity_at: new Date(now - 1000).toISOString() },
    { id: "live-1", state: "busy", queued: 0, last_activity_at: new Date(now).toISOString() },
    { id: "idle-2", state: "idle", queued: 0, last_activity_at: new Date(now - 500).toISOString() },
    { id: "live-2", state: "busy", queued: 0, last_activity_at: new Date(now).toISOString() },
  ];
  const acts = activityMap({
    "live-1": reduceActivity(null, { type: "text.delta", text: "x" }, now),
    "live-2": reduceActivity(null, { type: "text.delta", text: "x" }, now),
    "idle-1": seedActivity(sessions[0]),
    "idle-2": seedActivity(sessions[2]),
  });
  const rows = boardModel(sessions, acts, now);
  const order = rows.map(r => r.id);
  assert.deepEqual(reify(order), ["live-1", "live-2", "idle-2", "idle-1"]);
});

test("boardModel: detail precedence — an active tool wins over stall phase and idle outcome", () => {
  const now = 100_000;
  const session = { id: "s1", state: "busy", queued: 0, goal: null, last_activity_at: new Date(now).toISOString() };
  let a = reduceActivity(null, { type: "session.status", status: "busy" }, now - 70_000);
  a = reduceActivity(a, { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "sync_dir" } } }, now - 70_000);
  const rows = boardModel([session], activityMap({ s1: a }), now); // 70s silent -> stalled
  assert.equal(rows[0].cssClass, "bad");
  assert.equal(rows[0].detail, "Bash · sync_dir");
});

test("boardModel: idle rows show the last turn outcome, or 'worker parked' when the goal is paused on worker failure", () => {
  const now = 100_000;
  const completed = { id: "s1", state: "idle", queued: 0, last_turn: { outcome: "completed" }, last_activity_at: new Date(now).toISOString() };
  const parked = {
    id: "s2", state: "idle", queued: 0,
    goal: { condition: "ship it", active: true, paused: true, pause_reason: "worker_failure" },
    last_activity_at: new Date(now).toISOString(),
  };
  const rows = boardModel([completed, parked], activityMap({ s1: seedActivity(completed), s2: seedActivity(parked) }), now);
  const byId = Object.fromEntries(rows.map(r => [r.id, r]));
  assert.equal(byId.s1.detail, "completed");
  assert.equal(byId.s1.detailCritical, false);
  assert.equal(byId.s2.detail, "worker parked");
  assert.equal(byId.s2.detailCritical, true);
});

/* ---------- transcriptModel (RED-FIRST: written before the implementation) ---------- */

test("transcriptModel: durable messages fold into turn-mark + operator/assistant entries", () => {
  const messages = [
    { id: "m1", role: "user", created_at: "2024-01-01T00:00:00Z", parts: [{ type: "text", text: "run the tests" }] },
    { id: "m2", role: "assistant", created_at: "2024-01-01T00:00:01Z", parts: [{ type: "text", text: "on it" }] },
  ];
  const entries = transcriptModel(messages, []);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "assistant"]);
  assert.equal(entries[0].turn, 1);
  assert.equal(entries[1].text, "run the tests");
  assert.equal(entries[2].text, "on it");
});

test("transcriptModel: a durable tool_call pairs with its tool_result from the following tool-role message", () => {
  const messages = [
    { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "tool_call", call_id: "c1", name: "Bash", arguments: { command: "gofmt -l ." } }] },
    { id: "m2", role: "tool", created_at: "t", parts: [{ type: "tool_result", call_id: "c1", content: [{ type: "text", text: "clean" }], is_error: false }] },
  ];
  const entries = transcriptModel(messages, []);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].kind, "tool");
  assert.equal(entries[0].name, "Bash");
  assert.equal(entries[0].argsSummary, "gofmt -l .");
  assert.equal(entries[0].output, "clean");
  assert.equal(entries[0].running, false);
  assert.equal(entries[0].isError, false);
});

test("transcriptModel: a durable tool_call with no matching result renders as a running fold", () => {
  const messages = [
    { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "tool_call", call_id: "c1", name: "Bash", arguments: {} }] },
  ];
  const entries = transcriptModel(messages, []);
  assert.equal(entries[0].running, true);
});

test("transcriptModel: tool.end without a matching tool.start is ignored, not a crash", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "tool.end", tool_call: { call_id: "never-started" }, output: [{ type: "text", text: "??" }] },
    { type: "text.delta", text: "still fine" },
  ];
  const entries = transcriptModel([], evs);
  const kinds = entries.map(e => e.kind);
  assert.deepEqual(reify(kinds), ["turn-mark", "assistant"]);
  assert.equal(entries[1].text, "still fine");
});

test("transcriptModel: deltas that arrive while a tool fold is open start a new trailing text entry, not lost", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "ls" } } },
    { type: "text.delta", text: "still running, here's a note" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "tool", "assistant"]);
  assert.equal(entries[1].running, true);
  assert.equal(entries[2].text, "still running, here's a note");
});

test("transcriptModel: two sequential tools in one turn produce two distinct, correctly paired folds", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "one" } } },
    { type: "tool.end", tool_call: { call_id: "c1" }, output: [{ type: "text", text: "out-one" }], is_error: false },
    { type: "tool.start", tool_call: { call_id: "c2", name: "Bash", arguments: { command: "two" } } },
    { type: "tool.end", tool_call: { call_id: "c2" }, output: [{ type: "text", text: "out-two" }], is_error: false },
  ];
  const entries = transcriptModel([], evs);
  const tools = entries.filter(e => e.kind === "tool");
  assert.equal(tools.length, 2);
  assert.equal(tools[0].output, "out-one");
  assert.equal(tools[0].running, false);
  assert.equal(tools[1].output, "out-two");
  assert.equal(tools[1].running, false);
});

test("transcriptModel: turn markers appear across busy -> idle -> busy transitions", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "text.delta", text: "first turn" },
    { type: "session.status", status: "idle" },
    { type: "session.status", status: "busy" },
    { type: "text.delta", text: "second turn" },
  ];
  const entries = transcriptModel([], evs);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 2);
  assert.equal(marks[0].turn, 1);
  assert.equal(marks[1].turn, 2);
  // the idle transition discards the first turn's streaming draft: only the
  // second turn's text survives as an open (undurable) entry.
  const streamingTexts = entries.filter(e => e.kind === "assistant" && e.streaming);
  assert.deepEqual(reify(streamingTexts.map(e => e.text)), ["second turn"]);
});

test("transcriptModel: single-source turn marks — 'session.status busy' then a 'message' event for the SAME turn's user prompt marks once, not twice", () => {
  // The real wire order for both an idle-dispatched prompt and a
  // queued-prompt delivery: session.status(busy) opens the turn, THEN a
  // "message" event durably delivers the user's own prompt text (see
  // server/handlers.go's handlePrompt/dispatchQueueHead: emitDurable(busy)
  // happens before the Prompt() call that appends + publishes the user
  // message). Regression for the double turn-mark bug: session.status's own
  // case used to unconditionally push a mark AND foldMessage's user-role
  // branch pushed its own, independently, for the very same turn.
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "go" }] } },
    { type: "text.delta", text: "on it" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "assistant"]);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 1, "exactly one turn-mark for one turn, regardless of event count");
  assert.equal(marks[0].turn, 1);
  assert.equal(entries[1].text, "go");
});

test("transcriptModel: single-source turn marks — a queued-prompt delivery (dequeue -> busy -> message) also marks once", () => {
  // Traced against server/handlers.go's dispatchQueueHead: DequeuePrompt
  // publishes a durable prompt.dequeued record (transcriptModel has no
  // dedicated case for it — falls through to the event-loop's default, a
  // no-op), THEN emitDurable(session.status busy), THEN runPrompt's
  // st.sess.Prompt call appends and publishes the user message — the exact
  // same busy-then-message shape as an idle dispatch, just preceded by the
  // dequeue record.
  const evs = [
    { type: "prompt.dequeued", queue_text: "also run lint", queue_reason: "delivered" },
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "also run lint" }] } },
  ];
  const entries = transcriptModel([], evs);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 1, "a queued-delivery turn must mark exactly once");
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator"]);
});

test("transcriptModel: single-source turn marks — two consecutive live turns (busy/message pairs) each mark exactly once, numbered in order", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "first" }] } },
    { type: "message", message: { id: "m2", role: "assistant", created_at: "t1", parts: [{ type: "text", text: "done" }] } },
    { type: "session.status", status: "idle" },
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m3", role: "user", created_at: "t2", parts: [{ type: "text", text: "second" }] } },
  ];
  const entries = transcriptModel([], evs);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 2);
  assert.equal(marks[0].turn, 1);
  assert.equal(marks[1].turn, 2);
});

test("transcriptModel: single-source turn marks — the durable-history fold (no session.status events at all) is unaffected: every user message still marks", () => {
  const messages = [
    { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "first" }] },
    { id: "m2", role: "assistant", created_at: "t1", parts: [{ type: "text", text: "ok" }] },
    { id: "m3", role: "user", created_at: "t2", parts: [{ type: "text", text: "second" }] },
    { id: "m4", role: "assistant", created_at: "t3", parts: [{ type: "text", text: "ok again" }] },
  ];
  const entries = transcriptModel(messages, []);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 2);
  assert.equal(marks[0].turn, 1);
  assert.equal(marks[1].turn, 2);
});

test("transcriptModel: single-source turn marks — a user 'message' with no preceding live session.status (e.g. connecting mid-stream) still marks, order-tolerant", () => {
  const evs = [
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "go" }] } },
  ];
  const entries = transcriptModel([], evs);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 1);
  assert.equal(marks[0].turn, 1);
});

test("transcriptModel: single-source turn marks — session.status busy with no following user message (e.g. a goal-internal cycle) does not leak into a later, unrelated user message", () => {
  const evs = [
    { type: "session.status", status: "busy" }, // e.g. a goal-loop internal turn with no discrete user message
    { type: "text.delta", text: "internal step" },
    { type: "session.status", status: "idle" },
    // A later, genuinely separate user message arrives with no immediately
    // preceding busy in THIS buffer — it must still mark its own turn, not
    // be silently swallowed by a stale "turn already open" flag left over
    // from the earlier busy/idle cycle.
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "go" }] } },
  ];
  const entries = transcriptModel([], evs);
  const marks = entries.filter(e => e.kind === "turn-mark");
  assert.equal(marks.length, 2, "the goal-internal busy/idle cycle marks once, the later user message marks its own turn");
});

test("transcriptModel: a live 'message' event supersedes the streaming draft it completes, without duplication", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "text.delta", text: "partial" },
    { type: "text.delta", text: " reply" },
    { type: "message", message: { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "text", text: "partial reply" }] } },
  ];
  const entries = transcriptModel([], evs);
  const assistantTexts = entries.filter(e => e.kind === "assistant");
  assert.equal(assistantTexts.length, 1);
  assert.equal(assistantTexts[0].text, "partial reply");
  assert.equal(assistantTexts[0].streaming, undefined);
});

test("transcriptModel: an operator message arrives live as an operator entry the moment its 'message' event lands", () => {
  const evs = [
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator"]);
  assert.equal(entries[1].text, "go");
});

test("transcriptModel: a live tool_result ('message' event) pairs with a durable tool_call even though it lands after", () => {
  const messages = [
    { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "tool_call", call_id: "c1", name: "Bash", arguments: {} }] },
  ];
  const evs = [
    { type: "message", message: { id: "m2", role: "tool", created_at: "t", parts: [{ type: "tool_result", call_id: "c1", content: [{ type: "text", text: "done" }], is_error: false }] } },
  ];
  const entries = transcriptModel(messages, evs);
  const tool = entries.find(e => e.kind === "tool");
  assert.equal(tool.running, false);
  assert.equal(tool.output, "done");
});

test("transcriptModel: a live tool.end resolves a durable running tool_call fetched from history (no matching live tool.start)", () => {
  // The tool started before this page ever fetched history, so the running
  // fold comes from `messages`, not a live tool.start — tool.end is never
  // itself durable (see server/journal.go's Publish), so it must still be
  // able to patch this entry in place.
  const messages = [
    { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "tool_call", call_id: "c1", name: "Bash", arguments: { command: "go test ./server/ -race" } }] },
  ];
  const evs = [
    { type: "tool.end", tool_call: { call_id: "c1" }, output: [{ type: "text", text: "PASS" }], is_error: false },
  ];
  const entries = transcriptModel(messages, evs);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].kind, "tool");
  assert.equal(entries[0].running, false);
  assert.equal(entries[0].output, "PASS");
});

test("transcriptModel: turnOffset shifts turn numbering for a windowed (non-full) message list", () => {
  const messages = [
    { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] },
  ];
  const entries = transcriptModel(messages, [], 14);
  const mark = entries.find(e => e.kind === "turn-mark");
  assert.equal(mark.turn, 15);
});

test("transcriptModel: prompt.queued renders an optimistic operator entry with the queued text", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "prompt.queued", queue_text: "also check the linter", queue_id: 7 },
  ];
  const entries = transcriptModel([], evs);
  const queued = entries.find(e => e.kind === "operator" && e.queued);
  assert.ok(queued, "expected a queued operator entry");
  assert.equal(queued.text, "also check the linter");
  assert.equal(queued.queueId, 7);
});

test("transcriptModel: a delivered 'message' event supersedes its own prompt.queued placeholder without duplicating it", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "prompt.queued", queue_text: "also check the linter", queue_id: 7 },
    { type: "message", message: { id: "m9", role: "user", created_at: "t", parts: [{ type: "text", text: "also check the linter" }] } },
  ];
  const entries = transcriptModel([], evs);
  const operatorEntries = entries.filter(e => e.kind === "operator");
  assert.equal(operatorEntries.length, 1, "the queued placeholder must be replaced, not duplicated");
  assert.equal(operatorEntries[0].queued, undefined);
  assert.equal(operatorEntries[0].text, "also check the linter");
});

test("transcriptModel: a queued placeholder not yet delivered survives an unrelated draft reset", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "text.delta", text: "working on it" },
    { type: "prompt.queued", queue_text: "and also this", queue_id: 3 },
    // The current turn's own assistant reply completes (a normal "message"
    // event for the ASSISTANT, not the queued operator prompt) — this must
    // not discard the still-undelivered queued placeholder.
    { type: "message", message: { id: "m1", role: "assistant", created_at: "t", parts: [{ type: "text", text: "working on it" }] } },
  ];
  const entries = transcriptModel([], evs);
  const queued = entries.find(e => e.kind === "operator" && e.queued);
  assert.ok(queued, "the queued placeholder must survive an unrelated message event");
  assert.equal(queued.text, "and also this");
});

/* ---------- historyWindow ---------- */

test("HISTORY_WINDOW is exported with the documented value", () => {
  assert.equal(HISTORY_WINDOW, 50);
});

test("historyWindow returns everything, no hidden count, when the list is at or under the limit", () => {
  const messages = [{ role: "user" }, { role: "assistant" }];
  assert.deepEqual(reify(historyWindow(messages, 2)), { window: messages, hiddenCount: 0, hiddenTurns: 0 });
  assert.deepEqual(reify(historyWindow(messages, 50)), { window: messages, hiddenCount: 0, hiddenTurns: 0 });
});

test("historyWindow trims to the most recent `limit` messages and counts hidden turns", () => {
  const messages = [
    { role: "user", id: "1" }, { role: "assistant", id: "2" },
    { role: "user", id: "3" }, { role: "assistant", id: "4" },
    { role: "user", id: "5" }, { role: "assistant", id: "6" },
  ];
  const w = historyWindow(messages, 2);
  assert.deepEqual(reify(w.window), [{ role: "user", id: "5" }, { role: "assistant", id: "6" }]);
  assert.equal(w.hiddenCount, 4);
  assert.equal(w.hiddenTurns, 2); // two user messages in the dropped prefix
});

test("historyWindow treats a null/undefined/Infinity limit as unbounded", () => {
  const messages = [{ role: "user" }, { role: "assistant" }, { role: "user" }];
  assert.equal(historyWindow(messages, null).window.length, 3);
  assert.equal(historyWindow(messages, undefined).window.length, 3);
  assert.equal(historyWindow(messages, Infinity).window.length, 3);
});

test("historyWindow tolerates a non-array input", () => {
  assert.deepEqual(reify(historyWindow(null, 5)), { window: [], hiddenCount: 0, hiddenTurns: 0 });
});

/* ---------- adaptHistory ---------- */

test("adaptHistory keeps real messages and drops unmarshalable placeholders, counting them", () => {
  const raw = [
    { id: "m1", role: "user", parts: [{ type: "text", text: "hi" }] },
    { id: "m2", role: "assistant", marshal_error: "json: error calling MarshalJSON" }, // messagePlaceholder: no `parts`
    { id: "m3", role: "assistant", parts: [{ type: "text", text: "ok" }] },
  ];
  const adapted = adaptHistory(raw);
  assert.equal(adapted.messages.length, 2);
  assert.deepEqual(reify(adapted.messages.map(m => m.id)), ["m1", "m3"]);
  assert.equal(adapted.unavailable, 1);
});

test("adaptHistory tolerates a non-array/empty response", () => {
  assert.deepEqual(reify(adaptHistory(null)), { messages: [], unavailable: 0 });
  assert.deepEqual(reify(adaptHistory([])), { messages: [], unavailable: 0 });
});

/* ---------- countKind / liveMessageCount ---------- */

test("countKind counts entries by kind", () => {
  const entries = [{ kind: "tool" }, { kind: "assistant" }, { kind: "tool" }];
  assert.equal(countKind(entries, "tool"), 2);
  assert.equal(countKind(entries, "operator"), 0);
  assert.equal(countKind(null, "tool"), 0);
});

test("liveMessageCount counts only 'message' events", () => {
  const evs = [{ type: "message" }, { type: "text.delta" }, { type: "message" }, { type: "tool.start" }];
  assert.equal(liveMessageCount(evs), 2);
  assert.equal(liveMessageCount([]), 0);
});

/* ---------- entryKey ---------- */

test("entryKey: turn marks key by turn number; tool folds key by call_id regardless of running state", () => {
  assert.equal(entryKey({ kind: "turn-mark", turn: 3 }, 0), "turn:3");
  assert.equal(entryKey({ kind: "tool", id: "c1", running: true }, 0), "tool:c1");
  assert.equal(entryKey({ kind: "tool", id: "c1", running: false }, 0), "tool:c1");
});

test("entryKey: settled messages key by durable id; drafts and queued placeholders key without one", () => {
  assert.equal(entryKey({ kind: "assistant", id: "m1" }, 0), "msg:m1");
  assert.equal(entryKey({ kind: "assistant", id: null, streaming: true }, 0), "draft:assistant");
  assert.equal(entryKey({ kind: "reasoning", id: null, streaming: true }, 0), "draft:reasoning");
  assert.equal(entryKey({ kind: "operator", id: null, queued: true, queueId: 7 }, 0), "queued:7");
  assert.equal(entryKey({ kind: "operator", id: null, queued: true, queueId: null }, 2), "queued:idx:2");
});
