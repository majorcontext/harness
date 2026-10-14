// The file system of the browser Worker. It implements the callback API that
// wasm_exec.js and Go's syscall layer (syscall/fs_js.go) call on globalThis.fs:
// open close read write fsync fstat stat lstat ftruncate truncate mkdir readdir
// rmdir unlink rename readlink symlink link chmod fchmod chown fchown lchown
// utimes. The tree is in memory, except the directory below persistRoot, which
// is the origin private file system (OPFS). A file of OPFS stays open through a
// FileSystemSyncAccessHandle, which only a Worker has: a write puts the new
// bytes at an offset and fsync is flush().

/** persistRoot is the path of the OPFS directory. */
export const persistRoot = "/persist";
/** workDir is the work directory of a session. It is in memory. */
export const workDir = "/work";

// Node's O_* values, which Go reads from fs.constants.
export const constants = {
  O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 64, O_EXCL: 128, O_TRUNC: 512, O_APPEND: 1024, O_DIRECTORY: 65536,
};

const S_IFDIR = 0o40000;
const S_IFREG = 0o100000;
const S_IFCHR = 0o20000;

function fsError(code, what) {
  return Object.assign(new Error(`${code}: ${what}`), { code });
}

const domCodes = {
  NotFoundError: "ENOENT",
  TypeMismatchError: "ENOTDIR",
  InvalidModificationError: "ENOTEMPTY",
  NoModificationAllowedError: "EBUSY",
  QuotaExceededError: "ENOSPC",
  NotAllowedError: "EACCES",
  SecurityError: "EACCES",
  InvalidStateError: "EBADF",
  TypeError: "EINVAL",
};

/**
 * toError returns the error that Go can map: an Error with a code from Go's
 * table. An error of the adapter keeps its code, a DOMException of OPFS gets
 * the closest code, and anything else is EIO.
 */
export function toError(e) {
  if (e && typeof e.code === "string" && /^E[A-Z]+$/.test(e.code)) return e;
  return fsError(domCodes[e?.name] ?? "EIO", String(e?.message ?? e));
}

function split(path) {
  const out = [];
  for (const part of path.split("/")) {
    if (part === "..") out.pop();
    else if (part && part !== ".") out.push(part);
  }
  return out;
}

/** resolvePath returns the absolute path of segments, relative to cwd. */
export function resolvePath(cwd, ...segments) {
  let joined = "";
  for (let i = segments.length - 1; i >= 0; i--) {
    if (!segments[i]) continue;
    joined = joined ? `${segments[i]}/${joined}` : segments[i];
    if (segments[i].startsWith("/")) return "/" + split(joined).join("/");
  }
  return "/" + split(`${cwd}/${joined}`).join("/");
}

// A memory tree. A file node is shared by every open descriptor.
class MemFs {
  root = this.dir();

  dir() {
    return { dir: true, kids: new Map(), mtimeMs: Date.now() };
  }

  node(parts) {
    let n = this.root;
    for (const part of parts) {
      if (!n.dir) throw fsError("ENOTDIR", part);
      n = n.kids.get(part);
      if (!n) throw fsError("ENOENT", part);
    }
    return n;
  }

  parent(parts) {
    const p = this.node(parts.slice(0, -1));
    if (!p.dir) throw fsError("ENOTDIR", parts.join("/"));
    return p;
  }

  async stat(parts) {
    const n = this.node(parts);
    return { dir: n.dir, size: n.dir ? 0 : n.size, mtimeMs: n.mtimeMs };
  }

  async openFile(parts, { create, trunc }) {
    let n;
    try {
      n = this.node(parts);
    } catch (e) {
      if (e.code !== "ENOENT" || !create) throw e;
      const parent = this.parent(parts);
      n = { dir: false, data: new Uint8Array(64), size: 0, mtimeMs: Date.now() };
      parent.kids.set(parts.at(-1), n);
      return fileRef(n);
    }
    if (n.dir) throw fsError("EISDIR", parts.join("/"));
    if (trunc) {
      n.size = 0;
      n.mtimeMs = Date.now();
    }
    return fileRef(n);
  }

