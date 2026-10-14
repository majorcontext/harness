// The client API tests that every transport passes. A test file calls
// clientSuite with a start function that wraps createHarness for its transport.
import { test } from "node:test";
import assert from "node:assert/strict";
import { acpClient, fakeModel, tempDir, userConfig } from "./support.mjs";

const initialize = { protocolVersion: 1, clientCapabilities: {} };

/**
 * clientSuite registers the tests of the client API: the ACP stream, the
 * cancel of a turn, the store directory, and the close semantics. start(options)
 * resolves with a Harness; options holds config and store.
 */
export function clientSuite(start) {
  test("a prompt streams the text of the model and ends the turn", async () => {
    const model = await fakeModel([{ text: "hello" }]);
    const h = await start({ config: userConfig(model.url) });
    try {
      const c = acpClient(h);
      assert.equal((await c.prompt(await c.session(), "go")).stopReason, "end_turn");
      assert.ok(c.updates.some((u) => u.sessionUpdate === "agent_message_chunk" && u.content.text === "hello"));
      assert.equal((await model.requests()).length, 1);
    } finally {
      await h.close();
      model.close();
    }
  });

  test("a cancel ends a turn that streams", async () => {
    const model = await fakeModel([{ text: "partial", block: true }]);
    const h = await start({ config: userConfig(model.url) });
    try {
      const c = acpClient(h);
      const sessionId = await c.session();
      const turn = c.prompt(sessionId, "go");
      await c.streaming();
      await c.notify("session/cancel", { sessionId });
      assert.equal((await turn).stopReason, "cancelled");
    } finally {
      await h.close();
      model.close();
    }
  });

  test("a model that fails ends the turn with an error", async () => {
    const config = { ...userConfig("http://127.0.0.1:1"), request_max_retries: 0, stream_max_retries: 0 };
    const h = await start({ config });
    try {
      const c = acpClient(h);
      const sessionId = await c.session();
      await assert.rejects(c.prompt(sessionId, "go"), (err) => err.rpc !== undefined);
    } finally {
      await h.close();
    }
  });

  test("close ends the stream and a second close settles", async () => {
    const h = await start({ config: userConfig("http://127.0.0.1:1") });
    const c = acpClient(h);
    await c.request("initialize", initialize);
    await h.close();
    await c.done;
    await h.close();
  });

  test("the harness has no file system handle under Node", async () => {
    const h = await start({ config: userConfig("http://127.0.0.1:1") });
    try {
      assert.equal(h.fs, undefined);
    } finally {
      await h.close();
    }
  });

  test("a write after close rejects", async () => {
    const h = await start({ config: userConfig("http://127.0.0.1:1") });
    const writer = h.stream.writable.getWriter();
    await h.close();
    await assert.rejects(writer.write({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} }));
  });

  test("aborting the writable closes the connection", async () => {
    const h = await start({ config: userConfig("http://127.0.0.1:1") });
    const c = acpClient(h);
    await c.request("initialize", initialize);
    await c.abortWriter(new Error("done"));
    await c.done;
    await h.close();
  });

  test("a frame after the page cancels the readable does not throw, and the connection closes", async () => {
    const model = await fakeModel([{ text: "late" }]);
    const h = await start({ config: userConfig(model.url) });
    try {
      const c = acpClient(h);
      const sessionId = await c.session();
      c.prompt(sessionId, "go").catch(() => {});
      await c.cancelReader();
      await h.close();
    } finally {
      model.close();
    }
  });

  test("session/list returns the sessions and pages with a cursor", async () => {
    const cwd = tempDir();
    const store = { persist: true, dir: tempDir() };
    const config = userConfig("http://127.0.0.1:1");
    const ids = [];
    const seed = await start({ config, store });
    try {
      const c = acpClient(seed);
      await c.request("initialize", initialize);
      for (let i = 0; i < 51; i++) ids.push((await c.request("session/new", { cwd, mcpServers: [] })).sessionId);
    } finally {
      await seed.close();
    }
    const h = await start({ config, store });
    try {
      const c = acpClient(h);
      const init = await c.request("initialize", initialize);
      assert.ok(init.agentCapabilities.sessionCapabilities.list);
      const first = await c.request("session/list", {});
      assert.deepEqual(first.sessions.map((s) => s.sessionId), ids.slice(0, 50));
      assert.ok(first.nextCursor);
      const second = await c.request("session/list", { cursor: first.nextCursor });
      assert.deepEqual(second.sessions.map((s) => s.sessionId), ids.slice(50));
      assert.equal(second.nextCursor, undefined);
    } finally {
      await h.close();
    }
  });

  test("a store directory keeps the session for the next harness", async () => {
    const dir = tempDir();
    const cwd = tempDir();
    const model = await fakeModel([{ text: "hello" }]);
    let sessionId;
    try {
      const first = await start({ config: userConfig(model.url), store: { persist: true, dir } });
      try {
        const c = acpClient(first);
        await c.request("initialize", initialize);
        ({ sessionId } = await c.request("session/new", { cwd, mcpServers: [] }));
        assert.equal((await c.prompt(sessionId, "go")).stopReason, "end_turn");
      } finally {
        await first.close();
      }
      const second = await start({ config: userConfig(model.url), store: { persist: true, dir } });
      try {
        const c = acpClient(second);
        await c.request("initialize", initialize);
        await c.request("session/resume", { sessionId, cwd, mcpServers: [] });
      } finally {
        await second.close();
      }
    } finally {
      model.close();
    }
  });
}
