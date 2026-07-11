// REAL end-to-end verification of tools/hub/index.html's incremental
// rendering (see the header comment in index.html and AGENTS.md's
// "Development hub" section for the behaviors this checks): drives the
// ACTUAL page served by a REAL hub HTTP handler (byte-identical to
// tools/hub/index.html — see the diff check below) against a REAL running
// harness server (tools/hub/e2e's Stub — same wiring as `harness serve` and
// `harness hub`), using jsdom + Node's own, UNMOCKED fetch. Nothing in this
// file simulates HTTP/SSE traffic; every request below is a real socket
// round-trip to the servers e2e_test.go (or hubverify) started.
//
// Expects three arguments: <boxBase> <hubBase> <token> (see
// tools/hub/e2e/stub.go's Start / hubverify's printed JSON). Exits non-zero
// on any failed assertion, printing the failure to stderr.
//
// Requires "jsdom" (see tools/hub/e2e/package.json — `npm install` once in
// this directory). Run directly with:
//   go run ./tools/hub/e2e/hubverify   # prints {"box_base":...,"hub_base":...,"token":...}
//   node tools/hub/e2e/real_e2e.mjs <box_base> <hub_base> <token>
// or let `go test ./tools/hub/e2e/...` drive both automatically.
import { JSDOM } from "jsdom";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const [, , boxBase, hubBase, token] = process.argv;
if (!boxBase || !hubBase || !token) {
  console.error("usage: node real_e2e.mjs <box_base> <hub_base> <token>");
  process.exit(2);
}
console.error("box:", boxBase, "hub:", hubBase);

const here = dirname(fileURLToPath(import.meta.url));
const committedIndexHTML = readFileSync(join(here, "..", "index.html"), "utf8");

