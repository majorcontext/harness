// The page side of the browser job: it runs inside Chromium, starts the module
// in a Worker through createHarness, and runs one row against a fake model
// server. e2e/browser_test.go serves this file and asserts on the results.
import { createHarness, wasm } from "/embed/index.mjs";

const echo = {
  name: "js_echo",
  description: "Returns its text.",
  schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  run: async (input) => "echo:" + input.text,
};

const big = {
  name: "js_big",
  description: "Returns a text above the inline limit of a tool result.",
  schema: { type: "object", properties: {} },
  run: async () => Array.from({ length: 2000 }, (_, i) => "line " + i).join("\n"),
};

function acpClient(h) {
  const writer = h.stream.writable.getWriter();
  const reader = h.stream.readable.getReader();
  const waiting = new Map();
  const chunkWaiters = [];
  const text = [];
  let nextId = 1;
  (async () => {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) return;
      if (value.id !== undefined && waiting.has(value.id)) {
        waiting.get(value.id)(value);
        waiting.delete(value.id);
      } else if (value.method === "session/update" && value.params.update.sessionUpdate === "agent_message_chunk") {
        text.push(value.params.update.content.text);
        chunkWaiters.splice(0).forEach((fn) => fn());
      }
    }
  })();
  return {
    text,
    call(method, params) {
      const id = nextId++;
      return new Promise((resolve) => {
        waiting.set(id, resolve);
        writer.write({ jsonrpc: "2.0", id, method, params });
      });
    },
    notify: (method, params) => writer.write({ jsonrpc: "2.0", method, params }),
    firstChunk: () => (text.length ? Promise.resolve() : new Promise((resolve) => chunkWaiters.push(resolve))),
  };
}

function config(row) {
  const anthropic = {};
  if (row.baseURL) anthropic.base_url = row.baseURL;
  if (row.extraHeaders) anthropic.extra_headers = row.extraHeaders;
  return { model: "anthropic/claude-fable-5", providers: { anthropic } };
}

// start starts the module in a Worker with the paths relative to this page, and
// opens the ACP connection. A row with persist keeps the store in OPFS.
async function start(row, log = () => {}) {
  const logs = [];
  const started = performance.now();
  const h = await createHarness({
    transport: wasm({ module: "harness.wasm", wasmExec: "wasm_exec.js", worker: true }),
    config: config(row),
    proxy: row.proxy,
    tools: [echo, big],
    store: row.persist ? { persist: true } : {},
    log: (line) => (logs.push(line), log(line)),
  });
  const bootMs = performance.now() - started;
  const c = acpClient(h);
  await c.call("initialize", { protocolVersion: 1, clientCapabilities: {} });
  return { h, c, bootMs, logs };
}

async function newSession(c, logs) {
  const created = await c.call("session/new", { cwd: "/work", mcpServers: [] });
  if (created.error) throw new Error("session/new: " + JSON.stringify(created.error) + "\n" + logs.join("\n"));
  return created.result.sessionId;
}

// open starts the module and opens one ACP session.
async function open(row, log) {
  const { h, c, bootMs, logs } = await start(row, log);
  return { h, c, bootMs, sessionId: await newSession(c, logs) };
}

const prompt = (c, sessionId, text) => c.call("session/prompt", { sessionId, prompt: [{ type: "text", text }] });

