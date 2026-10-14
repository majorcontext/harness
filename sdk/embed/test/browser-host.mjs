// The browser host of the ACP scenario table: it speaks ACP on stdio, one frame
// for each line, and runs the harness module in a Worker of headless Chromium
// through the page of e2e/browser_test.go.
//
// usage: node browser-host.mjs <options.json>
// options: {origin, config}. The exit code is 3 when Chromium is not installed.
import fs from "node:fs";
import { chromium } from "playwright";

const { origin, config } = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));

let browser;
try {
  browser = await chromium.launch();
} catch (err) {
  console.error(String(err.message));
  process.exit(/Executable doesn't exist/.test(err.message) ? 3 : 1);
}
const page = await browser.newPage();
page.on("console", (m) => process.stderr.write("console: " + m.text() + "\n"));
page.on("pageerror", (err) => process.stderr.write("pageerror: " + err.message + "\n"));
page.on("worker", (w) => w.on("console", (m) => process.stderr.write("worker: " + m.text() + "\n")));
await page.exposeFunction("emitFrame", (line) => process.stdout.write(line + "\n"));
await page.goto(origin + "/app/index.html");
await page.evaluate(async (row) => (await import("/page/page.mjs")).serve(row), { config });

let sending = Promise.resolve();
const send = (line) => (sending = sending.then(() => page.evaluate((l) => globalThis.sendFrame(l), line)));
let pending = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  pending += chunk;
  for (let i = pending.indexOf("\n"); i >= 0; i = pending.indexOf("\n")) {
    send(pending.slice(0, i));
    pending = pending.slice(i + 1);
  }
});
process.stdin.on("end", async () => {
  if (pending !== "") send(pending);
  try {
    await sending;
    await page.evaluate(() => globalThis.endHarness());
  } finally {
    await Promise.race([browser.close(), new Promise((resolve) => setTimeout(resolve, 5000).unref())]);
    process.stdout.write("", () => process.exit(0));
  }
});
