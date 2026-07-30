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
  sameOriginDefaultBase,
  embeddedConnectPlan,
  extractFragmentToken,
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
  turnMarkAgoText,
  keepsLiveEventAfterReconcile,
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

/* ---------- sameOriginDefaultBase (embedded GET /monitor same-origin
   default) ---------- */

test("sameOriginDefaultBase: pathname '/monitor' defaults the base URL to the page's own origin", () => {
  assert.equal(sameOriginDefaultBase({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }), "http://127.0.0.1:4096");
});

test("sameOriginDefaultBase: a trailing-slash pathname ('/monitor/') also matches", () => {
  assert.equal(sameOriginDefaultBase({ pathname: "/monitor/", origin: "http://127.0.0.1:4096" }), "http://127.0.0.1:4096");
});

test("sameOriginDefaultBase: any other pathname (file://, a static host's own path, the board root) returns null", () => {
  assert.equal(sameOriginDefaultBase({ pathname: "/", origin: "http://127.0.0.1:4096" }), null);
  assert.equal(sameOriginDefaultBase({ pathname: "/tools/monitor/index.html", origin: "https://cdn.example.com" }), null);
  assert.equal(sameOriginDefaultBase({ pathname: "/Users/dev/harness/tools/monitor/index.html", origin: "null" }), null);
  // "monitoring" shares "/monitor" as a prefix but is a DIFFERENT path —
  // must not false-positive on a substring match.
  assert.equal(sameOriginDefaultBase({ pathname: "/monitoring", origin: "http://127.0.0.1:4096" }), null);
});

test("sameOriginDefaultBase: tolerates missing/malformed input without throwing", () => {
  assert.equal(sameOriginDefaultBase(null), null);
  assert.equal(sameOriginDefaultBase(undefined), null);
  assert.equal(sameOriginDefaultBase({}), null);
  assert.equal(sameOriginDefaultBase({ pathname: "/monitor" }), null); // no origin
  assert.equal(sameOriginDefaultBase({ pathname: "/monitor", origin: "" }), null); // empty origin
});

/* ---------- embeddedConnectPlan (RED-FIRST — same-origin auto-connect:
   opening a local box's /monitor drops the operator straight on the board,
   no panel, no typing, unless/until an attempt actually fails) ---------- */

test("embeddedConnectPlan: not embedded (file:// / static host) — show-full-panel, no auto-attempt, no notice", () => {
  const plan = embeddedConnectPlan({ pathname: "/", origin: "http://127.0.0.1:4096" }, null, "", "http://elsewhere:9000");
  assert.deepEqual(reify(plan), { embedded: false, base: null, autoToken: null, showBaseField: true, crossBoxNotice: null });
});

test("embeddedConnectPlan: RED-FIRST — embedded with NO token anywhere still signals auto-attempt-same-origin (autoToken null, not a reason to skip trying — covers the loopback-Unauthenticated case)", () => {
  const plan = embeddedConnectPlan({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }, null, "", null);
  assert.equal(plan.embedded, true);
  assert.equal(plan.base, "http://127.0.0.1:4096");
  assert.equal(plan.autoToken, null);
  assert.equal(plan.showBaseField, false);
  assert.equal(plan.crossBoxNotice, null);
});

test("embeddedConnectPlan: an explicit fragment token (#t=) wins as autoToken over a stored one — the fresher credential", () => {
  const plan = embeddedConnectPlan({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }, "fresh-tok", "stale-tok", null);
  assert.equal(plan.autoToken, "fresh-tok");
});

test("embeddedConnectPlan: falls back to the stored token when no fragment token is present", () => {
  const plan = embeddedConnectPlan({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }, null, "stale-tok", null);
  assert.equal(plan.autoToken, "stale-tok");
});

test("embeddedConnectPlan: RED-FIRST — a fragment base naming a DIFFERENT origin surfaces show-cross-box-notice, composing with (not replacing) the auto-attempt", () => {
  const plan = embeddedConnectPlan({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }, "tok", "", "http://other-box:4096");
  assert.equal(plan.embedded, true, "the own-origin auto-attempt must still be signaled");
  assert.equal(plan.base, "http://127.0.0.1:4096", "the own-origin base must still be usable despite the notice");
  assert.equal(plan.autoToken, "tok", "the auto-attempt's token is unaffected by the notice");
  assert.ok(plan.crossBoxNotice, "expected a cross-box notice");
  assert.ok(plan.crossBoxNotice.includes("other-box:4096"), "notice must name the OTHER box: " + plan.crossBoxNotice);
  assert.ok(plan.crossBoxNotice.includes("/monitor"), "notice should point at the other box's own /monitor: " + plan.crossBoxNotice);
});

