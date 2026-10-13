// The Playwright fixture of e2e/browser_test.go. It starts headless Chromium,
// opens the page that the Go test serves, runs every row in a context of its
// own, and prints one JSON object {rows: {name: result}} on stdout.
//
// usage: node browser.mjs <options.json>
// options: {origin, rows: [{name, kind, ...}]}. A row with throttle runs with
// the network of a 50 Mbps link and 40 ms round trip.
import fs from "node:fs";
import { chromium } from "playwright";

const { origin, rows } = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));

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
  await context.close();
  return { ...result, offlineFailures, console: console_ };
}

let browser;
try {
  browser = await chromium.launch();
} catch (err) {
  console.error(String(err.message));
  process.exit(/Executable doesn't exist/.test(err.message) ? 3 : 1);
}
try {
  const results = await Promise.all(rows.map((row) => runRow(browser, row)));
  const out = {};
  rows.forEach((row, i) => (out[row.name] = results[i]));
  process.stdout.write(JSON.stringify({ rows: out }));
} finally {
  await browser.close();
}
