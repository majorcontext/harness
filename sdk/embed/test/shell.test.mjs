// Tests of the shell export that need no module and no browser: the adapter
// from the file system handle to the file system of just-bash, and the refusal
// of a harness without a handle. The browser rows run the shell tool itself.
import { test } from "node:test";
import assert from "node:assert/strict";
import { shellFs } from "../src/shellfs.mjs";
import { shellTool } from "@majorcontext/harness-embed/shell";

function codeError(code, path) {
  return Object.assign(new Error(`${code}: ${path}`), { code });
}

// memoryFs is a HarnessFs over a Map: a path maps to bytes, or to null for a directory.
function memoryFs() {
  const nodes = new Map([["/", null], ["/work", null]]);
  const parent = (p) => p.slice(0, p.lastIndexOf("/")) || "/";
  const need = (p) => {
    if (!nodes.has(p)) throw codeError("ENOENT", p);
    return nodes.get(p);
  };
  return {
    async stat(p) {
      const v = need(p);
      return { isDirectory: v === null, size: v?.length ?? 0, mtimeMs: 0 };
    },
    async readdir(p) {
      if (need(p) !== null) throw codeError("ENOTDIR", p);
      return [...nodes.keys()].filter((k) => k !== p && parent(k) === p).map((k) => k.slice(k.lastIndexOf("/") + 1));
    },
    async readFile(p) {
      const v = need(p);
      if (v === null) throw codeError("EISDIR", p);
      return v;
    },
    async writeFile(p, data, { append } = {}) {
      need(parent(p));
      const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
      const old = append ? (nodes.get(p) ?? new Uint8Array()) : new Uint8Array();
      nodes.set(p, Uint8Array.from([...old, ...bytes]));
    },
    async mkdir(p, { recursive } = {}) {
      if (recursive) {
        let cur = "";
        for (const part of p.split("/").filter(Boolean)) nodes.set((cur += "/" + part), null);
      } else {
        need(parent(p));
        nodes.set(p, null);
      }
    },
    async rm(p) {
      need(p);
      for (const k of [...nodes.keys()]) if (k === p || k.startsWith(p + "/")) nodes.delete(k);
    },
    async rename(a, b) {
      nodes.set(b, need(a));
      nodes.delete(a);
    },
  };
}

test("the adapter maps a mount point to a directory of the module", async () => {
  const fs = memoryFs();
  await fs.writeFile("/work/a.txt", "hello\n");
  const mounted = shellFs(fs, "/work");
  assert.equal(await mounted.readFile("/a.txt"), "hello\n");
  await mounted.writeFile("/b.txt", "world\n");
  assert.equal(new TextDecoder().decode(await fs.readFile("/work/b.txt")), "world\n");
  assert.deepEqual((await mounted.readdir("/")).sort(), ["a.txt", "b.txt"]);
  await mounted.mkdir("/x/y", { recursive: true });
  assert.equal((await fs.stat("/work/x/y")).isDirectory, true);
  await mounted.mv("/a.txt", "/x/a.txt");
  assert.equal(await mounted.exists("/a.txt"), false);
  assert.equal(await mounted.readFile("/x/a.txt"), "hello\n");
});

test("the adapter keeps the error code of the module", async () => {
  const mounted = shellFs(memoryFs(), "/work");
  await assert.rejects(mounted.readFile("/missing"), (err) => err.code === "ENOENT");
  await assert.rejects(mounted.symlink("/a", "/b"), (err) => err.code === "ENOSYS");
});

test("the shell tool refuses a harness without a file system handle", async () => {
  const tool = shellTool();
  assert.equal(tool.name, "bash");
  await assert.rejects(tool.run({ command: "ls" }, { signal: new AbortController().signal }), /browser/);
});

test("the shell tool takes another name", () => {
  assert.equal(shellTool({ name: "shell" }).name, "shell");
});