test("embeddedConnectPlan: a fragment base that AGREES with this origin is not a conflict — no notice", () => {
  const plan = embeddedConnectPlan({ pathname: "/monitor", origin: "http://127.0.0.1:4096" }, null, "", "http://127.0.0.1:4096");
  assert.equal(plan.crossBoxNotice, null);
});

test("embeddedConnectPlan: show-token-panel's static half — embedded always signals showBaseField false (base fixed/known), regardless of whether a token is available yet", () => {
  // The DYNAMIC half (whether a panel is ever actually shown) depends on
  // the auto-attempt's real result, which this pure function cannot know
  // — see its own doc comment. bootstrap() only reveals a panel when
  // attemptConnect resolves false; when it does, THIS is what that panel
  // renders as.
  assert.equal(embeddedConnectPlan({ pathname: "/monitor", origin: "http://x" }, null, "", null).showBaseField, false);
  assert.equal(embeddedConnectPlan({ pathname: "/monitor", origin: "http://x" }, "tok", "", null).showBaseField, false);
});

test("embeddedConnectPlan: tolerates missing/malformed input without throwing", () => {
  assert.equal(embeddedConnectPlan(null, null, null, null).embedded, false);
  assert.equal(embeddedConnectPlan({ pathname: "/monitor", origin: "http://x" }, undefined, undefined, undefined).autoToken, null);
  assert.equal(embeddedConnectPlan({ pathname: "/monitor", origin: "http://x" }, "", "", "").crossBoxNotice, null); // empty strings are "absent", not conflicts
});

/* ---------- extractFragmentToken (RED-FIRST — #t= capability URL:
   zero-typing access without weakening auth) ---------- */

test("extractFragmentToken: adopts a plain '#t=<token>' and scrubs it, leaving a bare '#'", () => {
  const r = extractFragmentToken("#t=abc123");
  assert.equal(r.token, "abc123");
  assert.equal(r.cleanedHash, "#");
});

test("extractFragmentToken: RED-FIRST — scrubbing preserves OTHER recognized params (s=) intact", () => {
  const r = extractFragmentToken("#t=abc123&s=ses_01ky9fjq2wexvq8rn0m4tdq2m");
  assert.equal(r.token, "abc123");
  assert.equal(r.cleanedHash, "#s=ses_01ky9fjq2wexvq8rn0m4tdq2m");
});

test("extractFragmentToken: preserves the base param (b=) too, in either param order", () => {
  const r1 = extractFragmentToken("#b=http%3A%2F%2Flocalhost%3A4096&t=abc123");
  assert.equal(r1.token, "abc123");
  assert.equal(r1.cleanedHash, "#b=http%3A%2F%2Flocalhost%3A4096");
  const r2 = extractFragmentToken("#t=abc123&b=http%3A%2F%2Flocalhost%3A4096&s=xyz");
  assert.equal(r2.token, "abc123");
  assert.equal(r2.cleanedHash, "#b=http%3A%2F%2Flocalhost%3A4096&s=xyz");
});

test("extractFragmentToken: no 't' param — token null, cleanedHash unchanged (modulo re-encoding)", () => {
  const r = extractFragmentToken("#s=xyz");
  assert.equal(r.token, null);
  assert.equal(r.cleanedHash, "#s=xyz");
  const empty = extractFragmentToken("#");
  assert.equal(empty.token, null);
  assert.equal(empty.cleanedHash, "#");
});

test("extractFragmentToken: an explicitly empty '#t=' is null (nothing to adopt), not an empty-string credential", () => {
  const r = extractFragmentToken("#t=");
  assert.equal(r.token, null);
});

