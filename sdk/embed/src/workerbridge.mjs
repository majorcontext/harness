// The Worker end of the tool bridge, of the fetch bridge, and of the file bridge (see bridge.mjs).

const noBody = new Set([101, 204, 205, 304]);

function abortError() {
  return new DOMException("The operation was aborted.", "AbortError");
}

function pageError({ name, message }) {
  const err = name === "TypeError" ? new TypeError(message) : new Error(message);
  err.name = name;
  return err;
}

// call runs a callback style function of globalThis.fs, the file system that Go uses.
const call = (fn, ...args) => new Promise((resolve, reject) => fn.call(globalThis.fs, ...args, (err, value) => (err ? reject(err) : resolve(value))));

async function readWhole(path) {
  const fs = globalThis.fs;
  const fd = await call(fs.open, path, 0, 0);
  try {
    const info = await call(fs.fstat, fd);
    if (info.isDirectory()) throw Object.assign(new Error(`EISDIR: illegal operation on a directory, read '${path}'`), { code: "EISDIR" });
    const size = info.size;
    const bytes = new Uint8Array(size);
    let offset = 0;
    while (offset < size) {
      const n = await call(fs.read, fd, bytes, offset, size - offset, offset);
      if (!n) break;
      offset += n;
    }
    return bytes.slice(0, offset).buffer;
  } finally {
    await call(fs.close, fd);
  }
}

async function writeWhole(path, buffer, append) {
  const fs = globalThis.fs;
  const flags = fs.constants.O_WRONLY | fs.constants.O_CREAT | (append ? fs.constants.O_APPEND : fs.constants.O_TRUNC);
  const fd = await call(fs.open, path, flags, 0o644);
  try {
    const bytes = new Uint8Array(buffer);
    let offset = 0;
    while (offset < bytes.length) offset += await call(fs.write, fd, bytes, offset, bytes.length - offset, null);
  } finally {
    await call(fs.close, fd);
  }
}

const fileOps = {
  async stat(path) {
    const s = await call(globalThis.fs.stat, path);
    return { isDirectory: s.isDirectory(), size: s.size, mtimeMs: s.mtimeMs };
  },
  readdir: (path) => call(globalThis.fs.readdir, path),
  mkdir: (path) => call(globalThis.fs.mkdir, path, 0o755),
  unlink: (path) => call(globalThis.fs.unlink, path),
  rmdir: (path) => call(globalThis.fs.rmdir, path),
  rename: (from, to) => call(globalThis.fs.rename, from, to),
  readFile: readWhole,
  writeFile: writeWhole,
};

/**
 * workerBridge returns callTool, which runs a page tool; fetch, which stands in
 * for the global fetch and runs the fetch of the page; and handle, which takes
 * the answers of the page. post sends a message to the page.
 */
export function workerBridge(post) {
  let next = 0;
  const tools = new Map();
  const fetches = new Map();

  function callTool(name, args, signal) {
    const tid = ++next;
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason ?? abortError());
        return;
      }
      tools.set(tid, { resolve, reject });
      signal?.addEventListener(
        "abort",
        () => {
          if (tools.delete(tid)) {
            post({ toolAbort: { tid } });
            reject(signal.reason ?? abortError());
          }
        },
        { once: true },
      );
      post({ tool: { tid, name, args } });
    });
  }

  function bridgedFetch(input, init = {}) {
    const fid = ++next;
    const signal = init.signal;
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason ?? abortError());
        return;
      }
      const entry = { resolve, reject, controller: undefined };
      fetches.set(fid, entry);
      signal?.addEventListener(
        "abort",
        () => {
          if (!fetches.delete(fid)) return;
          post({ fetchAbort: { fid } });
          const reason = signal.reason ?? abortError();
          if (entry.controller) entry.controller.error(reason);
          else reject(reason);
        },
        { once: true },
      );
      const message = { fid, method: init.method ?? "GET", url: String(input), headers: [...new Headers(init.headers)] };
      if (init.redirect) message.redirect = init.redirect;
      const transfer = [];
      if (init.body) {
        message.body = new Uint8Array(init.body).slice().buffer;
        transfer.push(message.body);
      }
      post({ fetch: message }, transfer);
    });
  }

  async function serveFile({ id, op, args }) {
    try {
      const result = await fileOps[op](...args);
      post({ fsres: { id, result } }, result instanceof ArrayBuffer ? [result] : []);
    } catch (err) {
      post({ fsres: { id, error: { code: err?.code, message: String(err?.message ?? err) } } });
    }
  }

  function handle(m) {
    if (m.fsop) {
      serveFile(m.fsop);
    } else if (m.toolResult) {
      const { tid, result, error } = m.toolResult;
      const entry = tools.get(tid);
      tools.delete(tid);
      if (error) entry?.reject(pageError(error));
      else entry?.resolve(result);
    } else if (m.fetchHead) {
      const { fid, status, statusText, headers } = m.fetchHead;
      const entry = fetches.get(fid);
      if (!entry) return;
      const body = noBody.has(status)
        ? null
        : new ReadableStream({
            start: (controller) => (entry.controller = controller),
            cancel() {
              if (fetches.delete(fid)) post({ fetchAbort: { fid } });
            },
          });
      entry.resolve(new Response(body, { status, statusText, headers }));
      if (!body) fetches.delete(fid);
    } else if (m.fetchChunk) {
      fetches.get(m.fetchChunk.fid)?.controller?.enqueue(new Uint8Array(m.fetchChunk.chunk));
    } else if (m.fetchEnd) {
      const entry = fetches.get(m.fetchEnd.fid);
      fetches.delete(m.fetchEnd.fid);
      entry?.controller?.close();
    } else if (m.fetchError) {
      const entry = fetches.get(m.fetchError.fid);
      fetches.delete(m.fetchError.fid);
      if (!entry) return;
      const err = pageError(m.fetchError);
      if (entry.controller) entry.controller.error(err);
      else entry.reject(err);
    }
  }

  return { callTool, fetch: bridgedFetch, handle };
}