  async mkdir(parts) {
    if (parts.length === 0) throw fsError("EEXIST", "/");
    const p = this.parent(parts);
    if (p.kids.has(parts.at(-1))) throw fsError("EEXIST", parts.join("/"));
    p.kids.set(parts.at(-1), this.dir());
  }

  async readdir(parts) {
    const n = this.node(parts);
    if (!n.dir) throw fsError("ENOTDIR", parts.join("/"));
    return [...n.kids.keys()];
  }

  async unlink(parts) {
    const n = this.node(parts);
    if (n.dir) throw fsError("EISDIR", parts.join("/"));
    this.parent(parts).kids.delete(parts.at(-1));
  }

  async rmdir(parts) {
    const n = this.node(parts);
    if (!n.dir) throw fsError("ENOTDIR", parts.join("/"));
    if (n.kids.size > 0) throw fsError("ENOTEMPTY", parts.join("/"));
    if (parts.length === 0) throw fsError("EBUSY", "/");
    this.parent(parts).kids.delete(parts.at(-1));
  }

  async rename(from, to) {
    const n = this.node(from);
    const dest = this.parent(to);
    const old = dest.kids.get(to.at(-1));
    if (old && old.dir !== n.dir) throw fsError(old.dir ? "EISDIR" : "ENOTDIR", to.join("/"));
    if (old?.dir && old.kids.size > 0) throw fsError("ENOTEMPTY", to.join("/"));
    this.parent(from).kids.delete(from.at(-1));
    dest.kids.set(to.at(-1), n);
  }

  async touch(parts, mtimeMs) {
    this.node(parts).mtimeMs = mtimeMs;
  }
}

function fileRef(n) {
  return {
    read(buf, off, len, at) {
      const count = Math.max(0, Math.min(len, n.size - at));
      buf.set(n.data.subarray(at, at + count), off);
      return count;
    },
    write(buf, off, len, at) {
      const end = at + len;
      if (end > n.data.length) {
        const next = new Uint8Array(Math.max(end, n.data.length * 2));
        next.set(n.data.subarray(0, n.size));
        n.data = next;
      }
      if (at > n.size) n.data.fill(0, n.size, at);
      n.data.set(buf.subarray(off, off + len), at);
      n.size = Math.max(n.size, end);
      n.mtimeMs = Date.now();
      return len;
    },
    size: () => n.size,
    truncate(size) {
      if (size > n.data.length) {
        const next = new Uint8Array(size);
        next.set(n.data.subarray(0, n.size));
        n.data = next;
      } else if (size > n.size) {
        n.data.fill(0, n.size, size);
      }
      n.size = size;
      n.mtimeMs = Date.now();
    },
    flush() {},
    release() {},
  };
}

// An OPFS tree. A sync access handle locks its file, and Go opens one file more
// than once (the append handle of a log and the handle of a scan of the same
// log), so descriptors of one path share one handle that closes with the last.
class OpfsFs {
  open = new Map();

  constructor(root) {
    this.root = root;
  }

  async dir(parts) {
    let d = this.root;
    for (const part of parts) {
      try {
        d = await d.getDirectoryHandle(part);
      } catch (e) {
        if (e.name === "TypeMismatchError") throw fsError("ENOTDIR", part);
        throw e;
      }
    }
    return d;
  }

  // entry returns the handle of a file or directory, or throws ENOENT.
  async entry(parts) {
    if (parts.length === 0) return { dir: true, handle: this.root };
    const parent = await this.dir(parts.slice(0, -1));
    const name = parts.at(-1);
    try {
      return { dir: false, handle: await parent.getFileHandle(name), parent, name };
    } catch (e) {
      if (e.name !== "TypeMismatchError") throw e;
    }
    return { dir: true, handle: await parent.getDirectoryHandle(name), parent, name };
  }