test("extractFragmentToken: percent-decodes the token value, tolerating malformed encoding (degrades to raw text)", () => {
  assert.equal(extractFragmentToken("#t=a%20b").token, "a b");
  assert.equal(extractFragmentToken("#t=%zz").token, "%zz");
});

test("extractFragmentToken: a repeated 't' param uses the LAST occurrence (matches unroute's own repeated-param handling)", () => {
  const r = extractFragmentToken("#t=first&t=second");
  assert.equal(r.token, "second");
});

test("extractFragmentToken: tolerates junk hashes without throwing", () => {
  assert.deepEqual(reify(extractFragmentToken(undefined)), { token: null, cleanedHash: "#" });
  assert.deepEqual(reify(extractFragmentToken(null)), { token: null, cleanedHash: "#" });
  assert.deepEqual(reify(extractFragmentToken("not-a-hash-at-all")), { token: null, cleanedHash: "#" });
  assert.deepEqual(reify(extractFragmentToken("#&&&")), { token: null, cleanedHash: "#" });
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

/* ---------- boardModel: the "empty" detail cell (RED-FIRST — a zero-message
   idle session used to render a blank detail cell, indistinguishable from a
   rendering failure; reported via live testing: an operator clicked one
   expecting content). Precedence, lowest priority — verified: any outcome,
   or a paused goal, still wins; any messages > 0, or a missing/unpolled
   messages field, suppresses it. ---------- */

test("boardModel: an idle, never-prompted session (messages: 0, no outcome, no goal) shows a dim 'empty' detail cell, not blank", () => {
  const now = 100_000;
  const fresh = { id: "s1", state: "idle", queued: 0, messages: 0, last_activity_at: new Date(now).toISOString() };
  const row = boardModel([fresh], activityMap({ s1: seedActivity(fresh) }), now)[0];
  assert.equal(row.detail, "empty");
  assert.equal(row.detailCritical, false);
});

test("boardModel: 'empty' is suppressed the instant messages > 0", () => {
  const now = 100_000;
  const used = { id: "s1", state: "idle", queued: 0, messages: 1, last_activity_at: new Date(now).toISOString() };
  const row = boardModel([used], activityMap({ s1: seedActivity(used) }), now)[0];
  assert.equal(row.detail, null);
});

test("boardModel: 'empty' never overrides a real outcome or a paused-goal presentation, even when messages is also 0", () => {
  const now = 100_000;
  const completedButZero = { id: "s1", state: "idle", queued: 0, messages: 0, last_turn: { outcome: "completed" }, last_activity_at: new Date(now).toISOString() };
  const parkedButZero = {
    id: "s2", state: "idle", queued: 0, messages: 0,
    goal: { condition: "ship it", active: true, paused: true, pause_reason: "worker_failure" },
    last_activity_at: new Date(now).toISOString(),
  };
  const rows = boardModel([completedButZero, parkedButZero], activityMap({ s1: seedActivity(completedButZero), s2: seedActivity(parkedButZero) }), now);
  const byId = Object.fromEntries(rows.map(r => [r.id, r]));
  assert.equal(byId.s1.detail, "completed");
  assert.equal(byId.s2.detail, "worker parked");
});

test("boardModel: a session this page has only seen via a live stub (no `messages` field yet, before the next poll) does not show 'empty'", () => {
  const now = 100_000;
  const unpolled = { id: "s1", state: "idle", queued: 0, last_activity_at: new Date(now).toISOString() }; // no `messages` field at all
  const row = boardModel([unpolled], activityMap({ s1: seedActivity(unpolled) }), now)[0];
  assert.equal(row.detail, null, "an undefined messages count must not be guessed as zero");
});

/* ---------- detail live-chip: fresh-idle phase + the hidden-attribute CSS
   bug that actually made it visible (BUG 1) ----------

   index.html's updateDetailHeader (outside the TESTABLE region — it touches
   the DOM) derives the chip from the exact same boardModel call the board's
   own syncBoard makes: `boardModel([sessionSummary], state.activities,
   nowMs)[0]`, then sets `chip.hidden = row.cssClass === "idle"`. The two
   tests below cover the two, independently-necessary halves of that being
   correct end to end — the pure model computation (testable in the vm
   sandbox) AND the CSS that has to actually respect `chip.hidden` for the
   model's "idle" answer to be visible (not testable in the vm sandbox —
   there is no DOM/CSS engine there — so this asserts against the raw
   stylesheet text instead; see the comment below for why a naive jsdom
   getComputedStyle check would not have caught this). */

test("detail chip: a freshly created, zero-event session resolves through boardModel (the SAME call updateDetailHeader makes) to cssClass 'idle', matching the board row", () => {
  // Mirrors updateDetailHeader's own fallback exactly: a session id not yet
  // seen in a GET /session poll response, so it falls back to a minimal
  // { id, state: "idle", queued: 0 } summary — and no activity has been
  // folded for it yet (activities is empty), so boardModel's activityFor
  // falls through to seedActivity(session) — the "zero messages, never
  // polled, never streamed to" state a just-opened detail view starts in.
  const now = 1_000_000;
  const sessionSummary = { id: "s-fresh", state: "idle", queued: 0 };
  const row = boardModel([sessionSummary], activityMap({}), now)[0];
  assert.equal(row.cssClass, "idle", "a fresh, never-active session must resolve idle, not live/quiet/bad");
  assert.equal(row.phase, "idle");
});

test("detail chip: the .livechip CSS rule must not defeat the `hidden` attribute", () => {
  // ROOT CAUSE of BUG 1 (verified against a real Chrome tab, not just read):
  // the browser's UA stylesheet hides a [hidden] element via a PLAIN (non-
  // !important) `[hidden] { display: none }` rule. CSS cascade origin
  // outranks specificity: ANY author-stylesheet rule that sets `display` on
  // the same element — regardless of that rule's own selector's specificity
  // — beats a user-agent-origin rule. index.html used to declare
  // `.detail-head .livechip { display: inline-flex; ... }` unconditionally,
  // which defeated `chip.hidden = true` outright: in a real browser the
  // element stayed fully visible (getComputedStyle().display ===
  // "inline-flex", non-null offsetParent) with `hidden` set, showing
  // whatever text (or the markup's original hardcoded "tool running"
  // default) was last assigned — exactly the reported symptom on a freshly
  // opened, idle session's detail view. jsdom's own getComputedStyle does
  // NOT reproduce this (it special-cases `hidden` rather than emulating the
  // real cascade), which is exactly why this had to be found by live
  // browser testing and why this regression check is a text/regex
  // assertion against the shipped CSS, not a DOM-rendering one.
  const styleMatch = html.match(/<style>([\s\S]*?)<\/style>/);
  assert.ok(styleMatch, "index.html must contain a <style> block");
  const css = styleMatch[1];
  assert.doesNotMatch(
    css,
    /\.livechip\s*\{[^}]*display\s*:/,
    "a bare `.livechip { ... display: ... }` rule (no :not([hidden]) guard) silently defeats chip.hidden = true in a real browser",
  );
  assert.match(
    css,
    /\.livechip:not\(\[hidden\]\)\s*\{[^}]*display\s*:/,
    "the rule that sets .livechip's display must guard with :not([hidden]) so the hidden attribute isn't defeated",
  );
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
  // The turn is genuinely still open here (busy, no content, no turn.end) —
  // the pending "Thinking…" indicator (Change 2) correctly trails it; see
  // this file's own pending-indicator test block for that behavior in
  // isolation.
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "pending"]);
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

/* ---------- transcriptModel: error/aborted entries (BUG 2, RED-FIRST: a
   failed or aborted turn used to render NOTHING for the failure itself —
   only the preceding operator message — because transcriptModel had no
   case at all for session.error/turn.end/session.aborted; they silently
   fell into the live-event loop's `default: break;`.) ---------- */

test("transcriptModel: [status busy, message(user), session.error(text), status idle] folds to [turn-mark, operator, error] with the error text", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "run it" }] } },
    { type: "session.error", session_id: "s1", error: "provider request failed: 503" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "error"]);
  assert.equal(entries[2].text, "provider request failed: 503");
});

