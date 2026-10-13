# sdk/embed

Read the root AGENTS.md and `sdk/AGENTS.md`. The rules of `sdk/typescript` do not bind this package.

- This package is the page-side client of the WebAssembly module (`docs/architecture.md`, `Public packages`; the module is in `WebAssembly`). It speaks ACP to the module and has no UI and no ACP client of its own.
- Keep it plain ESM that runs in a browser, a Worker, and Node. Import a Node built-in only by a dynamic `import()` behind a Node check, so a browser bundle never loads it.
- Add no runtime dependency without a design decision. A dependency or a build step needs the same decision.
- The message names between the page and the Worker are the contract of `src/bridge.mjs` and `src/workerbridge.mjs`. Change both ends together.
- A new option of `createHarness` needs a line in the Public packages section of the spec first.
- A message to the Worker moves data, not functions. Keep every option serializable.
- Tests start the real module in a Worker against a scripted model server. The model is `internal/embedfake`, the scripted server of `harnesstest`; a test double of its own would escape the wire gate. The tests need `HARNESS_WASM`, `HARNESS_WASM_EXEC`, and `HARNESS_EMBEDFAKE`, and a missing variable fails the test.
- Keep `src/index.d.ts` equal to the exported API of `src/index.mjs`.
- Run `node --test sdk/embed/test/*.test.mjs`.
- `test/browser.mjs`, `test/browser/page.mjs`, and `test/browser/fsstub.mjs` are the fixture of `e2e/browser_test.go` (headless Chromium). Run it with `npm ci` in this directory, `npx playwright install chromium`, and `HARNESS_E2E_BROWSER=1 go test -race ./e2e/ -run TestBrowserRows`. `playwright` is a dev dependency, pinned in `package.json` and the lock file; the package has no runtime dependency.
