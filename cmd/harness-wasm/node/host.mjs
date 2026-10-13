// Runs the harness WebAssembly module under Node and speaks ACP on stdio:
// one frame for each line of stdin and of stdout.
//
// usage: node host.mjs <module.wasm> <wasm_exec.js> <options.json>
//
// The options file holds the harnessConnect options that Node supplies
// (configPath, which the host reads into config, storeDir, env, builtinTools),
// echoTool, which adds a JavaScript tool, and online, which sets navigator.onLine.
import fs from "node:fs";
import { createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { acquireStoreLock } from "./storelock.mjs";

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
go.exit = (code) => {
  release();
  process.exit(code);
};
const { instance } = await WebAssembly.instantiate(fs.readFileSync(wasmPath), go.importObject);
go.run(instance);

const echoTool = {
  name: "js_echo",
  description: "Returns its text.",
  schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  run: async (input) => "echo:" + input.text,
};

let config = {};
try {
  config = JSON.parse(fs.readFileSync(options.configPath, "utf8"));
} catch (err) {
  if (err.code !== "ENOENT") throw err;
}
const conn = globalThis.harnessConnect({
  config,
  storeDir: options.storeDir,
  env: options.env,
  builtinTools: options.builtinTools,
  tools: options.echoTool ? [echoTool] : undefined,
  onFrame: (frame) => process.stdout.write(frame + "\n"),
  onLog: (line) => process.stderr.write(line + "\n"),
});

let closing;
function close() {
  closing ??= conn.close().finally(() => {
    release();
    process.exit(0);
  });
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