test("transcriptModel: turn.end-with-error variant — a live turn.end(outcome:'error') also folds an error entry when no session.error preceded it", () => {
  // Exercises the OTHER of the two sources (see server/journal.go:
  // recordTurnEnd's Error field is set whenever turnErr != nil) on its own,
  // e.g. a page connecting to the stream in the narrow window between the
  // real session.error and turn.end calls (both durable, but two separate
  // SSE frames) would only observe this one.
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "run it" }] } },
    { type: "turn.end", outcome: "error", error: "context deadline exceeded" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "error"]);
  assert.equal(entries[2].text, "context deadline exceeded");
});

test("transcriptModel: session.error immediately followed by turn.end(outcome:'error') with the SAME text folds exactly ONE error entry, not two", () => {
  // The real wire ALWAYS pairs these for a genuine failure (see
  // server/handlers.go's runPrompt/runGoal default branch: session.error
  // then recordTurnEnd, same sanitized err.Error() text both times) — this
  // is the ordinary case, not an edge case, so double-rendering it would be
  // the common outcome, not a rare one.
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "run it" }] } },
    { type: "session.error", error: "provider request failed: 503" },
    { type: "turn.end", outcome: "error", error: "provider request failed: 503" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  const errors = entries.filter(e => e.kind === "error");
  assert.equal(errors.length, 1, "session.error and its matching turn.end must not double-render the same failure");
  assert.equal(errors[0].text, "provider request failed: 503");
});

