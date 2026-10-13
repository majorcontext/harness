// Only Node loads this module, and it loads the built-ins by dynamic import, so
// a browser bundler never resolves them.
const [{ default: crypto }, { default: fs }, { default: path }] = await Promise.all([
  import("node:crypto"),
  import("node:fs"),
  import("node:path"),
]);

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === "EPERM";
  }
}

function pidOf(text) {
  return Number.parseInt(text.split("\n")[0], 10);
}

function readLock(file) {
  try {
    return fs.readFileSync(file, "utf8");
  } catch (err) {
    if (err.code === "ENOENT") return null;
    throw err;
  }
}

// Only a holder of the guard replaces a stale lock, so a process never removes
// the live lock of another. A guard older than staleGuardMs belongs to a crashed
// process.
const staleGuardMs = 10_000;

function withGuard(file, fn) {
  const guard = file + ".takeover";
  for (;;) {
    try {
      fs.mkdirSync(guard);
      break;
    } catch (err) {
      if (err.code !== "EEXIST") throw err;
      try {
        if (Date.now() - fs.statSync(guard).mtimeMs > staleGuardMs) fs.rmdirSync(guard);
      } catch {
        // The holder removed it first.
      }
    }
  }
  try {
    return fn();
  } finally {
    fs.rmdirSync(guard);
  }
}

// acquireStoreLock makes dir exclusive to this process. The lock file
// <dir>/.embed.lock holds the pid and a random token. It appears whole, by a
// hard link of a private file, so no process reads a partial lock. When the pid
// in the file is dead, one process replaces the file under a guard directory
// and re-checks the file inside it. The returned function removes the file only
// when it still holds the token of this process.
export function acquireStoreLock(dir) {
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, ".embed.lock");
  const mine = `${process.pid}\n${crypto.randomBytes(16).toString("hex")}`;
  const tmp = `${file}.${crypto.randomBytes(8).toString("hex")}.tmp`;
  fs.writeFileSync(tmp, mine);
  const release = () => {
    if (readLock(file) === mine) fs.rmSync(file, { force: true });
  };
  try {
    for (;;) {
      try {
        fs.linkSync(tmp, file);
        return release;
      } catch (err) {
        if (err.code !== "EEXIST") throw err;
      }
      const held = withGuard(file, () => {
        const text = readLock(file);
        if (text === null) return false;
        const pid = pidOf(text);
        if (pid > 0 && alive(pid)) throw new Error(`store directory ${dir} is in use by process ${pid}`);
        fs.renameSync(tmp, file);
        return true;
      });
      if (held) return release;
    }
  } finally {
    fs.rmSync(tmp, { force: true });
  }
}