const rows = {
  async turn(row) {
    const { h, c, bootMs, sessionId } = await open(row);
    const done = await prompt(c, sessionId, "go");
    await h.close();
    return { bootMs, stopReason: done.result?.stopReason, error: done.error, text: c.text.join("") };
  },

  async cancel(row) {
    const { h, c, sessionId } = await open(row);
    const turn = prompt(c, sessionId, "go");
    await c.firstChunk();
    const canceled = performance.now();
    await c.notify("session/cancel", { sessionId });
    const done = await turn;
    const endedMs = performance.now() - canceled;
    const serverSawClose = (await (await fetch("/await-canceled?row=" + encodeURIComponent(row.awaitRow))).text()) === "true";
    await h.close();
    return { stopReason: done.result?.stopReason, error: done.error, endedMs, serverSawClose };
  },

  // reload ends the page without closing the harness. The Worker ends with the
  // page, so the records of the first turn must already be in the store.
  async reload(row) {
    const t = await tabs.open();
    const run = (name, args) => tabs.run(t, name, args);
    await run("boot", { ...row, persist: true });
    const sessionId = await run("newSession");
    const first = await run("prompt", { sessionId, text: "one" });
    await tabs.reload(t);
    const stored = await peek();
    const boot = await run("boot", { ...row, persist: true });
    const resumed = await run("resume", { sessionId });
    const second = await run("prompt", { sessionId, text: "two" });
    await run("close");
    const final = await peek();
    await tabs.close(t);
    return { sessionId, first, second, stored, boot, resumed, final };
  },

  // twoTabs starts a second tab on a store that the first tab holds. The first
  // tab keeps writing while the second tab tries.
  async twoTabs(row) {
    const t1 = await tabs.open();
    const t2 = await tabs.open();
    const run = (t, name, args) => tabs.run(t, name, args);
    await run(t1, "boot", { ...row, persist: true });
    const sessionId = await run(t1, "newSession");
    const first = await run(t1, "prompt", { sessionId, text: "one" });
    const refused = await run(t2, "boot", { ...row, persist: true });
    const again = await run(t1, "prompt", { sessionId, text: "two" });
    await run(t1, "close");
    await tabs.close(t1);
    const final = await peek();
    await tabs.close(t2);
    return { sessionId, first, again, refused, final };
  },

  // handoff closes the first tab and boots the second on the same store.
  async handoff(row) {
    const t1 = await tabs.open();
    const t2 = await tabs.open();
    const run = (t, name, args) => tabs.run(t, name, args);
    await run(t1, "boot", { ...row, persist: true });
    const sessionId = await run(t1, "newSession");
    const first = await run(t1, "prompt", { sessionId, text: "one" });
    await run(t1, "close");
    await tabs.close(t1);
    const seen = await peek();
    const boot = await run(t2, "boot", { ...row, persist: true });
    const secondId = await run(t2, "newSession");
    const second = await run(t2, "prompt", { sessionId: secondId, text: "two" });
    await run(t2, "close");
    const final = await peek();
    await tabs.close(t2);
    return { sessionId, secondId, first, second, seen, boot, final };
  },

  // differentSessions starts two tabs at once. One tab gets the store and
  // creates a session in it; the other falls back to a memory store.
  async differentSessions(row) {
    const t1 = await tabs.open();
    const t2 = await tabs.open();
    const run = (t, name, args) => tabs.run(t, name, args);
    const boots = await Promise.all([t1, t2].map((t) => run(t, "boot", { ...row, persist: true })));
    const out = [];
    for (const [i, t] of [t1, t2].entries()) {
      const winner = boots[i].ok;
      if (!winner) {
        const memory = await run(t, "boot", { ...row, persist: false });
        if (!memory.ok) throw new Error("the memory store did not boot: " + memory.error);
      }
      const sessionId = await run(t, "newSession");
      const turn = await run(t, "prompt", { sessionId, text: "from tab " + (i + 1) });
      out.push({ winner, code: boots[i].code, turn });
    }
    await Promise.all([t1, t2].map((t) => run(t, "close")));
    const final = await peek();
    return { tabs: out, final };
  },

  // blob keeps a tool result above the inline limit in a blob file, and ends the
  // page without closing.
  async blob(row) {
    const t = await tabs.open();
    const run = (name, args) => tabs.run(t, name, args);
    await run("boot", { ...row, persist: true });
    const sessionId = await run("newSession");
    const turn = await run("prompt", { sessionId, text: "go" });
    await tabs.reload(t);
    const stored = await peek();
    await tabs.close(t);
    return { sessionId, turn, stored };
  },

  // kill ends the Worker right after the turn is acknowledged.
  async kill(row) {
    const t = await tabs.open();
    const run = (name, args) => tabs.run(t, name, args);
    await run("boot", { ...row, persist: true });
    const sessionId = await run("newSession");
    const turn = await run("prompt", { sessionId, text: "go" });
    await run("kill");
    const stored = await peek();
    await tabs.close(t);
    return { sessionId, turn, stored };
  },

  // files runs the file tools in the work directory, then opens a second
  // session there, which reads the AGENTS.md that the first one wrote.
  async files(row) {
    const { h, c, sessionId } = await open(row);
    const turn = await prompt(c, sessionId, "go");
    const second = await newSession(c, []);
    const again = await prompt(c, second, "again");
    await h.close();
    return { stopReason: turn.result?.stopReason, error: turn.error, againStop: again.result?.stopReason };
  },

  // fsprobe runs the Go probe on the file system of the Worker.
  async fsprobe(row) {
    const w = new NativeWorker("/page/fsprobe-worker.mjs", { type: "module" });
    try {
      const abs = (p) => new URL(p, location.href).href;
      return await new Promise((resolve, reject) => {
        w.onmessage = (e) => resolve(e.data);
        w.onerror = (e) => reject(new Error(e.message));
        w.postMessage({ mode: "go", wasm: abs("fsprobe.wasm"), wasmExec: abs("wasm_exec.js"), args: ["/work/p", "/persist/p"] });
      });
    } finally {
      w.terminate();
    }
  },

  // fskill ends a Worker after an fsync and a later unflushed write. The file
  // holds a prefix of what the Worker wrote and all the bytes before the fsync.
  async fskill() {
    const w = new NativeWorker("/page/fsprobe-worker.mjs", { type: "module" });
    const written = await new Promise((resolve, reject) => {
      w.onmessage = (e) => resolve(e.data);
      w.onerror = (e) => reject(new Error(e.message));
      w.postMessage({ mode: "kill" });
    });
    w.terminate();
    return { written, stored: await inspect({ file: "/persist/killtest" }) };
  },

  // offline leaves the network after the module booted. The driver returns it
  // when the first model request fails, so the turn must retry to finish.
  async offline(row) {
    const { h, c, sessionId } = await open(row);
    await globalThis.goOffline();
    const done = await prompt(c, sessionId, "go");
    await h.close();
    return { stopReason: done.result?.stopReason, error: done.error, text: c.text.join("") };
  },
};