test("transcriptModel: turn.end outcomes other than 'error' (e.g. 'completed') never fold an error entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "run it" }] } },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "error").length, 0);
});

/* ---------- transcriptModel: "turn completed with no output" placeholder
   (RED-FIRST — a turn that ends completed with no assistant text, no tool
   call, and no error used to render NOTHING beyond its own operator
   prompt, indistinguishable from the turn having silently vanished;
   observed with an exhausted scripted provider, rare but real with an
   actual model too). ---------- */

test("transcriptModel: [status busy, message(user), turn.end(completed), status idle] folds a 'turn completed with no output' placeholder", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "do the thing" }] } },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "turn-empty"]);
  assert.equal(entries[2].text, "turn completed with no output");
});

test("transcriptModel: an assistant text entry in the turn suppresses the no-output placeholder", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "do the thing" }] } },
    { type: "message", message: { id: "m2", role: "assistant", created_at: "t1", parts: [{ type: "text", text: "done" }] } },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "turn-empty").length, 0);
});

test("transcriptModel: a tool call in the turn suppresses the no-output placeholder", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "do the thing" }] } },
    { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "ls" } } },
    { type: "tool.end", tool_call: { call_id: "c1" }, output: [{ type: "text", text: "ok" }], is_error: false },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "turn-empty").length, 0);
});

test("transcriptModel: an error entry in the turn suppresses the no-output placeholder (a failure is not silently-empty)", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "do the thing" }] } },
    { type: "session.error", error: "boom" },
    { type: "turn.end", outcome: "error", error: "boom" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "turn-empty").length, 0);
});

test("transcriptModel: reasoning-only output still counts as 'no output' — the placeholder still folds", () => {
  // Deliberate: the task's own definition of "output" is assistant text,
  // tool call, or error — reasoning is explicitly excluded, so a turn that
  // only thought out loud and never replied or acted is exactly the "no
  // output" case, not a false negative. The open reasoning draft itself
  // does not survive into the result: the trailing session.status(idle)
  // resetDraft()s it away, same as it always has for ANY never-finalized
  // draft, regardless of this placeholder feature — only entries already
  // pushed into `entries` (turn-mark, operator, and now turn-empty) do.
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "do the thing" }] } },
    { type: "reasoning.delta", text: "thinking about it" },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "turn-empty"]);
});

test("transcriptModel: each turn is judged independently — an earlier turn's real output does not suppress a LATER empty turn's placeholder", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "first" }] } },
    { type: "message", message: { id: "m2", role: "assistant", created_at: "t1", parts: [{ type: "text", text: "first reply" }] } },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m3", role: "user", created_at: "t2", parts: [{ type: "text", text: "second" }] } },
    { type: "turn.end", outcome: "completed" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  const emptyMarks = entries.filter(e => e.kind === "turn-empty");
  assert.equal(emptyMarks.length, 1, "only the second (genuinely empty) turn gets a placeholder");
  assert.equal(emptyMarks[0].turn, 2);
});

test("entryKey: turn-empty placeholders key by turn number", () => {
  assert.equal(entryKey({ kind: "turn-empty", turn: 3 }, 0), "turn-empty:3");
});