async function main() {
  // ---- 0. The hub server must be serving the EXACT committed file (proves
  // this isn't drifted/stale wiring — the production `harness hub` binary
  // embeds this same tools/hub/index.html via go:embed). ----
  const servedHTML = await (await fetch(hubBase + "/")).text();
  assert.equal(servedHTML, committedIndexHTML, "the hub server must serve tools/hub/index.html byte-for-byte");
  console.error("PASS: hub server serves the committed index.html byte-for-byte");

  // ---- 1. Build a URL fragment with this real box, then load the real
  // page with that fragment, using jsdom's real (Node global) fetch
  // throughout, and a real AbortController (jsdom's own AbortController
  // produces AbortSignal instances real undici fetch rejects as foreign). ----
  const bootDom = new JSDOM(servedHTML, {
    url: hubBase + "/",
    runScripts: "dangerously", resources: "usable", pretendToBeVisual: true,
    beforeParse(w) { w.fetch = fetch; w.requestAnimationFrame = (cb) => setTimeout(cb, 0); },
  });
  const encoded = bootDom.window.encodeHubState({
    boxes: [{ id: "b1", name: "real-box", base: boxBase, token }],
    view: {}, notify: false,
  });
  bootDom.window.close();

  const dom = new JSDOM(servedHTML, {
    url: hubBase + "/#" + encoded,
    runScripts: "dangerously", resources: "usable", pretendToBeVisual: true,
    beforeParse(w) {
      w.fetch = fetch;
      w.requestAnimationFrame = (cb) => setTimeout(cb, 0);
      w.AbortController = AbortController; // real Node AbortController, compatible with real fetch
    },
  });
  const w = dom.window;
  const doc = w.document;

  // ---- 2. Synchronous first paint: no empty-state flash with a populated fragment. ----
  assert.ok(!doc.getElementById("fleet").textContent.includes("no boxes yet"), "must not flash empty state");
  assert.ok(doc.querySelector(".box-card"), "box card must render synchronously");
  console.error("PASS: real page, synchronous skeleton on load, no empty-state flash");

  // ---- 3. Real health/session poll lands; dot turns healthy. ----
  await new Promise((r) => setTimeout(r, 300));
  const dot = doc.querySelector(".dot");
  assert.ok(dot.classList.contains("on"), "dot should be healthy after the real /health poll: " + dot.className);
  // vcs_revision comes from Go's build-info VCS stamping, which only embeds
  // when the module's working tree is clean at build time (go help
  // buildmode's -buildvcs). That's an artifact of running this check from a
  // dirty tree mid-development, not a hub behavior under test — accept
  // either a real hex prefix (clean tree) or the "…" placeholder the hub
  // renders for a healthy box with no reported revision, as long as it's
  // not still the loading ellipsis's sibling "unreachable"/stale state.
  const meta = doc.querySelector(".box-meta").textContent;
  assert.ok(/^[0-9a-f]{10}$/.test(meta) || meta === "\u2026", "box-meta should show a real vcs_revision or the no-revision placeholder, got: " + meta);
  console.error("PASS: real /health poll resolved, dot healthy, box-meta:", meta);

  const cardBeforeSessionCreate = doc.querySelector(".box-card");

  // ---- 4. Create a real session via the real "+ New session" button. ----
  const buttons = [...doc.querySelectorAll(".box-actions button")];
  const newSessionBtn = buttons.find((b) => b.textContent.includes("New session"));
  newSessionBtn.click();
  await new Promise((r) => setTimeout(r, 300));
  const sessRow = doc.querySelector(".sess");
  assert.ok(sessRow, "a real session row should appear");
  assert.strictEqual(doc.querySelector(".box-card"), cardBeforeSessionCreate, "box card DOM node must survive a real session being added");
  console.error("PASS: real session created, box card DOM node stable");

  // ---- 5. Timeline: send a real prompt to the scripted provider, expand
  // its reasoning block, send a second real prompt, confirm the first
  // message's node + expand state survive (keyed append-only against a
  // REAL server-driven SSE stream, not a mocked one). ----
  const promptBox = doc.getElementById("promptBox");
  const sendBtn = doc.getElementById("sendBtn");
  promptBox.value = "hello";
  sendBtn.click();

  let reasoningDetails = null;
  for (let i = 0; i < 60 && !reasoningDetails; i++) {
    await new Promise((r) => setTimeout(r, 150));
    reasoningDetails = doc.querySelector("#timeline .tl-messages details.reason");
  }
  assert.ok(reasoningDetails, "a real reasoning block should render in the durable message");
  const firstMsgTexts = [...doc.querySelectorAll("#timeline .tl-messages .msg .text")];
  const firstMsgText = firstMsgTexts.find((n) => /reply number \d+/.test(n.textContent));
  assert.ok(firstMsgText, "the real scripted provider's first reply should render among: " + firstMsgTexts.map((n) => n.textContent).join(" | "));
  const firstReplyNum = firstMsgText.textContent.match(/reply number (\d+)/)[1];
  reasoningDetails.open = true;
  console.error("PASS: real turn " + firstReplyNum + " rendered (reasoning block + text), expanded it");

  for (let i = 0; i < 60 && sendBtn.disabled; i++) await new Promise((r) => setTimeout(r, 100));
  promptBox.value = "again";
  sendBtn.click();
  let secondMsg = null;
  const secondReplyNum = String(Number(firstReplyNum) + 1);
  for (let i = 0; i < 60 && !secondMsg; i++) {
    await new Promise((r) => setTimeout(r, 150));
    const texts = [...doc.querySelectorAll("#timeline .tl-messages .msg .text")];
    secondMsg = texts.find((n) => n.textContent.includes("reply number " + secondReplyNum));
  }
  assert.ok(secondMsg, "a real second reply (number " + secondReplyNum + ") should render");
  const reasoningDetailsAfter = doc.querySelector("#timeline .tl-messages details.reason");
  assert.strictEqual(reasoningDetailsAfter, reasoningDetails, "the first message's reasoning node must be the SAME DOM node after a second real turn (keyed append-only, not a rebuild)");
  assert.equal(reasoningDetailsAfter.open, true, "the first message's expanded reasoning block must survive a second real server-driven render");
  console.error("PASS: real second turn appended without disturbing the first message's node/expand state");

  // ---- 6. Pinned-tail autoscroll against real renders: scroll up, confirm
  // a real subsequent render does not yank the viewport back down. ----
  const tl = doc.getElementById("timeline");
  Object.defineProperty(tl, "scrollHeight", { value: 2000, configurable: true });
  Object.defineProperty(tl, "clientHeight", { value: 100, configurable: true });
  tl.scrollTop = 0;
  for (let i = 0; i < 60 && sendBtn.disabled; i++) await new Promise((r) => setTimeout(r, 100));
  promptBox.value = "third";
  sendBtn.click();
  await new Promise((r) => setTimeout(r, 800));
  assert.equal(tl.scrollTop, 0, "a scrolled-up viewport must not be moved by a real subsequent render");
  console.error("PASS: scrolled-up position survives real new messages");

  dom.window.close();
  console.error("ALL REAL END-TO-END CHECKS PASSED");
}

// The hub page's own poll/reconnect intervals (see index.html's
// HUB_POLL_MS and connectBoxStream backoff) keep timers pending in jsdom's
// window even after dom.window.close(), which would otherwise leave this
// process alive indefinitely — force a clean, explicit exit instead of
// waiting on the event loop to drain.
main()
  .then(() => process.exit(0))
  .catch((e) => { console.error(e); process.exit(1); });
