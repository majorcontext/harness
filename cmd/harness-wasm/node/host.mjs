// Runs the harness WebAssembly module under Node and speaks ACP on stdio:
// one frame for each line of stdin and of stdout.
//
// usage: node host.mjs <module.wasm> <wasm_exec.js> <options.json>
//
// The options file holds the harnessConnect options that Node supplies
// (configPath, which the host reads into config, storeDir, env, builtinTools),
// echoTool and jsTools, which add JavaScript tools, connect, which overrides
// harnessConnect options, cyclicConfig, and online, which sets navigator.onLine.
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import v8 from "node:v8";
import { acquireStoreLock } from "../../../sdk/embed/src/storelock.mjs";

// Node 24 can deadlock in process.exit: the main thread joins the V8 worker
// threads while a worker that compiles baseline code waits for a collection
// that only the main thread can run. No concurrent baseline compile, no worker
// that waits.
v8.setFlagsFromString("--no-concurrent-sparkplug");

const [wasmPath, execPath, optionsPath] = process.argv.slice(2);
const options = JSON.parse(fs.readFileSync(optionsPath, "utf8"));

let release = () => {};
if (options.storeDir) release = acquireStoreLock(options.storeDir);

if (options.online !== undefined) {
  Object.defineProperty(globalThis, "navigator", { value: { onLine: options.online }, configurable: true });
}
globalThis.fs = fs;
globalThis.path = path;
globalThis.crypto ??= (await import("node:crypto")).webcrypto;
createRequire(import.meta.url)(execPath);

const go = new Go();
go.argv = ["harness-wasm"];
go.env = { TMPDIR: os.tmpdir() };
function exit(code) {
  release();
  process.stdout.write("", () => process.exit(code));
}
go.exit = exit;
const { instance } = await WebAssembly.instantiate(fs.readFileSync(wasmPath), go.importObject);
go.run(instance);

const echoTool = {
  name: "js_echo",
  description: "Returns its text.",
  schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  run: async (input) => "echo:" + input.text,
};

const jsTools = {
  bash: { name: "bash", description: "Host shell.", schema: { type: "object", properties: { command: { type: "string" } }, required: ["command"] }, run: async (input) => "host:" + input.command },
  js_cyclic: { name: "js_cyclic", description: "Returns a cyclic object.", run: async () => { const o = {}; o.self = o; return o; } },
  js_reject: { name: "js_reject", description: "Rejects with a plain object.", run: async () => Promise.reject({ code: 1 }) },
};

let config = {};
try {
  config = JSON.parse(fs.readFileSync(options.configPath, "utf8"));
} catch (err) {
  if (err.code !== "ENOENT") throw err;
}
if (options.cyclicConfig) {
  config = {};
  config.self = config;
}
const conn = globalThis.harnessConnect({
  config,
  storeDir: options.storeDir,
  defaultDir: process.cwd(),
  env: options.env,
  builtinTools: options.builtinTools,
  tools: [...(options.echoTool ? [echoTool] : []), ...(options.jsTools ?? []).map((name) => jsTools[name])],
  onFrame: (frame) => process.stdout.write(frame + "\n"),
  onLog: (line) => process.stderr.write(line + "\n"),
  ...options.connect,
});

let closing;
function close() {
  closing ??= conn.close().finally(() => exit(0));
}

let pending = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  pending += chunk;
  for (let i = pending.indexOf("\n"); i >= 0; i = pending.indexOf("\n")) {
    conn.send(pending.slice(0, i));
    pending = pending.slice(i + 1);
  }
});
process.stdin.on("end", () => {
  if (pending !== "") conn.send(pending);
  close();
});
process.on("SIGINT", close);
process.on("SIGTERM", close);

process.on("SIGUSR2", () => {
  const lines = [
    "host state: closing=" + (closing !== undefined) + " pendingBytes=" + pending.length,
    "stdin: ended=" + process.stdin.readableEnded + " destroyed=" + process.stdin.destroyed,
    "active resources: " + process.getActiveResourcesInfo().join(","),
  ];
  process.stderr.write(lines.join("\n") + "\n");
});