const lockName = () => "harness:store:" + location.origin;

// tab is the harness of this page when a driver page runs it as a tab.
let tab;
const workers = [];
const NativeWorker = globalThis.Worker;
globalThis.Worker = class extends NativeWorker {
  constructor(...args) {
    super(...args);
    workers.push(this);
  }
};

// inspect reads the persistent store from a Worker of this page.
async function inspect(message = {}) {
  const w = new NativeWorker("/page/inspect-worker.mjs", { type: "module" });
  try {
    const out = await new Promise((resolve, reject) => {
      w.onmessage = (e) => resolve(e.data);
      w.onerror = (e) => reject(new Error(e.message));
      w.postMessage(message);
    });
    if (out.failure) throw new Error(out.failure);
    return out;
  } finally {
    w.terminate();
  }
}

/** tabOps are the operations that a driver page runs in a tab. */
export const tabOps = {
  async boot(row) {
    try {
      tab = await start(row);
      return { ok: true };
    } catch (err) {
      return { ok: false, error: err.message, code: err.code };
    }
  },
  newSession: async () => newSession(tab.c, tab.logs),
  async prompt({ sessionId, text }) {
    const before = tab.c.text.length;
    const done = await prompt(tab.c, sessionId, text);
    return { stopReason: done.result?.stopReason, error: done.error, text: tab.c.text.slice(before).join("") };
  },
  async resume({ sessionId }) {
    const done = await tab.c.call("session/resume", { sessionId, cwd: "/work" });
    return { error: done.error };
  },
  async close() {
    await tab.h.close();
    return {};
  },
  async kill() {
    workers.at(-1).terminate();
    return {};
  },
  inspect,
};

const tabs = {
  open: () => globalThis.tab("open"),
  run: (id, name, args) => globalThis.tab("run", id, { name, args }),
  reload: (id) => globalThis.tab("reload", id),
  close: (id) => globalThis.tab("close", id),
};

// peek reads the store from a new tab.
async function peek() {
  const t = await tabs.open();
  try {
    return await tabs.run(t, "inspect");
  } finally {
    await tabs.close(t);
  }
}

/** runRow runs the row named by row.kind and returns its result as JSON data. */
export async function runRow(row) {
  try {
    return await rows[row.kind](row);
  } catch (err) {
    return { failure: String(err?.stack ?? err) };
  }
}
