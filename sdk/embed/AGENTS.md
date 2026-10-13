# sdk/embed

Read the root AGENTS.md and `sdk/AGENTS.md`. The rules of `sdk/typescript` do not bind this package.

- This package is the page-side client of the WebAssembly module (`docs/architecture.md`, `WebAssembly`). It speaks ACP to the module and has no UI and no ACP client of its own.
- Keep it plain ESM that runs in a browser, a Worker, and Node. Import a Node built-in only by a dynamic `import()` behind a Node check, so a browser bundle never loads it.
- Add no runtime dependency without a design decision. A dependency or a build step needs the same decision.
- The message names between the page and the Worker are the contract of `src/bridge.mjs` and `src/workerbridge.mjs`. Change both ends together.
- A new option of `createHarness` needs a line in the WebAssembly section of the spec first.
- A message to the Worker moves data, not functions. Keep every option serializable.
- Tests start the real module in a Worker against a scripted model server. They need `HARNESS_WASM` and `HARNESS_WASM_EXEC`, and a missing variable fails the test.
- Run `node --test sdk/embed/test/*.test.mjs`.