  async stat(parts) {
    const e = await this.entry(parts);
    if (e.dir) return { dir: true, size: 0, mtimeMs: Date.now() };
    const state = this.open.get(parts.join("/"));
    if (state) {
      const s = await state.ready;
      return { dir: false, size: s.sah.getSize(), mtimeMs: s.mtimeMs };
    }
    const f = await e.handle.getFile();
    return { dir: false, size: f.size, mtimeMs: f.lastModified };
  }

  async openFile(parts, { create, trunc }) {
    if (parts.length === 0) throw fsError("EISDIR", "/");
    const parent = await this.dir(parts.slice(0, -1));
    const name = parts.at(-1);
    let handle;
    try {
      handle = await parent.getFileHandle(name, { create: Boolean(create) });
    } catch (e) {
      if (e.name === "TypeMismatchError") throw fsError("EISDIR", name);
      throw e;
    }
    const key = parts.join("/");
    let state = this.open.get(key);
    if (!state) {
      state = { refs: 0, mtimeMs: 0 };
      state.ready = (async () => {
        state.mtimeMs = (await handle.getFile()).lastModified;
        state.sah = await handle.createSyncAccessHandle();
        return state;
      })();
      this.open.set(key, state);
    }
    state.refs++;
    try {
      await state.ready;
      if (trunc) state.sah.truncate(0);
    } catch (e) {
      if (--state.refs === 0) {
        state.sah?.close();
        this.open.delete(key);
      }
      throw e;
    }
    return {
      read: (buf, off, len, at) => state.sah.read(buf.subarray(off, off + len), { at }),
      write(buf, off, len, at) {
        state.mtimeMs = Date.now();
        return state.sah.write(buf.subarray(off, off + len), { at });
      },
      size: () => state.sah.getSize(),
      truncate(size) {
        state.mtimeMs = Date.now();
        state.sah.truncate(size);
      },
      flush: () => state.sah.flush(),
      release: () => {
        if (--state.refs === 0) {
          state.sah.close();
          this.open.delete(key);
        }
      },
    };
  }

  // kind returns "file", "dir", or null for the entry name of parent.
  async kind(parent, name) {
    try {
      await parent.getFileHandle(name);
      return "file";
    } catch (e) {
      if (e.name === "TypeMismatchError") return "dir";
      if (e.name === "NotFoundError") return null;
      throw e;
    }
  }

  async mkdir(parts) {
    if (parts.length === 0) throw fsError("EEXIST", "/");
    const parent = await this.dir(parts.slice(0, -1));
    const name = parts.at(-1);
    if (await this.kind(parent, name)) throw fsError("EEXIST", name);
    await parent.getDirectoryHandle(name, { create: true });
  }

  async readdir(parts) {
    const e = await this.entry(parts);
    if (!e.dir) throw fsError("ENOTDIR", parts.join("/"));
    const names = [];
    for await (const name of e.handle.keys()) names.push(name);
    return names;
  }

  async unlink(parts) {
    const e = await this.entry(parts);
    if (e.dir) throw fsError("EISDIR", parts.join("/"));
    await e.parent.removeEntry(e.name);
  }

  async rmdir(parts) {
    const e = await this.entry(parts);
    if (!e.dir) throw fsError("ENOTDIR", parts.join("/"));
    if (parts.length === 0) throw fsError("EBUSY", "/");
    for await (const _ of e.handle.keys()) throw fsError("ENOTEMPTY", parts.join("/"));
    await e.parent.removeEntry(e.name);
  }

