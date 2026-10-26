import { installFs, Vfs } from "./opfsfs.mjs";

/**
 * installMemoryFs sets globalThis.fs, process, and path over an empty in-memory
 * tree with /work, /tmp, and /persist. Call it before the wasm_exec.js of the
 * module runs: wasm_exec.js keeps a file system that it finds, and otherwise
 * installs a stub whose calls all fail with ENOSYS. It replaces globalThis.process,
 * so do not call it in a Node program.
 */
export function installMemoryFs() {
  installFs(new Vfs(null));
}
