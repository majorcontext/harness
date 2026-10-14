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

const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

const running = (text) => {
  const pid = pidOf(text);
  return pid > 0 && alive(pid);
};

// takeOver replaces the stale lock whose text is stale by the lock in tmp. The
// claim file is named by the token of the stale lock and appears whole by a hard
// link of tmp, so one process at a time holds a claim on that lock. The claimant
// checks the lock again after it holds the claim: a process that read the stale
// text late finds a live lock there and gives up. Then only the claimant changes
// the lock file, and its rename is atomic. A claim of a dead process is moved
// aside, and put back when it turns out to be live. A claimant checks that the
// claim is still its own before it renames it, so a claim that another process
// took away only makes the claimant try again.
function takeOver(file, stale, tmp, mine) {
  const claim = `${file}.takeover-${stale.split("\n")[1] ?? ""}`;
  try {
    fs.linkSync(tmp, claim);
  } catch (err) {
    if (err.code !== "EEXIST") throw err;
    const holder = readLock(claim);
    if (holder !== null && !running(holder)) removeDead(claim, holder);
    else sleep(2);
    return false;
  }
  if (readLock(file) !== stale || readLock(claim) !== mine) {
    fs.rmSync(claim, { force: true });
    return false;
  }
  try {
    fs.renameSync(claim, file);
  } catch (err) {
    if (err.code !== "ENOENT") throw err;
    return false;
  }
  return true;
}

// removeDead removes the claim whose text was holder, the claim of a dead process.
// The rename takes it out of the way at once; a claim that is not the dead one
// goes back by a hard link, which fails when its owner already made another.
function removeDead(claim, holder) {
  const aside = `${claim}.dead-${crypto.randomBytes(8).toString("hex")}`;
  try {
    fs.renameSync(claim, aside);
  } catch (err) {
    if (err.code === "ENOENT") return;
    throw err;
  }
  if (readLock(aside) !== holder) {
    try {
      fs.linkSync(aside, claim);
    } catch (err) {
      if (err.code !== "EEXIST") throw err;
    }
  }
  fs.rmSync(aside, { force: true });
}

// acquireStoreLock makes dir exclusive to this process. The lock file
// <dir>/.embed.lock holds the pid and a random token. It appears whole, by a
// hard link of a private file, so no process reads a partial lock. When the pid
// in the file is dead, the processes that found it race for one claim on it
// (takeOver), and the winner replaces the file. The returned function removes the
// file only when it still holds the token of this process.
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
      const text = readLock(file);
      if (text === null) continue;
      if (running(text)) throw new Error(`store directory ${dir} is in use by process ${pidOf(text)}`);
      if (takeOver(file, text, tmp, mine)) return release;
    }
  } finally {
    fs.rmSync(tmp, { force: true });
  }
}
