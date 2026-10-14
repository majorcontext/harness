// Tests of the client API over the subprocess transport. They spawn the real
// `harness acp` against a scripted model server, and a stand-in binary for the
// failures that the real one cannot be made to show. Run with
// `node --test sdk/embed/test/*.test.mjs` after setting HARNESS_BIN and
// HARNESS_EMBEDFAKE (see support.mjs).
import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createHarness, subprocess } from "../src/index.mjs";
import { clientSuite } from "./suite.mjs";
import { acpClient, fakeModel, harnessBinary, tempDir, userConfig } from "./support.mjs";

const fake = path.join(path.dirname(fileURLToPath(import.meta.url)), "fakeharness.mjs");

// A home directory of its own keeps the config of the developer out of the run.
function realTransport() {
  return subprocess({ command: harnessBinary(), args: ["acp"], env: { HOME: tempDir(), ANTHROPIC_API_KEY: "test-key" } });
}

function harness(options) {
  return createHarness({ transport: realTransport(), ...options });
}

function fakeHarness(mode, extra = {}) {
  return subprocess({ command: process.execPath, args: [fake, "arg1"], ...extra, env: { FAKE_MODE: mode, ...extra.env } });
}

async function firstFrame(h) {
  const { value } = await h.stream.readable.getReader().read();
  return value;
}

clientSuite((options) => harness(options));

test("the first request lists the whole tool set of the binary, with bash and process", async () => {
  const model = await fakeModel([{ text: "ok" }]);
  const h = await harness({ config: userConfig(model.url) });
  try {
    const c = acpClient(h);
    await c.prompt(await c.session(), "go");
    const names = (await model.requests())[0].body.tools.map((t) => t.name);
    assert.ok(names.includes("bash") && names.includes("process"));
  } finally {
    await h.close();
    model.close();
  }
});

test("command, args, env, cwd, and config reach the process", async () => {
  const cwd = tempDir();
  const dir = tempDir();
  const h = await createHarness({
    transport: fakeHarness("report", { cwd, env: { FAKE_MARKER: "m1", HARNESS_CONFIG_FILE: "/nonexistent" } }),
    config: { model: "anthropic/claude-fable-5" },
    store: { dir },
  });
  const frame = await firstFrame(h);
  assert.equal(fs.realpathSync(frame.params.cwd), fs.realpathSync(cwd));
  assert.equal(frame.params.arg, "arg1");
  assert.equal(frame.params.marker, "m1");
  assert.equal(frame.params.sessions, dir);
  assert.deepEqual(JSON.parse(fs.readFileSync(frame.params.config, "utf8")), { model: "anthropic/claude-fable-5" });
  await h.close();
  assert.equal(fs.existsSync(frame.params.config), false);
});

test("a command that cannot start rejects createHarness with its name", async () => {
  const command = path.join(tempDir(), "no-such-harness");
  await assert.rejects(createHarness({ transport: subprocess({ command }) }), (err) => err.message.includes("could not start") && err.message.includes(command));
});

test("each line of stderr goes to log and none goes to the stream", async () => {
  const lines = [];
  const h = await createHarness({ transport: fakeHarness("noisy"), log: (line) => lines.push(line) });
  const frame = await firstFrame(h);
  assert.equal(frame.method, "ready");
  await h.close();
  assert.deepEqual(lines, ["one", "two"]);
});

test("close resolves when the process exits with status 0 at the end of its input", async () => {
  const h = await createHarness({ transport: fakeHarness("noisy") });
  await h.close();
});

test("close rejects with the exit status of a process that exits non-zero", async () => {
  const h = await createHarness({ transport: fakeHarness("eof3") });
  await assert.rejects(h.close(), /exited with code 3/);
  await assert.rejects(h.close(), /exited with code 3/);
});

test("close stops a process that ignores the end of its input and says so", async () => {
  const h = await createHarness({ transport: fakeHarness("hang", { closeTimeout: 100 }) });
  await firstFrame(h);
  await assert.rejects(h.close(), /did not exit .*SIGKILL/);
});

test("a line on stdout that is no frame ends the stream with an error and stops the process", async () => {
  const h = await createHarness({ transport: fakeHarness("garbage", { closeTimeout: 100 }) });
  await assert.rejects(h.stream.readable.getReader().read(), /not an ACP frame/);
  await h.close().catch(() => {});
});

test("a process that exits on its own ends the stream with its exit status, and close rejects with it", async () => {
  const lines = [];
  const h = await createHarness({ transport: fakeHarness("crash"), log: (line) => lines.push(line) });
  await assert.rejects(h.stream.readable.getReader().read(), /exited with code 2/);
  await assert.rejects(h.close(), /exited with code 2/);
  assert.deepEqual(lines, ["boom"]);
});

test("an option that the process cannot honor is refused by name", async () => {
  const transport = realTransport();
  const tool = { name: "t", description: "d", run: () => "" };
  await assert.rejects(createHarness({ transport, tools: [tool] }), /subprocess transport does not support tools/);
  await assert.rejects(createHarness({ transport, builtinTools: ["read_file"] }), /subprocess transport does not support builtinTools/);
  await assert.rejects(createHarness({ transport, proxy: { baseURL: "http://127.0.0.1:1" } }), /subprocess transport does not support proxy/);
  await assert.rejects(createHarness({ transport, fetch: async () => new Response("") }), /subprocess transport does not support fetch/);
});

test("a subprocess transport without a command is refused", () => {
  assert.throws(() => subprocess({}), /command/);
});
