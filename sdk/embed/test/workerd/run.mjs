// Runs worker.mjs under workerd (the npm package, pinned in package.json) and prints the JSON answer of one request.
//
// usage: node run.mjs <module.wasm> <wasm_exec.js> <model base URL> <model key>
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "esbuild";
import workerd from "workerd";

const [wasmPath, wasmExecPath, modelURL, modelKey] = process.argv.slice(2);
const here = path.dirname(fileURLToPath(import.meta.url));
const dir = fs.mkdtempSync(path.join(os.tmpdir(), "harness-workerd-"));

await build({
  entryPoints: [path.join(here, "worker.mjs")],
  outfile: path.join(dir, "worker.mjs"),
  bundle: true,
  format: "esm",
  platform: "neutral",
  external: ["./harness.wasm", "./wasm_exec.js", "node:*"],
  logLevel: "warning",
});
fs.copyFileSync(wasmPath, path.join(dir, "harness.wasm"));
fs.copyFileSync(wasmExecPath, path.join(dir, "wasm_exec.js"));

const port = await new Promise((resolve) => {
  const s = net.createServer().listen(0, "127.0.0.1", () => {
    const { port } = s.address();
    s.close(() => resolve(port));
  });
});
const text = (s) => JSON.stringify(s);
fs.writeFileSync(
  path.join(dir, "config.capnp"),
  `using Workerd = import "/workerd/workerd.capnp";
const config :Workerd.Config = (
  services = [
    (name = "main", worker = (
      compatibilityDate = "${workerd.compatibilityDate}",
      modules = [
        (name = "worker.mjs", esModule = embed "worker.mjs"),
        (name = "harness.wasm", wasm = embed "harness.wasm"),
        (name = "wasm_exec.js", esModule = embed "wasm_exec.js"),
      ],
      bindings = [
        (name = "MODEL_URL", text = ${text(modelURL)}),
        (name = "MODEL_KEY", text = ${text(modelKey)}),
      ],
      globalOutbound = "internet",
    )),
    (name = "internet", network = (allow = ["public", "private", "local"])),
  ],
  sockets = [(name = "http", address = "127.0.0.1:${port}", http = (), service = "main")],
);
`,
);

const child = spawn(workerd.default, ["serve", path.join(dir, "config.capnp")], { cwd: dir, stdio: ["ignore", "inherit", "inherit"] });
const exited = new Promise((resolve) => child.once("exit", (code) => resolve(code)));
let failure;
try {
  const url = `http://127.0.0.1:${port}/`;
  let res;
  for (let i = 0; i < 100 && !res; i++) {
    res = await Promise.race([fetch(url).catch(() => undefined), exited.then(() => Promise.reject(new Error("workerd exited before it answered")))]);
    if (!res) await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (!res) throw new Error("workerd did not answer");
  process.stdout.write(JSON.stringify(await res.json()) + "\n");
} catch (err) {
  failure = err;
} finally {
  child.kill("SIGTERM");
  await exited;
  fs.rmSync(dir, { recursive: true, force: true });
}
if (failure) {
  console.error(failure);
  process.exit(1);
}
