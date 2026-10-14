// The subprocess transport: createHarness spawns `harness acp` and speaks ACP
// over its stdio, one JSON frame on each line. Node only.
import { acpStream } from "./stream.mjs";

const isNode = typeof globalThis.process?.versions?.node === "string" && typeof globalThis.WorkerGlobalScope === "undefined";

const supports = Object.freeze({ tools: false, builtinTools: false, proxy: false, fetch: false });

/**
 * subprocess names the native transport: command with args is `harness acp`.
 * env is added to the environment of this process, cwd is the work directory,
 * and closeTimeout is the number of milliseconds that close() waits for the
 * process to exit after each step: the end of its input, SIGTERM, SIGKILL.
 * Page tools, builtinTools, proxy, and fetch are refused: the binary has its
 * own tools, and the page cannot answer for it.
 */
export function subprocess({ command, args = [], env = {}, cwd, closeTimeout = 5000 } = {}) {
  if (typeof command !== "string" || command === "") throw new Error("subprocess needs a command");
  return { kind: "subprocess", command, args, env, cwd, closeTimeout, supports, defaultTools: undefined, start: startSubprocess };
}

function describeExit({ code, signal }) {
  return signal ? `harness was killed by signal ${signal}` : `harness exited with code ${code}`;
}

// lines calls onLine with each complete line of a stream and with a last line that has no newline.
function lines(stream, onLine) {
  stream.setEncoding("utf8");
  let rest = "";
  stream.on("data", (chunk) => {
    rest += chunk;
    for (let i = rest.indexOf("\n"); i >= 0; i = rest.indexOf("\n")) {
      onLine(rest.slice(0, i));
      rest = rest.slice(i + 1);
    }
  });
  stream.on("end", () => rest && onLine(rest));
}

// within resolves with the value of promise, or with undefined after ms.
function within(promise, ms) {
  let timer;
  const timeout = new Promise((resolve) => (timer = setTimeout(resolve, ms)));
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

async function startSubprocess(transport, { config, store, log }) {
  if (!isNode) throw new Error("the subprocess transport runs under Node only");
  const [{ spawn }, fs, os, path] = await Promise.all([import("node:child_process"), import("node:fs/promises"), import("node:os"), import("node:path")]);
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "harness-embed-"));
  let removed;
  const removeDir = () => (removed ??= fs.rm(dir, { recursive: true, force: true }));
  const configPath = path.join(dir, "config.json");
  const env = { ...process.env, ...transport.env, HARNESS_CONFIG_FILE: configPath };
  if (store.dir) env.HARNESS_SESSION_DIR = store.dir;
  let child;
  try {
    await fs.writeFile(configPath, JSON.stringify(config), { mode: 0o600 });
    child = spawn(transport.command, transport.args, { cwd: transport.cwd, env, stdio: ["pipe", "pipe", "pipe"] });
    await new Promise((resolve, reject) => {
      child.once("spawn", resolve);
      child.once("error", reject);
    });
  } catch (err) {
    await removeDir();
    throw new Error(`could not start ${transport.command}: ${err.message}`, { cause: err });
  }
  child.removeAllListeners("error");
  child.on("error", () => {});
  child.stdin.on("error", () => {});

  let closing;
  let failed = false;
  let exit;
  const exited = new Promise((resolve) => child.once("close", (code, signal) => resolve((exit = { code, signal }))));
  const stream = acpStream({
    send(message) {
      if (closing || failed) throw new Error("the harness is closed");
      child.stdin.write(JSON.stringify(message) + "\n");
    },
    close: () => close(),
  });
  // fail ends the stream with err and stops the process. close() reports how it stopped.
  const fail = (err) => {
    if (failed) return;
    failed = true;
    stream.end(err);
    close().catch(() => {});
  };
  lines(child.stdout, (line) => {
    if (failed || line.trim() === "") return;
    let frame;
    try {
      frame = JSON.parse(line);
    } catch {
      // The line is reported below.
    }
    if (frame === null || typeof frame !== "object" || Array.isArray(frame)) {
      fail(new Error(`harness wrote a line to stdout that is not an ACP frame: ${line.slice(0, 80)}`));
      return;
    }
    stream.emit(frame);
  });
  lines(child.stderr, (line) => log?.(line));
  exited.then((info) => {
    if (closing) return;
    failed = true;
    stream.end(new Error(describeExit(info)));
    removeDir();
  });

  function close() {
    return (closing ??= (async () => {
      if (failed && exit) {
        await removeDir();
        throw new Error(describeExit(exit));
      }
      child.stdin.end();
      const sent = [];
      let info = await within(exited, transport.closeTimeout);
      for (const signal of ["SIGTERM", "SIGKILL"]) {
        if (info) break;
        sent.push(signal);
        child.kill(signal);
        info = await within(exited, transport.closeTimeout);
      }
      if (!info) {
        // A grandchild can hold the pipes after the process is dead.
        child.stdin.destroy();
        child.stdout.destroy();
        child.stderr.destroy();
        info = (await within(exited, transport.closeTimeout)) ?? { code: null, signal: "SIGKILL" };
      }
      await removeDir();
      stream.end();
      if (sent.length) throw new Error(`harness did not exit ${transport.closeTimeout} ms after its input closed; sent ${sent.join(" then ")}`);
      if (info.code !== 0 || info.signal) throw new Error(describeExit(info));
    })());
  }
  return { stream: { readable: stream.readable, writable: stream.writable }, close };
}
