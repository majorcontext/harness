// The soak row of the browser job (HARNESS_E2E_SOAK=1). A tab runs a long
// scripted session on the persistent store, reloads, replays the history with
// session/load, and greps a large tree. The row measures each phase; the Go
// test asserts generous bounds and prints the table.
import { start, newSession, prompt, workers } from "./page.mjs";

const mib = (bytes) => Math.round((bytes / 1048576) * 10) / 10;
const pad = "x".repeat(200);

let s;
let longTasks = [];
let lagMax = 0;
let probeID = 0;

// observe collects the frames of the agent that carry timings.
function observe(c) {
  const o = { updates: {}, compactions: [], tools: [], compactionStart: new Map(), toolStart: new Map() };
  c.onFrame = (f) => {
    const u = f.method === "session/update" ? f.params.update : undefined;
    if (!u) return;
    const now = performance.now();
    o.updates[u.sessionUpdate] = (o.updates[u.sessionUpdate] ?? 0) + 1;
    if (u.sessionUpdate === "compaction_update") {
      if (u.status === "in_progress") o.compactionStart.set(u.compactionId, now);
      else if (u.status === "completed" && o.compactionStart.has(u.compactionId)) o.compactions.push(now - o.compactionStart.get(u.compactionId));
    } else if (u.sessionUpdate === "tool_call") {
      o.toolStart.set(u.toolCallId, { now, title: u.title });
    } else if (u.sessionUpdate === "tool_call_update" && (u.status === "completed" || u.status === "failed") && o.toolStart.has(u.toolCallId)) {
      const began = o.toolStart.get(u.toolCallId);
      o.tools.push({ title: began.title, ms: now - began.now });
    }
  };
  return o;
}

// probe asks the Worker for its memory. The instrumented wasm_exec.js of the
// Go test answers; the module and the bridge ignore the message.
function probe() {
  const w = workers.at(-1);
  const id = ++probeID;
  return new Promise((resolve) => {
    const on = (e) => {
      if (e.data?.probe?.id !== id) return;
      w.removeEventListener("message", on);
      resolve(e.data.probe);
    };
    w.addEventListener("message", on);
    w.postMessage({ probe: id });
  });
}

async function sample() {
  await new Promise((resolve) => setTimeout(resolve, 100));
  const w = await probe();
  const out = {
    pageHeapMB: mib(performance.memory?.usedJSHeapSize ?? 0),
    wasmMB: mib(w.wasmBytes),
    longTaskMaxMs: Math.round(Math.max(0, ...longTasks)),
    longTasks: longTasks.length,
    lagMaxMs: Math.round(lagMax),
  };
  longTasks = [];
  lagMax = 0;
  return out;
}

const percentile = (sorted, p) => sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))] ?? 0;

/** soakOps are the operations that the soak row runs in a tab. */
export const soakOps = {
  async soakBoot(row) {
    try {
      new PerformanceObserver((list) => list.getEntries().forEach((e) => longTasks.push(e.duration))).observe({ type: "longtask" });
    } catch {
      // The browser has no long task entries; the row reports 0.
    }
    let last = performance.now();
    setInterval(() => {
      const now = performance.now();
      lagMax = Math.max(lagMax, now - last - 10);
      last = now;
    }, 10);
    const started = await start(row);
    s = { ...started, obs: observe(started.c) };
    return { bootMs: started.bootMs };
  },

  soakNewSession: () => newSession(s.c, s.logs),

  async soakTurns({ sessionId, from, to }) {
    const ms = [];
    const failures = [];
    s.obs.compactions.length = 0;
    const began = performance.now();
    for (let n = from; n <= to; n++) {
      const t0 = performance.now();
      const done = await prompt(s.c, sessionId, `soak turn ${n}: ${pad}`);
      ms.push(performance.now() - t0);
      if (done.result?.stopReason !== "end_turn") failures.push({ turn: n, error: done.error ?? done.result });
    }
    const sorted = [...ms].sort((a, b) => a - b);
    return {
      turns: ms.length,
      totalMs: performance.now() - began,
      p50Ms: percentile(sorted, 0.5),
      p95Ms: percentile(sorted, 0.95),
      maxMs: sorted.at(-1) ?? 0,
      failureCount: failures.length,
      failures: failures.slice(0, 3),
      compactionMs: [...s.obs.compactions],
    };
  },

  async soakLoad({ sessionId }) {
    s.obs.updates = {};
    const t0 = performance.now();
    const loaded = await s.c.call("session/load", { sessionId, cwd: "/work", mcpServers: [] });
    return { replayMs: performance.now() - t0, error: loaded.error, updates: { ...s.obs.updates } };
  },

  async soakSeed({ dirs, perDir }) {
    const t0 = performance.now();
    for (let d = 0; d < dirs; d++) {
      await s.h.fs.mkdir(`/work/tree/d${d}`, { recursive: true });
      const lines = Array.from({ length: 20 }, (_, i) => `line ${i} of the file with some text to scan`);
      await Promise.all(
        Array.from({ length: perDir }, (_, f) => {
          const body = (f % 7 === 0 ? `needle_${d}_${f}\n` : "") + lines.join("\n") + "\n";
          return s.h.fs.writeFile(`/work/tree/d${d}/f${f}.txt`, body);
        }),
      );
    }
    return { seedMs: performance.now() - t0, files: dirs * perDir };
  },

  async soakGrep({ sessionId }) {
    s.obs.tools.length = 0;
    const t0 = performance.now();
    const done = await prompt(s.c, sessionId, "soak grep");
    return { turnMs: performance.now() - t0, stopReason: done.result?.stopReason, error: done.error, tools: [...s.obs.tools] };
  },

  soakSample: () => sample(),

  async soakClose() {
    await s.h.close();
    return {};
  },
};

/** soakRow runs the phases in one tab and returns their measurements. */
export async function soakRow(row, tabs) {
  const t = await tabs.open();
  const run = (name, args) => tabs.run(t, name, args);
  const boot = { ...row, persist: true, compaction: true };
  const phases = {};
  await run("soakBoot", boot);
  const sessionId = await run("soakNewSession");
  phases.boot = await run("soakSample");
  const turns = await run("soakTurns", { sessionId, from: 1, to: row.turns });
  phases.turns = await run("soakSample");
  await tabs.reload(t);
  const reboot = await run("soakBoot", boot);
  phases.reboot = await run("soakSample");
  const load = await run("soakLoad", { sessionId });
  phases.replay = await run("soakSample");
  const seed = await run("soakSeed", { dirs: row.dirs, perDir: row.perDir });
  const grep = await run("soakGrep", { sessionId });
  phases.grep = await run("soakSample");
  await run("soakClose");
  await tabs.close(t);
  return { sessionId, turns, load, seed, grep, reboot, phases };
}
