// A process of storelock.test.mjs: it waits for a line on stdin, tries to take
// the store lock of argv[2], prints "acquired" or "refused", and holds the lock
// until stdin closes. With argv[3] "naive" it replaces a stale lock the way a
// lock without a claim would, after a pause that makes the race certain.
import fs from "node:fs";
import path from "node:path";
import readline from "node:readline";
import { acquireStoreLock } from "../src/storelock.mjs";

function naive(dir) {
  const file = path.join(dir, ".embed.lock");
  const stale = fs.readFileSync(file, "utf8");
  try {
    process.kill(Number.parseInt(stale, 10), 0);
    throw new Error("in use by process");
  } catch (err) {
    if (err.code !== "ESRCH") throw err;
  }
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 100);
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, `${process.pid}\nnaive`);
  fs.renameSync(tmp, file);
  return () => {};
}

const lines = readline.createInterface({ input: process.stdin });
let release;
lines.once("line", () => {
  try {
    release = process.argv[3] === "naive" ? naive(process.argv[2]) : acquireStoreLock(process.argv[2]);
    console.log("acquired");
  } catch (err) {
    console.log("refused: " + err.message);
  }
});
lines.once("close", () => release?.());
