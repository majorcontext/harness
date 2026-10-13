// The Playwright fixture of e2e/browser_test.go. It starts headless Chromium,
// opens the page that the Go test serves, runs every row in a context of its
// own, and prints one JSON object {rows: {name: result}} on stdout.
//
// usage: node browser.mjs <options.json>
// options: {origin, rows: [{name, kind, ...}]}. A row with throttle runs with
// the network of a 50 Mbps link and 40 ms round trip. A row page can open more
// pages in its context, its tabs, through the tab function: open returns an id,
// run evaluates a tab operation of page.mjs in the tab, reload and close act on it.
import fs from "node:fs";
import { promisify } from "node:util";
import zlib from "node:zlib";
import { chromium } from "playwright";

const { origin, rows, modulePath } = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));

async function runRow(browser, row) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const console_ = [];
  page.on("console", (m) => console_.push(m.text()));
  page.on("pageerror", (err) => console_.push("pageerror: " + err.message));
  page.on("worker", (w) => w.on("console", (m) => console_.push("worker: " + m.text())));
  let offline = false;
  let offlineFailures = 0;
  await page.exposeFunction("goOffline", async () => {
    offline = true;
    await context.setOffline(true);
  });
  context.on("requestfailed", async () => {
    if (!offline) return;
    offlineFailures++;
    offline = false;
    await context.setOffline(false);
  });
  const tabs = new Map();
  const t0 = Date.now();
  await context.exposeFunction("tab", async (op, id, arg) => {
    const done = (v) => (console_.push(`tab ${op} ${id ?? ""} ${arg?.name ?? ""} ended at ${Date.now() - t0} ms`), v);
    console_.push(`tab ${op} ${id ?? ""} ${arg?.name ?? ""} began at ${Date.now() - t0} ms`);
    if (op === "open") {
      const tabPage = await context.newPage();
      tabPage.on("console", (m) => console_.push("tab: " + m.text()));
      tabPage.on("pageerror", (err) => console_.push("tab pageerror: " + err.message));
      tabPage.on("worker", (w) => w.on("console", (m) => console_.push("tab worker: " + m.text())));
      await tabPage.goto(origin + "/app/index.html");
      tabs.set(tabs.size + 1, tabPage);
      return done(tabs.size);
    }
    const tabPage = tabs.get(id);
    if (op === "run") return done(await tabPage.evaluate(async ({ name, args }) => (await import("/page/page.mjs")).tabOps[name](args), arg));
    if (op === "reload") await tabPage.reload();
    if (op === "close") await tabPage.close();
    return done(null);
  });
  if (row.throttle) {
    const cdp = await context.newCDPSession(page);
    await cdp.send("Network.enable");
    await cdp.send("Network.emulateNetworkConditions", {
      offline: false,
      latency: 40,
      downloadThroughput: (50 * 1024 * 1024) / 8,
      uploadThroughput: (50 * 1024 * 1024) / 8,
    });
  }
  await page.goto(origin + "/app/index.html");
  let timer;
  const deadline = new Promise((resolve) => (timer = setTimeout(() => resolve({ failure: "the row did not finish in 30 s" }), 30000)));
  const result = await Promise.race([page.evaluate(async (r) => (await import("/page/page.mjs")).runRow(r), row), deadline]);
  clearTimeout(timer);
  await Promise.race([context.close(), new Promise((resolve) => setTimeout(resolve, 5000).unref())]);
  const out = { ...result, offlineFailures, console: console_ };
  if (row.throttle && modulePath) out.brotliBytes = (await promisify(zlib.brotliCompress)(fs.readFileSync(modulePath))).length;
  return out;
}

let browser;
try {
  browser = await chromium.launch();
} catch (err) {
  console.error(String(err.message));
  process.exit(/Executable doesn't exist/.test(err.message) ? 3 : 1);
}
let output = "";
try {
  const results = await Promise.all(rows.map((row) => runRow(browser, row)));
  const out = {};
  rows.forEach((row, i) => (out[row.name] = results[i]));
  output = JSON.stringify({ rows: out });
} finally {
  await Promise.race([browser.close(), new Promise((resolve) => setTimeout(resolve, 5000).unref())]);
}
process.stdout.write(output, () => process.exit(0));
