// The store of the browser Worker: the store-wide Web Lock, then the file
// system of opfsfs.mjs. A Worker loads this module before wasm_exec.js, because
// Go reads globalThis.fs, process, and path when the module starts.
import { installFs, openVfs } from "./opfsfs.mjs";

/** StoreInUseError means another Worker of this origin holds the store. */
export class StoreInUseError extends Error {
  code = "store_in_use";
  constructor() {
    super("the store is in use by another page of this origin");
    this.name = "StoreInUseError";
  }
}

/** storeLockName is the name of the Web Lock of the store of this origin. */
export const storeLockName = () => `harness:store:${globalThis.location?.origin ?? ""}`;

/** storeLockWaitMs is how long a Worker waits for the store lock before it fails with store_in_use. */
export const storeLockWaitMs = 2000;

/**
 * takeStoreLock takes the store-wide Web Lock of the origin and returns the
 * function that releases it. The lock stays held until that function runs or the
 * Worker ends. It waits up to storeLockWaitMs, so a page that reloads while the
 * old Worker still holds the lock gets it, and rejects with StoreInUseError when
 * another holder keeps it longer. It never steals.
 */
export function takeStoreLock(locks = globalThis.navigator?.locks, waitMs = storeLockWaitMs) {
  if (!locks) return Promise.reject(new Error("the persistent store needs navigator.locks"));
  return new Promise((resolve, reject) => {
    locks
      .request(storeLockName(), { mode: "exclusive", signal: AbortSignal.timeout(waitMs) }, (lock) => new Promise((release) => resolve(release)))
      .catch((err) => reject(err?.name === "TimeoutError" ? new StoreInUseError() : err));
  });
}

/**
 * installBrowserFs takes the store lock when persist is true, then opens the
 * file system and sets globalThis.fs, process, and path. The lock comes first:
 * no file of the store is open before the Worker owns the store. It resolves
 * with the function that releases the lock.
 */
export async function installBrowserFs({ persist }) {
  const release = persist ? await takeStoreLock() : () => {};
  try {
    installFs(await openVfs({ persist }));
  } catch (err) {
    release();
    throw err;
  }
  return release;
}
