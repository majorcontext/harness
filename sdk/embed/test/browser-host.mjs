// The browser host of the ACP scenario table: it speaks ACP on stdio, one frame
// for each line, and runs the harness module in a Worker of headless Chromium
// through the page of e2e/browser_test.go.
//
// usage: node browser-host.mjs <options.json>
// options: {origin, config, wsEndpoint}. The host connects to the shared Chromium
// of browser-server.mjs and works in a context of its own, which it closes at the
// end: a row shares no storage, OPFS, or Web Lock with another row.
import fs from "node:fs";
import { chromium } from "playwright";

const { origin, config, wsEndpoint } = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));

let browser;
try {
  browser = await chromium.connect(wsEndpoint);
} catch (err) {
  console.error(String(err.message));
  process.exit(1);
}
const context = await browser.newContext();
const page = await context.newPage();
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
  let code = 0;
  try {
    await sending;
    await page.evaluate(() => globalThis.endHarness());
  } catch (err) {
    process.stderr.write("host failed: " + String(err?.stack ?? err) + "\n");
    code = 1;
  }
  await Promise.race([context.close().then(() => browser.close()), new Promise((resolve) => setTimeout(resolve, 5000).unref())]);
  process.stdout.write("", () => process.exit(code));
});
