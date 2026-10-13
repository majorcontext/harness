// The message ports of the page and of the Worker, for a browser Worker and
// for a Node worker_threads Worker. Both ends send {frame}, {tool}, {fetch},
// and the other messages of worker.mjs and bridge.mjs.

const inNode = typeof globalThis.process?.versions?.node === "string";
const inBrowserWorker = typeof globalThis.WorkerGlobalScope !== "undefined";

/** spawnWorker starts worker.mjs and returns the page end of its port. */
export async function spawnWorker() {
  const url = new URL("./worker.mjs", import.meta.url);
  if (inNode && !inBrowserWorker) {
    const { Worker } = await import("node:worker_threads");
    const w = new Worker(url);
    let ended = false;
    return {
      post: (message, transfer) => w.postMessage(message, transfer),
      onMessage: (fn) => w.on("message", fn),
      onFailure(fn) {
        w.on("error", fn);
        w.on("exit", (code) => ended || fn(new Error(`the harness worker exited with code ${code}`)));
      },
      terminate() {
        ended = true;
        return w.terminate();
      },
    };
  }
  const w = new Worker(url, { type: "module" });
  return {
    post: (message, transfer) => w.postMessage(message, transfer),
    onMessage: (fn) => w.addEventListener("message", (e) => fn(e.data)),
    onFailure(fn) {
      w.addEventListener("error", (e) => fn(new Error(e.message || "the harness worker failed")));
      w.addEventListener("messageerror", () => fn(new Error("the harness worker sent a message that cannot be read")));
    },
    terminate: async () => w.terminate(),
  };
}

/** workerPort returns the Worker end of the port. */
export async function workerPort() {
  if (inNode && !inBrowserWorker) {
    const { parentPort } = await import("node:worker_threads");
    return {
      post: (message, transfer) => parentPort.postMessage(message, transfer),
      onMessage: (fn) => parentPort.on("message", fn),
    };
  }
  return {
    post: (message, transfer) => globalThis.postMessage(message, transfer),
    onMessage: (fn) => globalThis.addEventListener("message", (e) => fn(e.data)),
  };
}
