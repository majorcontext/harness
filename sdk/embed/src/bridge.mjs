// The page end of the tool bridge, of the fetch bridge, and of the file bridge.
//
// Tool: the Worker posts {tool: {tid, name, args}}; the page runs the tool and
// posts {toolResult: {tid, result | error}}. {toolAbort: {tid}} aborts the
// signal of the run.
//
// Fetch: the Worker posts {fetch: {fid, method, url, headers, body}}; the page
// runs its fetch and posts {fetchHead: {fid, status, statusText, headers}}, one
// {fetchChunk: {fid, chunk}} for each body chunk with the ArrayBuffer
// transferred, and {fetchEnd: {fid}}, or {fetchError: {fid, name, message}}.
// {fetchAbort: {fid}} aborts the fetch.
//
// File: the page posts {fsop: {id, op, args}}; the Worker runs op on the file
// system of the module and posts {fsres: {id, result | error: {code, message}}}.
// The ops are stat, readdir, mkdir, unlink, rmdir, rename, readFile, and
// writeFile(path, bytes, append). A byte array moves as a transferred
// ArrayBuffer in both directions.

function describe(err) {
  if (err instanceof Error) return { name: err.name, message: err.message };
  return { name: "Error", message: String(err) };
}

// toolResult maps what run returned to a string or {text, isError}.
function toolResult(value) {
  if (value === undefined || value === null) return "";
  if (typeof value === "string") return value;
  if (typeof value === "object" && typeof value.text === "string") return { text: value.text, isError: Boolean(value.isError) };
  return JSON.stringify(value);
}

const encoder = new TextEncoder();

// ownBytes copies data into a new ArrayBuffer that the page can transfer.
function ownBytes(data) {
  if (typeof data === "string") return encoder.encode(data).buffer;
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength).slice().buffer;
  if (data instanceof ArrayBuffer) return data.slice(0);
  throw new TypeError("data must be a string, an ArrayBuffer, or a typed array");
}

function opError({ code, message }) {
  return Object.assign(new Error(message), { code });
}

// pageFs returns the file system handle of the page (h.fs) over the fsop
// messages, the function that takes the answers, and the function that rejects
// every call that waits.
function pageFs(post) {
  let next = 0;
  const waiting = new Map();
  let closed = false;
  const call = (op, args, transfer = []) =>
    new Promise((resolve, reject) => {
      if (closed) {
        reject(new Error("the harness is closed"));
        return;
      }
      const id = ++next;
      waiting.set(id, { resolve, reject });
      post({ fsop: { id, op, args } }, transfer);
    });
  const code = (err) => err?.code;
  const stat = (path) => call("stat", [path]);

  async function mkdir(path, { recursive = false } = {}) {
    if (!recursive) return call("mkdir", [path]);
    let current = "";
    for (const part of path.split("/").filter(Boolean)) {
      current += "/" + part;
      try {
        await call("mkdir", [current]);
      } catch (err) {
        if (code(err) !== "EEXIST" || !(await stat(current)).isDirectory) throw err;
      }
    }
  }

  async function rm(path, { recursive = false, force = false } = {}) {
    let info;
    try {
      info = await stat(path);
    } catch (err) {
      if (force && code(err) === "ENOENT") return;
      throw err;
    }
    if (!info.isDirectory) return call("unlink", [path]);
    if (recursive) {
      for (const name of await call("readdir", [path])) await rm(`${path.replace(/\/$/, "")}/${name}`, { recursive, force });
    }
    return call("rmdir", [path]);
  }

  const fs = {
    stat,
    readdir: (path) => call("readdir", [path]),
    readFile: async (path) => new Uint8Array(await call("readFile", [path])),
    writeFile(path, data, { append = false } = {}) {
      const bytes = ownBytes(data);
      return call("writeFile", [path, bytes, append], [bytes]);
    },
    mkdir,
    rm,
    rename: (from, to) => call("rename", [from, to]),
  };
  return {
    fs,
    handle({ id, result, error }) {
      const entry = waiting.get(id);
      waiting.delete(id);
      if (error) entry?.reject(opError(error));
      else entry?.resolve(result);
    },
    close() {
      closed = true;
      for (const entry of waiting.values()) entry.reject(new Error("the harness is closed"));
      waiting.clear();
    },
  };
}

/**
 * pageBridge answers the tool and fetch messages of the Worker and carries the
 * file calls of the page. post sends a message to the Worker. With files true,
 * fs is the file system handle of the page and a tool gets it in its options.
 * handle takes one message; close aborts what runs.
 */
export function pageBridge({ tools, pageFetch, post, files = false }) {
  const byName = new Map(tools.map((t) => [t.name, t]));
  const running = new Map();
  const pf = files ? pageFs(post) : undefined;

  async function runTool({ tid, name, args }) {
    const ac = new AbortController();
    running.set("tool:" + tid, ac);
    try {
      const tool = byName.get(name);
      if (!tool) throw new Error(`unknown tool ${name}`);
      const result = toolResult(await tool.run(args, { signal: ac.signal, fs: pf?.fs }));
      post({ toolResult: { tid, result } });
    } catch (err) {
      post({ toolResult: { tid, error: describe(err) } });
    } finally {
      running.delete("tool:" + tid);
    }
  }

  async function runFetch({ fid, method, url, headers, body, redirect }) {
    const ac = new AbortController();
    running.set("fetch:" + fid, ac);
    try {
      const init = { method, headers: Object.fromEntries(headers), signal: ac.signal };
      if (body) init.body = new Uint8Array(body);
      if (redirect) init.redirect = redirect;
      const res = await pageFetch(url, init);
      // A callback may ignore init.redirect. A request that must follow no redirect fails on a redirect answer.
      if ((redirect === "manual" || redirect === "error") && (res.type === "opaqueredirect" || res.redirected || (res.status >= 300 && res.status < 400))) {
        res.body?.cancel().catch(() => {});
        throw new Error(`the page fetch answered with a redirect (${res.status}), which is not followed`);
      }
      post({ fetchHead: { fid, status: res.status, statusText: res.statusText, headers: [...res.headers] } });
      if (res.body) {
        const reader = res.body.getReader();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          const chunk = value.slice().buffer;
          post({ fetchChunk: { fid, chunk } }, [chunk]);
        }
      }
      post({ fetchEnd: { fid } });
    } catch (err) {
      post({ fetchError: { fid, ...describe(err) } });
    } finally {
      running.delete("fetch:" + fid);
    }
  }

  return {
    fs: pf?.fs,
    handle(m) {
      if (m.fsres) pf?.handle(m.fsres);
      else if (m.tool) runTool(m.tool);
      else if (m.toolAbort) running.get("tool:" + m.toolAbort.tid)?.abort();
      else if (m.fetch) runFetch(m.fetch);
      else if (m.fetchAbort) running.get("fetch:" + m.fetchAbort.fid)?.abort();
    },
    close() {
      for (const ac of running.values()) ac.abort();
      pf?.close();
    },
  };
}