  // rename uses the move() of the file handle, which replaces the destination.
  // The standard does not say whether that is atomic against a crash. A browser
  // without move() gets a copy and a remove, and so does a destination that is
  // open (a reader holds its handle, and a handle blocks move()): the bytes go
  // into the open handle, and the destination holds a prefix if the Worker ends
  // in between. A source that is open is EBUSY.
  async rename(from, to) {
    const e = await this.entry(from);
    if (e.dir) throw fsError("ENOSYS", "rename of a directory");
    const target = await this.dir(to.slice(0, -1));
    if ((await this.kind(target, to.at(-1))) === "dir") throw fsError("EISDIR", to.join("/"));
    if (this.open.has(from.join("/"))) throw fsError("EBUSY", from.join("/"));
    const destOpen = this.open.get(to.join("/"));
    if (typeof e.handle.move === "function" && !destOpen) {
      await e.handle.move(target, to.at(-1));
      return;
    }
    const bytes = new Uint8Array(await (await e.handle.getFile()).arrayBuffer());
    const own = destOpen ? null : await (await target.getFileHandle(to.at(-1), { create: true })).createSyncAccessHandle();
    const sah = destOpen ? (await destOpen.ready).sah : own;
    try {
      sah.truncate(0);
      sah.write(bytes, { at: 0 });
      sah.flush();
    } finally {
      own?.close();
    }
    await e.parent.removeEntry(e.name);
  }

  async touch() {}
}

/**
 * Vfs is the tree that globalThis.fs serves: memory, with the OPFS directory at
 * persistRoot when there is one. Every path is absolute and clean.
 */
export class Vfs {
  constructor(opfsRoot) {
    this.mem = new MemFs();
    for (const d of [workDir, "/tmp", persistRoot]) this.mem.node([]).kids.set(d.slice(1), this.mem.dir());
    this.opfs = opfsRoot ? new OpfsFs(opfsRoot) : null;
  }

  route(path) {
    const parts = split(path);
    if (this.opfs && parts[0] === persistRoot.slice(1)) return { fs: this.opfs, parts: parts.slice(1) };
    return { fs: this.mem, parts };
  }

  /** readFile returns the bytes of the file at path. */
  async readFile(path) {
    const { fs, parts } = this.route(path);
    const ref = await fs.openFile(parts, {});
    try {
      const out = new Uint8Array(ref.size());
      ref.read(out, 0, out.length, 0);
      return out;
    } finally {
      ref.release();
    }
  }

  async readdir(path) {
    const { fs, parts } = this.route(path);
    return fs.readdir(parts);
  }

  async stat(path) {
    const { fs, parts } = this.route(path);
    return fs.stat(parts);
  }
}

/** openVfs returns a Vfs, with the OPFS directory of the store when persist is true. */
export async function openVfs({ persist }) {
  if (!persist) return new Vfs(null);
  const root = await (await navigator.storage.getDirectory()).getDirectoryHandle("harness-store", { create: true });
  return new Vfs(root);
}

function consoleWriter(log) {
  const decoder = new TextDecoder("utf-8");
  let pending = "";
  return (buf) => {
    pending += decoder.decode(buf, { stream: true });
    const nl = pending.lastIndexOf("\n");
    if (nl !== -1) {
      log(pending.slice(0, nl));
      pending = pending.slice(nl + 1);
    }
    return buf.length;
  };
}

// stat is the object that Go reads: numbers, and the isDirectory method that
// Open calls on the result of fstat.
function stat(mode, size, t) {
  return {
    dev: 0, ino: 0, mode, nlink: 1, uid: 0, gid: 0, rdev: 0, size, blksize: 4096, blocks: Math.ceil(size / 512),
    atimeMs: t, mtimeMs: t, ctimeMs: t,
    isDirectory: () => (mode & 0o170000) === S_IFDIR,
    isFile: () => (mode & 0o170000) === S_IFREG,
    isSymbolicLink: () => false,
  };
}

const statOf = (s, size = s.size) => stat(s.dir ? S_IFDIR | 0o755 : S_IFREG | 0o644, size, s.mtimeMs);
const charDevice = stat(S_IFCHR | 0o620, 0, 0);

// call adapts an async function to the callback style of fs: it answers once,
// with null as the error on success, and never throws.
const call = (fn) => (...args) => {
  const cb = args.pop();
  let p;
  try {
    p = Promise.resolve(fn(...args));
  } catch (e) {
    p = Promise.reject(e);
  }
  p.then(
    (v) => cb(null, v),
    (e) => cb(toError(e)),
  );
};

