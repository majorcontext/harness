import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import readline from "node:readline";
import test from "node:test";
import { acquireStoreLock } from "../src/storelock.mjs";
import { tempDir } from "./support.mjs";

const child = path.join(import.meta.dirname, "storelock-child.mjs");

function deadPid() {
  const r = spawnSync(process.execPath, ["-e", "process.stdout.write(String(process.pid))"], { encoding: "utf8" });
  return Number(r.stdout);
}

function start(dir, mode) {
  const p = spawn(process.execPath, [child, dir, mode], { stdio: ["pipe", "pipe", "inherit"] });
  const answer = new Promise((resolve) => readline.createInterface({ input: p.stdout }).once("line", resolve));
  const exited = new Promise((resolve) => p.once("exit", resolve));
  return { p, answer, exited };
}

// race starts eight processes on one stale lock at the same moment and returns
// their answers. A shared signal ends the wait, and every child is killed before
// the function returns.
async function race(mode, signal) {
  const dir = tempDir();
  fs.writeFileSync(path.join(dir, ".embed.lock"), `${deadPid()}\ndeadbeef`);
  const procs = Array.from({ length: 8 }, () => start(dir, mode));
  try {
    for (const { p } of procs) p.stdin.write("go\n");
    const timeout = new Promise((_, reject) =>
      signal.addEventListener("abort", () => reject(new Error("the children did not answer in time")), { once: true }),
    );
    const answers = await Promise.race([Promise.all(procs.map((x) => x.answer)), timeout]);
    for (const { p } of procs) p.stdin.end();
    await Promise.race([Promise.all(procs.map((x) => x.exited)), timeout]);
    return { answers, left: fs.readdirSync(dir) };
  } finally {
    for (const { p } of procs) p.kill("SIGKILL");
  }
}

const acquired = (answers) => answers.filter((a) => a === "acquired").length;

test("processes that find one stale lock cannot both replace it", async () => {
  const signal = AbortSignal.timeout(30_000);
  for (let round = 0; round < 10; round++) {
    const { answers, left } = await race("real", signal);
    assert.equal(acquired(answers), 1, answers.join("; "));
    assert.ok(answers.every((a) => a === "acquired" || /in use by process/.test(a)), answers.join("; "));
    assert.deepEqual(left, []);
  }
});

test("the race check fails a lock that replaces a stale lock without a claim", async () => {
  const { answers } = await race("naive", AbortSignal.timeout(30_000));
  assert.ok(acquired(answers) > 1, `the broken lock gave ${acquired(answers)} winners: ${answers.join("; ")}`);
});

test("a claim of a crashed process does not block the lock", () => {
  const dir = tempDir();
  const dead = deadPid();
  fs.writeFileSync(path.join(dir, ".embed.lock"), `${dead}\nstale`);
  fs.writeFileSync(path.join(dir, ".embed.lock.takeover-stale"), `${dead}\nclaimant`);
  const release = acquireStoreLock(dir);
  assert.match(fs.readFileSync(path.join(dir, ".embed.lock"), "utf8"), new RegExp(`^${process.pid}\n`));
  release();
  assert.deepEqual(fs.readdirSync(dir), []);
});

test("a live lock refuses a second process and release removes only its own file", () => {
  const dir = tempDir();
  const release = acquireStoreLock(dir);
  assert.throws(() => acquireStoreLock(dir), /in use by process/);
  fs.writeFileSync(path.join(dir, ".embed.lock"), "other");
  release();
  assert.equal(fs.readFileSync(path.join(dir, ".embed.lock"), "utf8"), "other");
});