test("transcriptModel: a live session.aborted event folds into an error-styled entry reading 'turn aborted'", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t0", parts: [{ type: "text", text: "run it" }] } },
    { type: "session.aborted", session_id: "s1" },
    { type: "session.status", status: "idle" },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "error"]);
  assert.equal(entries[2].text, "turn aborted");
});

test("entryKey: error entries key by their position among the folded entries", () => {
  assert.equal(entryKey({ kind: "error", text: "boom" }, 5), "error:5");
});

/* ---------- transcriptModel: pendingSends (Change 1, RED-FIRST — an
   optimistic operator message rendered on composer submit, before the POST
   even resolves). Passed as transcriptModel's 4th argument, folded as a
   TRAILING operator entry ({kind:"operator", pendingSend:true, clientId}),
   deduped the instant a real durable/stream operator message (a "message"
   event, or the busy-session "prompt.queued" placeholder) arrives for it —
   matched by containment/FIFO, the same tolerance prompt.queued's own
   existing dedup already needs (the engine template-wraps a queued prompt's
   eventual delivery — see server/handlers.go's queued-prompt injection), not
   strict equality. ---------- */

test("transcriptModel: a pendingSend renders as a trailing operator entry", () => {
  const entries = transcriptModel([], [], 0, [{ text: "check the tests", clientId: "c1" }]);
  const last = entries[entries.length - 1];
  assert.equal(last.kind, "operator");
  assert.equal(last.text, "check the tests");
  assert.equal(last.pendingSend, true);
  assert.equal(last.clientId, "c1");
});

test("transcriptModel: a pendingSend dedups to exactly one entry once its matching 'message' event lands (idle-dispatch case)", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "check the tests" }] } },
  ];
  const entries = transcriptModel([], evs, 0, [{ text: "check the tests", clientId: "c1" }]);
  const operatorEntries = entries.filter(e => e.kind === "operator");
  assert.equal(operatorEntries.length, 1, "the pendingSend must not duplicate the real message: " + JSON.stringify(reify(operatorEntries)));
  assert.equal(operatorEntries[0].pendingSend, undefined);
  assert.equal(operatorEntries[0].text, "check the tests");
});

test("transcriptModel: a pendingSend dedups to exactly one entry once its matching 'prompt.queued' event lands (busy-session case)", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "prompt.queued", queue_text: "check the tests", queue_id: 3 },
  ];
  const entries = transcriptModel([], evs, 0, [{ text: "check the tests", clientId: "c1" }]);
  const operatorEntries = entries.filter(e => e.kind === "operator");
  assert.equal(operatorEntries.length, 1, "the pendingSend must not duplicate the real prompt.queued placeholder: " + JSON.stringify(reify(operatorEntries)));
  assert.equal(operatorEntries[0].queued, true);
  assert.equal(operatorEntries[0].pendingSend, undefined);
});

test("transcriptModel: a pendingSend with no matching event survives, alongside the unrelated real entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "totally different text" }] } },
  ];
  const entries = transcriptModel([], evs, 0, [{ text: "check the tests", clientId: "c1" }]);
  const operatorEntries = entries.filter(e => e.kind === "operator");
  assert.equal(operatorEntries.length, 2, "an unmatched pendingSend must survive alongside the unrelated real message: " + JSON.stringify(reify(operatorEntries)));
  assert.ok(operatorEntries.some(e => e.pendingSend && e.text === "check the tests"));
  assert.ok(operatorEntries.some(e => !e.pendingSend && e.text === "totally different text"));
});

test("transcriptModel: a pendingSend matches a template-wrapped queued delivery by containment, not strict equality", () => {
  const wrapped = "OPERATOR MESSAGES (address these, then continue the task): 1. check the tests";
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: wrapped }] } },
  ];
  const entries = transcriptModel([], evs, 0, [{ text: "check the tests", clientId: "c1" }]);
  const operatorEntries = entries.filter(e => e.kind === "operator");
  assert.equal(operatorEntries.length, 1, "containment match must dedup the wrapped delivery, not leave a duplicate: " + JSON.stringify(reify(operatorEntries)));
  assert.equal(operatorEntries[0].text, wrapped);
});

