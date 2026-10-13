// The page end of the tool bridge and of the fetch bridge.
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

/**
 * pageBridge answers the tool and fetch messages of the Worker. post sends a
 * message to the Worker. handle takes one message; close aborts what runs.
 */
export function pageBridge({ tools, pageFetch, post }) {
  const byName = new Map(tools.map((t) => [t.name, t]));
  const running = new Map();

  async function runTool({ tid, name, args }) {
    const ac = new AbortController();
    running.set("tool:" + tid, ac);
    try {
      const tool = byName.get(name);
      if (!tool) throw new Error(`unknown tool ${name}`);
      const result = toolResult(await tool.run(args, { signal: ac.signal }));
      post({ toolResult: { tid, result } });
    } catch (err) {
      post({ toolResult: { tid, error: describe(err) } });
    } finally {
      running.delete("tool:" + tid);
    }
  }

  async function runFetch({ fid, method, url, headers, body }) {
    const ac = new AbortController();
    running.set("fetch:" + fid, ac);
    try {
      const init = { method, headers: Object.fromEntries(headers), signal: ac.signal };
      if (body) init.body = new Uint8Array(body);
      const res = await pageFetch(url, init);
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
    handle(m) {
      if (m.tool) runTool(m.tool);
      else if (m.toolAbort) running.get("tool:" + m.toolAbort.tid)?.abort();
      else if (m.fetch) runFetch(m.fetch);
      else if (m.fetchAbort) running.get("fetch:" + m.fetchAbort.fid)?.abort();
    },
    close() {
      for (const ac of running.values()) ac.abort();
    },
  };
}
