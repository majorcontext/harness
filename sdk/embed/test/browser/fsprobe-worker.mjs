// A Worker of the browser job that runs on the file system of sdk/embed without
// the harness module. mode "go" runs e2e/testdata/fsprobe (a Go program) with
// the real wasm_exec.js and posts its lines. mode "kill" writes a pattern, makes
// the first part durable with fsync, writes more without fsync, and posts when
// the page may end the Worker.
import { installBrowserFs } from "/embed/browserfs.mjs";
import { toError } from "/embed/opfsfs.mjs";

const pattern = (from, to) => Uint8Array.from({ length: to - from }, (_, i) => (from + i) % 251);

function call(name, ...args) {
  return new Promise((resolve) => globalThis.fs[name](...args, (err, value) => resolve({ err, value })));
}

async function goProbe({ wasm, wasmExec, args }) {
  const lines = [];
  const log = (line) => lines.push(line);
  console.log = log;
  await installBrowserFs({ persist: true });
  const adapter = (name, ok, detail = "") => log(ok ? `ok ${name}` : `FAIL ${name}: ${detail}`);
  adapter("an unknown error is EIO", toError(new Error("x")).code === "EIO");
  adapter("a quota error is ENOSPC", toError(new DOMException("x", "QuotaExceededError")).code === "ENOSPC");
  adapter("a missing entry is ENOENT", toError(new DOMException("x", "NotFoundError")).code === "ENOENT");
  const bad = await call("open", "/work/nope/x", 0, 0);
  adapter("a callback answers an error once and never throws", bad.err?.code === "ENOENT", String(bad.err?.code));
  const std = await call("fstat", 1);
  adapter("fstat of fd 1 answers a character device", (std.value.mode & 0o170000) === 0o020000, String(std.value?.mode));
  await import(wasmExec);
  const go = new Go();
  go.argv = ["probe", ...args];
  go.env = {};
  const { instance } = await WebAssembly.instantiate(await (await fetch(wasm)).arrayBuffer(), go.importObject);
  await go.run(instance);
  return { lines };
}

async function killWorker() {
  await installBrowserFs({ persist: true });
  const open = await call("open", "/persist/killtest", 1 | 64 | 1024, 0o644);
  const fd = open.value;
  const first = pattern(0, 100);
  await call("write", fd, first, 0, first.length, null);
  await call("fsync", fd);
  const rest = pattern(100, 600_000);
  await call("write", fd, rest, 0, rest.length, null);
  return { durable: first.length };
}

onmessage = async (e) => {
  try {
    postMessage(e.data.mode === "kill" ? await killWorker() : await goProbe(e.data));
  } catch (err) {
    postMessage({ failure: String(err?.stack ?? err) });
  }
};
