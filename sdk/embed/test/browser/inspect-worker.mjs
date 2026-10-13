// A Worker that reads the persistent store of the origin: for each session the
// lines of its log, whether any line is torn, and the size of each file in
// blobs, temporary files included. It waits for the store lock, so it runs when
// no other Worker holds the store.
import { storeLockName } from "/embed/browserfs.mjs";
import { openVfs } from "/embed/opfsfs.mjs";

async function read(vfs) {
  const root = "/persist/sessions";
  const sessions = {};
  const ids = await vfs.readdir(root).catch(() => []);
  for (const id of ids) {
    const dir = `${root}/${id}`;
    const bytes = await vfs.readFile(`${dir}/log.jsonl`).catch(() => new Uint8Array());
    const text = new TextDecoder().decode(bytes);
    const lines = text === "" ? [] : text.replace(/\n$/, "").split("\n");
    let bad = text !== "" && !text.endsWith("\n");
    for (const line of lines) {
      try {
        JSON.parse(line);
      } catch {
        bad = true;
      }
    }
    const blobs = {};
    for (const key of await vfs.readdir(`${dir}/blobs`).catch(() => [])) blobs[key] = (await vfs.stat(`${dir}/blobs/${key}`)).size;
    sessions[id] = { lines, bad, blobs };
  }
  return { sessions };
}

// pattern checks the file written by the kill mode of fsprobe-worker.mjs.
async function pattern(vfs, file) {
  const bytes = await vfs.readFile(file);
  return { size: bytes.length, prefix: bytes.every((b, i) => b === i % 251) };
}

onmessage = async (e) => {
  try {
    postMessage(
      await navigator.locks.request(storeLockName(), async () => {
        const vfs = await openVfs({ persist: true });
        return e.data.file ? pattern(vfs, e.data.file) : read(vfs);
      }),
    );
  } catch (err) {
    postMessage({ failure: String(err?.stack ?? err) });
  }
};
