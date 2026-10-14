// The shared Chromium of the ACP browser rows: it starts one headless Chromium,
// prints {"wsEndpoint": ...} on stdout, and closes the browser when stdin ends.
// Each row connects with browser-host.mjs and works in a context of its own.
//
// usage: node browser-server.mjs
// The exit code is 3 when Chromium is not installed.
import { chromium } from "playwright";

let server;
try {
  server = await chromium.launchServer();
} catch (err) {
  console.error(String(err.message));
  process.exit(/Executable doesn't exist/.test(err.message) ? 3 : 1);
}
process.stdout.write(JSON.stringify({ wsEndpoint: server.wsEndpoint() }) + "\n");
process.stdin.resume();
process.stdin.on("end", async () => {
  await server.close();
  process.exit(0);
});