// serial runs fn after the earlier calls with the same key, so a check of a path
// and the create that follows it cannot interleave with another create.
function serializer() {
  const tails = new Map();
  return (key, fn) => {
    const run = (tails.get(key) ?? Promise.resolve()).then(fn, fn);
    const tail = run.catch(() => {});
    tails.set(key, tail);
    tail.then(() => tails.get(key) === tail && tails.delete(key));
    return run;
  };
}

// openEntry opens path and returns the table entry of the descriptor.
async function openEntry({ vfs, abs }, path, flags) {
  const { fs, parts } = vfs.route(abs(path));
  const acc = flags & 3;
  const want = {
    create: Boolean(flags & constants.O_CREAT), excl: Boolean(flags & constants.O_EXCL), trunc: Boolean(flags & constants.O_TRUNC),
  };
  let s;
  try {
    s = await fs.stat(parts);
  } catch (e) {
    if (toError(e).code !== "ENOENT" || !want.create) throw e;
  }
  if (s && want.create && want.excl) throw fsError("EEXIST", path);
  if (s?.dir) {
    if (acc !== 0) throw fsError("EISDIR", path);
    return { dir: true, path: abs(path) };
  }
  if (flags & constants.O_DIRECTORY) throw fsError("ENOTDIR", path);
  const ref = await fs.openFile(parts, want);
  return { ref, pos: 0, read: acc !== 1, write: acc !== 0, append: Boolean(flags & constants.O_APPEND), path: abs(path) };
}

// fdCalls returns the calls on descriptors. ctx holds the vfs, the path
// resolver, and the table of descriptors.
function fdCalls(ctx) {
  const { vfs, fds, out, err } = ctx;
  let next = 3;
  const serial = serializer();
  const std = (fd) => fd >= 0 && fd <= 2;
  const entry = (fd) => {
    const e = fds.get(fd);
    if (!e) throw fsError("EBADF", String(fd));
    return e;
  };

  return {
    writeSync: (fd, buf) => (fd === 2 ? err(buf) : out(buf)),
    write: call(async (fd, buf, offset, length, position) => {
      if (std(fd)) return (fd === 2 ? err : out)(buf.subarray(offset, offset + length));
      const e = entry(fd);
      if (e.dir || !e.write) throw fsError(e.dir ? "EISDIR" : "EBADF", String(fd));
      const at = e.append ? e.ref.size() : position ?? e.pos;
      const n = e.ref.write(buf, offset, length, at);
      if (position == null) e.pos = at + n;
      return n;
    }),
    read: call(async (fd, buf, offset, length, position) => {
      if (fd === 0) return 0;
      const e = entry(fd);
      if (e.dir) throw fsError("EISDIR", String(fd));
      if (!e.read) throw fsError("EBADF", String(fd));
      const at = position ?? e.pos;
      const n = e.ref.read(buf, offset, length, at);
      if (position == null) e.pos = at + n;
      return n;
    }),
    open: call(async (path, flags) => {
      const run = () => openEntry(ctx, path, flags);
      const e = await (flags & constants.O_CREAT ? serial(ctx.abs(path), run) : run());
      const fd = next++;
      fds.set(fd, e);
      return fd;
    }),
    close: call(async (fd) => {
      const e = entry(fd);
      fds.delete(fd);
      e.ref?.release();
    }),
    fsync: call(async (fd) => {
      if (std(fd)) return;
      entry(fd).ref?.flush();
    }),
    fstat: call(async (fd) => {
      if (std(fd)) return charDevice;
      const e = entry(fd);
      if (e.dir) return statOf({ dir: true, size: 0, mtimeMs: Date.now() });
      const { fs, parts } = vfs.route(e.path);
      const s = await fs.stat(parts).catch(() => ({ dir: false, mtimeMs: Date.now() }));
      return statOf(s, e.ref.size());
    }),
    ftruncate: call(async (fd, length) => {
      const e = entry(fd);
      if (e.dir) throw fsError("EISDIR", String(fd));
      if (!e.write) throw fsError("EINVAL", String(fd));
      e.ref.truncate(length);
    }),
    fchmod: call(async (fd) => void entry(fd)),
    fchown: call(async (fd) => void entry(fd)),
  };
}

