// The shell tool of a browser page: a just-bash shell over the file system of
// the harness module. A page that does not import this file loads nothing of
// it, and just-bash itself loads at the first call of the tool.
import { shellFs } from "./shellfs.mjs";

const description =
  "Runs a bash command in a sandboxed shell: a JavaScript interpreter, not an operating system. " +
  "The working directory is /work. /work and /tmp hold the same files as read_file, write_file, ls, glob, and grep. " +
  "It runs pipes, redirects, grep, sed, awk, jq, sort, find, cat, head, tail, wc, and similar commands. " +
  "It has no network, no processes, no git, no tar, and no symbolic links. " +
  "Each call starts a new shell: cd and variables do not last to the next call. " +
  "A long or endless command stops at a limit.";

const schema = { type: "object", properties: { command: { type: "string", description: "The command line." } }, required: ["command"] };

const perFs = new WeakMap();

function clip(text, max) {
  if (text.length <= max) return text;
  const half = Math.floor(max / 2);
  return `${text.slice(0, half)}\n[... ${text.length - max} characters cut ...]\n${text.slice(-half)}`;
}

async function isDirectory(fs, path) {
  try {
    return (await fs.stat(path)).isDirectory;
  } catch {
    return false;
  }
}

async function createBash(fs, { load, maxCommands, timeoutMs }) {
  const { Bash, InMemoryFs, MountableFs } = await load();
  const mounts = [{ mountPoint: "/work", filesystem: shellFs(fs, "/work") }];
  if (await isDirectory(fs, "/tmp")) mounts.push({ mountPoint: "/tmp", filesystem: shellFs(fs, "/tmp") });
  return new Bash({
    fs: new MountableFs({ base: new InMemoryFs(), mounts }),
    cwd: "/work",
    executionLimits: { maxCommandCount: maxCommands, maxLoopIterations: maxCommands, maxExecutionTimeMs: timeoutMs },
  });
}

/**
 * shellTool returns a page tool that runs a just-bash shell over h.fs. Pass it
 * in the tools of createHarness; it takes the file system handle from the
 * harness at each call, so it needs no reference to the harness. name is the
 * tool name (default bash). load returns the just-bash module and defaults to
 * import("just-bash/browser"), which a bundler or an import map resolves.
 * timeoutMs and maxCommands limit one call; maxOutput clips stdout and stderr
 * each. The shell has no network.
 */
export function shellTool({ name = "bash", load = () => import("just-bash/browser"), timeoutMs = 10000, maxCommands = 10000, maxOutput = 20000 } = {}) {
  return {
    name,
    description,
    schema,
    async run(input, { signal, fs } = {}) {
      if (!fs) throw new Error("the shell tool needs the file system of the harness, which only the wasm transport in a browser provides");
      if (typeof input?.command !== "string") throw new Error("command must be a string");
      let bash = perFs.get(fs);
      if (!bash) {
        bash = createBash(fs, { load, maxCommands, timeoutMs });
        perFs.set(fs, bash);
        bash.catch(() => perFs.delete(fs));
      }
      const result = await (await bash).exec(input.command, { signal });
      const stderr = result.stderr ? `\n[stderr]\n${clip(result.stderr, maxOutput)}` : "";
      return { text: `${clip(result.stdout, maxOutput)}${stderr}\n[exit ${result.exitCode}]`, isError: result.exitCode !== 0 };
    },
  };
}
