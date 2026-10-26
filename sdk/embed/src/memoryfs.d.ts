/**
 * installMemoryFs sets globalThis.fs, process, and path over an empty in-memory tree. Call it before the wasm_exec.js of
 * the module runs. It replaces globalThis.process, so do not call it in a Node program.
 */
export function installMemoryFs(): void;