test("entryKey: a pendingSend keys by its clientId, independent of a queued placeholder's queueId keying", () => {
  assert.equal(entryKey({ kind: "operator", id: null, pendingSend: true, clientId: "c7" }, 0), "pendingSend:c7");
});

/* ---------- transcriptModel: the "Thinking…" pending-assistant indicator
   (Change 2, RED-FIRST) — a real, currently OPEN turn (a live
   session.status busy observed, with no session.status idle since) that has
   produced no renderable content yet folds a single trailing
   {kind:"pending"} entry, dismissed the instant ANY content (reasoning
   delta, text delta, tool start, an already-assembled assistant message, or
   an error) arrives, and never coexists with the turn-empty "turn completed
   with no output" placeholder for the same turn — this is derived PURELY
   from the fold (liveEvents), never from an externally-supplied busy/idle
   flag, so a not-yet-dispatched queued send never triggers it on its own
   (see the queued-behind-a-running-turn scenario below). ---------- */

test("transcriptModel: an open turn with no content yet folds a pending entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
  ];
  const entries = transcriptModel([], evs);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "pending"]);
});

test("transcriptModel: the first text.delta dismisses the pending entry, replaced by the streaming entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
    { type: "text.delta", text: "working" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "pending").length, 0);
  assert.ok(entries.some(e => e.kind === "assistant" && e.streaming));
});

test("transcriptModel: the first reasoning.delta dismisses the pending entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
    { type: "reasoning.delta", text: "thinking" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "pending").length, 0);
});

test("transcriptModel: tool.start dismisses the pending entry", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
    { type: "tool.start", tool_call: { call_id: "c1", name: "Bash", arguments: { command: "ls" } } },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "pending").length, 0);
});

test("transcriptModel: a turn completing with no output shows the 'no output' placeholder, never the pending entry — the two never coexist", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "go" }] } },
    { type: "turn.end", outcome: "completed" },
  ];
  const entries = transcriptModel([], evs);
  assert.equal(entries.filter(e => e.kind === "pending").length, 0);
  assert.equal(entries.filter(e => e.kind === "turn-empty").length, 1);
});

test("entryKey: the pending entry keys by a fixed tag — one node, created once, never rebuilt per tick", () => {
  assert.equal(entryKey({ kind: "pending" }, 0), "pending");
});

