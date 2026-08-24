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

// jsdom does not implement the HTML Popover API (showPopover/hidePopover/
// togglePopover and :popover-open). The hub's card overflow menu calls
// hidePopover() at the TOP of its item handlers (index.html's card menu),
// so without these methods the real page throws mid-handler and the item's
// real work (e.g. quickNewSession) never runs. Install minimal stubs that
// track open state via the popover-open attribute, exactly as a real browser
// would toggle it, so the served page runs unmodified. Same category of fix
// as the fetch/requestAnimationFrame/AbortController patches below: supply a
// browser capability jsdom lacks, never alter the product code to suit it.
function installPopoverPolyfill(w) {
  const proto = w.HTMLElement.prototype;
  if (typeof proto.showPopover === "function") return;
  proto.showPopover = function () { this.setAttribute("popover-open", ""); };
  proto.hidePopover = function () { this.removeAttribute("popover-open"); };
  proto.togglePopover = function (force) {
    const next = force === undefined ? !this.hasAttribute("popover-open") : !!force;
    if (next) this.showPopover(); else this.hidePopover();
    return next;
  };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// waitFor polls `fn` until it returns a truthy value, throwing with `label`
// if `timeoutMs` elapses first. Every wait in this file goes through it.
//
// A fixed `await sleep(N)` followed by a hard assertion is a guessed
// deadline: it encodes "a real HTTP round trip plus a real render finishes
// in N ms", which is false on a loaded machine. That exact shape made this
// test flaky — a 300ms sleep before the health-dot assertion failed under
// CPU pressure because the real /health round trip had not landed yet.
// Poll the CONDITION instead, with a timeout far above any real latency:
// the fast path stays fast (25ms granularity), and a slow machine waits
// longer instead of failing.
async function waitFor(fn, { timeoutMs = 15000, intervalMs = 25, label = "condition" } = {}) {
  const start = Date.now();
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() - start > timeoutMs) throw new Error("timed out waiting for: " + label);
    await sleep(intervalMs);
  }
}

// replyTextNode returns the rendered durable-message text node for the
// scripted provider's Nth reply ("reply number N"), or null.
function replyTextNode(doc, n) {
  const texts = [...doc.querySelectorAll("#timeline .tl-messages .msg .text")];
  return texts.find((node) => node.textContent.includes("reply number " + n)) || null;
}

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
      installPopoverPolyfill(w);
    },
  });
  const w = dom.window;
  const doc = w.document;

  // ---- 2. Synchronous first paint: no empty-state flash with a populated fragment. ----
  assert.ok(!doc.getElementById("fleet").textContent.includes("no boxes yet"), "must not flash empty state");
  assert.ok(doc.querySelector(".box-card"), "box card must render synchronously");
  console.error("PASS: real page, synchronous skeleton on load, no empty-state flash");

  // ---- 3. Real health/session poll lands; dot turns healthy. ----
  const dot = await waitFor(() => {
    const d = doc.querySelector(".dot");
    return d && d.classList.contains("on") ? d : null;
  }, { label: "the real /health poll to resolve and turn the dot healthy" });
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
  await waitFor(() => doc.querySelector(".sess"), { label: "a real session row to appear after the real create round trip" });
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

  // Wait for BOTH halves of the durable message — the reasoning block and
  // the reply text. They can land in separate renders, so a wait on the
  // reasoning block alone leaves the text assertion racing the next render.
  const reasoningDetails = await waitFor(() => {
    const r = doc.querySelector("#timeline .tl-messages details.reason");
    const texts = [...doc.querySelectorAll("#timeline .tl-messages .msg .text")];
    return r && texts.some((n) => /reply number \d+/.test(n.textContent)) ? r : null;
  }, { label: "the real first turn's reasoning block and reply text to render" });
  const firstMsgTexts = [...doc.querySelectorAll("#timeline .tl-messages .msg .text")];
  const firstMsgText = firstMsgTexts.find((n) => /reply number \d+/.test(n.textContent));
  const firstReplyNum = firstMsgText.textContent.match(/reply number (\d+)/)[1];
  reasoningDetails.open = true;
  console.error("PASS: real turn " + firstReplyNum + " rendered (reasoning block + text), expanded it");

  await waitFor(() => !sendBtn.disabled, { label: "the composer to re-enable after the first real turn" });
  promptBox.value = "again";
  sendBtn.click();
  const secondReplyNum = String(Number(firstReplyNum) + 1);
  await waitFor(() => replyTextNode(doc, secondReplyNum), { label: "a real second reply (number " + secondReplyNum + ") to render" });
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
  // jsdom does not synthesize "wheel"/"scroll" events from a plain property
  // write the way a real browser's input and layout pipeline does (there is
  // no real layout here at all) — dispatch both explicitly, so the page's
  // own listeners (index.html's renderTimeline) see the "user scrolled up"
  // signal exactly as they would from a real user action, and flip
  // tlDom.stick accordingly.
  //
  // The wheel-up event is load-bearing, not decoration. renderTimeline's
  // "scroll" listener deliberately IGNORES a scroll event that arrives
  // within 120ms of its own programmatic pin (tlDom.lastAuto), to stop a
  // late-delivered scroll from releasing stick mid-replay. A scroll-only
  // dispatch therefore races that product-defined window: it releases
  // stick when the preceding turn's render happened to be more than 120ms
  // ago, and is silently swallowed when it was not — the test then asserts
  // pinned-tail behavior that the page was never told to enter. A wheel-up
  // is the same listener block's unambiguous, unguarded user-intent path,
  // so dispatching it makes the release deterministic instead of dependent
  // on how fast the machine ran the previous turn.
  tl.dispatchEvent(new w.WheelEvent("wheel", { deltaY: -120 }));
  tl.dispatchEvent(new w.Event("scroll"));
  await waitFor(() => !sendBtn.disabled, { label: "the composer to re-enable after the second real turn" });
  promptBox.value = "third";
  sendBtn.click();
  // Wait for the third reply to actually RENDER before checking scrollTop.
  // A fixed sleep here was vacuous as well as flaky: if no render had
  // happened yet, scrollTop was trivially still 0 and the assertion passed
  // without ever exercising the pinned-tail behavior it names.
  const thirdReplyNum = String(Number(firstReplyNum) + 2);
  await waitFor(() => replyTextNode(doc, thirdReplyNum), { label: "a real third reply (number " + thirdReplyNum + ") to render" });
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