// pathCalls returns the calls on paths.
function pathCalls({ vfs, abs }) {
  const at = (path) => vfs.route(abs(path));
  const exists = async (path) => {
    const { fs, parts } = at(path);
    await fs.stat(parts);
  };
  const stat = call(async (path) => {
    const { fs, parts } = at(path);
    return statOf(await fs.stat(parts));
  });
  return {
    stat,
    lstat: stat,
    truncate: call(async (path, length) => {
      const { fs, parts } = at(path);
      const ref = await fs.openFile(parts, {});
      try {
        ref.truncate(length);
        ref.flush();
      } finally {
        ref.release();
      }
    }),
    mkdir: call(async (path) => {
      const { fs, parts } = at(path);
      await fs.mkdir(parts);
    }),
    readdir: call(async (path) => {
      const { fs, parts } = at(path);
      return fs.readdir(parts);
    }),
    rmdir: call(async (path) => {
      const { fs, parts } = at(path);
      await fs.rmdir(parts);
    }),
    unlink: call(async (path) => {
      const { fs, parts } = at(path);
      await fs.unlink(parts);
    }),
    rename: call(async (from, to) => {
      const a = at(from);
      const b = at(to);
      if (a.fs !== b.fs) throw fsError("EXDEV", `${from} to ${to}`);
      await a.fs.rename(a.parts, b.parts);
    }),
    utimes: call(async (path, atime, mtime) => {
      const { fs, parts } = at(path);
      await fs.stat(parts);
      await fs.touch(parts, Number(mtime) * 1000);
    }),
    chmod: call(exists),
    chown: call(exists),
    lchown: call(exists),
    readlink: call(async (path) => {
      await exists(path);
      throw fsError("EINVAL", `${path} is not a link`);
    }),
    symlink: call(async () => {
      throw fsError("ENOSYS", "symlink");
    }),
    link: call(async () => {
      throw fsError("ENOSYS", "link");
    }),
  };
}

/**
 * nodeFs returns the object for globalThis.fs over vfs. cwd() is the directory
 * that a relative path resolves against. fd 0, 1, and 2 are the standard
 * streams: 1 and 2 write lines to the console. Every call answers through its
 * callback, once, with null as the error on success, because Go treats any
 * other first argument as a failure.
 */
export function nodeFs(vfs, cwd = () => workDir, log = { out: console.log, err: console.error }) {
  const ctx = {
    vfs,
    abs: (p) => resolvePath(cwd(), String(p)),
    fds: new Map(),
    out: consoleWriter(log.out),
    err: consoleWriter(log.err),
  };
  return { constants, ...fdCalls(ctx), ...pathCalls(ctx) };
}

/**
 * installFs sets globalThis.fs, process, and path over vfs for wasm_exec.js,
 * which keeps what it finds. process.cwd() is workDir.
 */
export function installFs(vfs) {
  let cwd = workDir;
  globalThis.fs = nodeFs(vfs, () => cwd);
  globalThis.path = { resolve: (...segments) => resolvePath(cwd, ...segments) };
  const enosys = () => fsError("ENOSYS", "not implemented");
  globalThis.process = {
    getuid: () => -1, getgid: () => -1, geteuid: () => -1, getegid: () => -1,
    getgroups: () => { throw enosys(); },
    umask: () => { throw enosys(); },
    pid: -1,
    ppid: -1,
    cwd: () => cwd,
    chdir(dir) {
      cwd = resolvePath(cwd, dir);
    },
  };
}
