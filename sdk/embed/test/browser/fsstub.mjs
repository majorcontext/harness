// A file system for the Worker of the browser job: the root directory exists
// and every other path is absent, so the module finds no project config and no
// AGENTS.md, and nothing can be created. It stands in for the ZenFS shim, which
// the module does not have yet. fd 1 and 2 go to the console. It must load
// before wasm_exec.js.
const absent = () => Object.assign(new Error("no such file or directory"), { code: "ENOENT" });
const decoder = new TextDecoder("utf-8");
let pending = "";

function write(buf) {
  pending += decoder.decode(buf);
  const nl = pending.lastIndexOf("\n");
  if (nl !== -1) {
    console.log(pending.slice(0, nl));
    pending = pending.slice(nl + 1);
  }
  return buf.length;
}

const fail = (callback) => callback(absent());

const root = {
  dev: 0, ino: 1, mode: 0o40755, nlink: 1, uid: 0, gid: 0, rdev: 0, size: 0, blksize: 4096, blocks: 0,
  atimeMs: 0, mtimeMs: 0, ctimeMs: 0, isDirectory: () => true, isFile: () => false,
};
const stat = (path, callback) => (path === "/" ? callback(null, root) : fail(callback));

globalThis.fs = {
  constants: { O_WRONLY: 1, O_RDWR: 2, O_CREAT: 64, O_TRUNC: 512, O_APPEND: 1024, O_EXCL: 128, O_DIRECTORY: 65536 },
  writeSync: (fd, buf) => write(buf),
  write(fd, buf, offset, length, position, callback) {
    if (offset !== 0 || length !== buf.length || position !== null) {
      fail(callback);
      return;
    }
    callback(null, write(buf));
  },
  chmod: (p, m, cb) => fail(cb),
  chown: (p, u, g, cb) => fail(cb),
  close: (fd, cb) => fail(cb),
  fchmod: (fd, m, cb) => fail(cb),
  fchown: (fd, u, g, cb) => fail(cb),
  fstat: (fd, cb) => fail(cb),
  fsync: (fd, cb) => cb(null),
  ftruncate: (fd, l, cb) => fail(cb),
  lchown: (p, u, g, cb) => fail(cb),
  link: (p, l, cb) => fail(cb),
  lstat: stat,
  mkdir: (p, perm, cb) => fail(cb),
  open: (p, f, m, cb) => fail(cb),
  read: (fd, b, o, l, pos, cb) => fail(cb),
  readdir: (p, cb) => fail(cb),
  readlink: (p, cb) => fail(cb),
  rename: (a, b, cb) => fail(cb),
  rmdir: (p, cb) => fail(cb),
  stat,
  symlink: (p, l, cb) => fail(cb),
  truncate: (p, l, cb) => fail(cb),
  unlink: (p, cb) => fail(cb),
  utimes: (p, a, m, cb) => fail(cb),
};
