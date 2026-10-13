import fs from "node:fs";
import path from "node:path";

function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return err.code === "EPERM";
  }
}

// acquireStoreLock makes dir exclusive to this process: it creates
// <dir>/.embed.lock with O_EXCL and the pid, and refuses when that pid is
// alive. Two processes that find the same stale file can both replace it. It returns a function that removes the file.
export function acquireStoreLock(dir) {
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, ".embed.lock");
  for (;;) {
    try {
      fs.writeFileSync(file, String(process.pid), { flag: "wx" });
      return () => fs.rmSync(file, { force: true });
    } catch (err) {
      if (err.code !== "EEXIST") throw err;
    }
    const pid = Number.parseInt(fs.readFileSync(file, "utf8"), 10);
    if (pid > 0 && alive(pid)) {
      throw new Error(`store directory ${dir} is in use by process ${pid}`);
    }
    fs.rmSync(file, { force: true });
  }
}