test("transcriptModel: a pendingSend queued behind a DIFFERENT, currently-running turn renders its own operator entry, while the pending indicator attaches only to the running turn — no special-casing idle-vs-busy needed", () => {
  const evs = [
    { type: "session.status", status: "busy" },
    { type: "message", message: { id: "m1", role: "user", created_at: "t", parts: [{ type: "text", text: "run turn A" }] } },
  ];
  const entries = transcriptModel([], evs, 0, [{ text: "also do B", clientId: "c9" }]);
  assert.deepEqual(reify(entries.map(e => e.kind)), ["turn-mark", "operator", "pending", "operator"]);
  assert.equal(entries[3].pendingSend, true);
  assert.equal(entries[3].text, "also do B");
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

/* ---------- turnMarkAgoText (NIT fix: settled turn-marks' "· Xm ago" used
   to freeze, because entrySignature keyed on the immutable entry.at instead
   of the RENDERED text — see entrySignature's turn-mark case, which now
   keys on this function's own return value so a later tick's different
   nowMs produces a different signature and actually re-patches the DOM
   node, mirroring boardModel's elapsed.text-in-the-signature approach).
   entrySignature itself lives outside this file's TESTABLE region (its
   "tool" case reads the closured detailState, same reason toolElapsedText
   was never made TESTABLE either) — this function is the pure heart of the
   fix, factored out specifically so it IS unit-testable; the full
   entrySignature/DOM integration is covered by the e2e's ticking
   assertion. ---------- */

test("turnMarkAgoText: empty for a still-open turn (no entry.at) or an unparseable one", () => {
  assert.equal(turnMarkAgoText({ turn: 1, at: null }, 100_000), "");
  assert.equal(turnMarkAgoText({ turn: 1, at: "not-a-date" }, 100_000), "");
});

test("turnMarkAgoText: RED-FIRST — the same settled entry (entry.at is immutable) renders a DIFFERENT string as nowMs advances", () => {
  // This is the exact fact entrySignature's turn-mark case now keys on
  // instead of the frozen entry.at alone — before the fix, nothing in the
  // signature ever changed for a settled turn-mark, so syncTranscript's
  // signature-gated patch never re-touched the node and the "ago" text
  // froze at whatever it first rendered.
  const entry = { turn: 1, at: new Date(0).toISOString() };
  assert.equal(turnMarkAgoText(entry, 5_000), "5s ago");
  assert.equal(turnMarkAgoText(entry, 12 * 60 * 1000), "12m ago");
  assert.notEqual(turnMarkAgoText(entry, 5_000), turnMarkAgoText(entry, 12 * 60 * 1000));
});

/* ---------- keepsLiveEventAfterReconcile (MEDIUM fix: detail transcript
   permanently missed events streamed during a reconnect / PERF fix:
   liveEvents grew unbounded — reconcileDetail unifies both fixes around one
   seq boundary; this is that boundary's pure predicate, RED-FIRST since
   neither reconcileDetail nor this predicate existed before.) ---------- */

test("keepsLiveEventAfterReconcile: keeps only events with a numeric seq strictly greater than snapSeq — equal is dropped, not kept", () => {
  assert.equal(keepsLiveEventAfterReconcile({ type: "session.status", seq: 10 }, 5), true);
  assert.equal(keepsLiveEventAfterReconcile({ type: "session.status", seq: 5 }, 5), false);
  assert.equal(keepsLiveEventAfterReconcile({ type: "session.status", seq: 4 }, 5), false);
});

test("keepsLiveEventAfterReconcile: seq-less (transient, non-journaled) events are ALWAYS dropped, regardless of snapSeq", () => {
  // text.delta/reasoning.delta/tool.start/tool.end/compaction_failed carry
  // no seq at all (server/journal.go's publishLive, never emitDurable) —
  // there is no way to place them relative to snapSeq's boundary, so the
  // rule is an unconditional drop, not "keep if snapSeq is very low".
  assert.equal(keepsLiveEventAfterReconcile({ type: "text.delta", text: "hi" }, 0), false);
  assert.equal(keepsLiveEventAfterReconcile({ type: "tool.start" }, -1), false);
  assert.equal(keepsLiveEventAfterReconcile({ type: "reasoning.delta" }, Number.MIN_SAFE_INTEGER), false);
});

test("keepsLiveEventAfterReconcile: a non-numeric seq is treated as absent (dropped), and null/undefined events never throw", () => {
  assert.equal(keepsLiveEventAfterReconcile({ type: "text.delta", seq: "10" }, 0), false);
  assert.equal(keepsLiveEventAfterReconcile(null, 5), false);
  assert.equal(keepsLiveEventAfterReconcile(undefined, 5), false);
});

test("keepsLiveEventAfterReconcile: RED-FIRST — session.error/turn.end/session.aborted are ALWAYS kept, even with seq <= snapSeq", () => {
  // These three are durable (carry a seq — server/journal.go's emitDurable)
  // but have NO GET /session/{id}/message representation at all: unlike
  // every other durable event this file handles, nothing in a fresh
  // /message snapshot could ever reconstruct one. An earlier version of
  // this rule dropped them like any other durable event once their seq
  // fell behind snapSeq — which silently erased a failed/aborted turn's
  // ENTIRE error entry (transcriptModel's pushIfNewError folds exclusively
  // from these three) the instant reconcileDetail's cap trigger fired
  // during a live error. Caught by the e2e's error-entry scenario timing
  // out once DETAIL_LIVE_EVENTS_CAP was tuned low for the buffer-cap
  // scenario — this is the regression test for that.
  assert.equal(keepsLiveEventAfterReconcile({ type: "session.error", seq: 1, error: "boom" }, 100), true);
  assert.equal(keepsLiveEventAfterReconcile({ type: "turn.end", seq: 1, outcome: "error", error: "boom" }, 100), true);
  assert.equal(keepsLiveEventAfterReconcile({ type: "turn.end", seq: 1, outcome: "completed" }, 100), true);
  assert.equal(keepsLiveEventAfterReconcile({ type: "session.aborted", seq: 1 }, 100), true);
});
