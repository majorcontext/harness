// A just-bash IFileSystem over the file system handle of the harness (h.fs).
// just-bash mounts it at a directory and passes paths relative to the mount
// point, so root maps them back into the namespace of the module.
const encoder = new TextEncoder();
const decoder = new TextDecoder();

function normalize(path) {
  const out = [];
  for (const part of path.split("/")) {
    if (part === "..") out.pop();
    else if (part && part !== ".") out.push(part);
  }
  return "/" + out.join("/");
}

function codeError(code, message, path) {
  return Object.assign(new Error(`${code}: ${message}, ${path}`), { code });
}

const bytesOf = (content) => (typeof content === "string" ? encoder.encode(content) : content);

/** shellFs returns the just-bash file system over fs, rooted at the directory root of the module. */
export function shellFs(fs, root) {
  const at = (path) => {
    const p = normalize(path);
    return p === "/" ? root : root + p;
  };

  const stat = async (path) => {
    const p = normalize(path);
    const s = await fs.stat(at(p));
    return {
      isFile: !s.isDirectory,
      isDirectory: s.isDirectory,
      isSymbolicLink: false,
      mode: s.isDirectory ? 0o755 : 0o644,
      size: s.size,
      mtime: new Date(s.mtimeMs),
      identity: "harness:" + at(p),
    };
  };

  const exists = async (path) => {
    try {
      await fs.stat(at(path));
      return true;
    } catch {
      return false;
    }
  };

  const readdir = async (path) => (await fs.readdir(at(path))).sort();

  async function cp(src, dest, options) {
    const s = await stat(src);
    if (!s.isDirectory) {
      await fs.writeFile(at(dest), await fs.readFile(at(src)));
      return;
    }
    if (!options?.recursive) throw codeError("EISDIR", "is a directory", src);
    await fs.mkdir(at(dest), { recursive: true });
    for (const name of await readdir(src)) await cp(`${src}/${name}`, `${dest}/${name}`, options);
  }

  return {
    async readFile(path) {
      return decoder.decode(await fs.readFile(at(path)));
    },
    readFileBuffer: (path) => fs.readFile(at(path)),
    writeFile: (path, content) => fs.writeFile(at(path), bytesOf(content)),
    appendFile: (path, content) => fs.writeFile(at(path), bytesOf(content), { append: true }),
    exists,
    stat,
    lstat: stat,
    mkdir: (path, options) => fs.mkdir(at(path), { recursive: Boolean(options?.recursive) }),
    rm: (path, options) => fs.rm(at(path), { recursive: Boolean(options?.recursive), force: Boolean(options?.force) }),
    cp,
    readdir,
    async readdirWithFileTypes(path) {
      const p = normalize(path);
      const out = [];
      for (const name of await readdir(p)) {
        const s = await stat(`${p}/${name}`);
        out.push({ name, isFile: s.isFile, isDirectory: s.isDirectory, isSymbolicLink: false });
      }
      return out;
    },
    async mv(src, dest) {
      const s = await stat(src);
      try {
        await fs.rename(at(src), at(dest));
      } catch (err) {
        if (!s.isDirectory) throw err;
        await cp(src, dest, { recursive: true });
        await fs.rm(at(src), { recursive: true });
      }
    },
    resolvePath: (base, path) => normalize(path.startsWith("/") ? path : `${base}/${path}`),
    getAllPaths: () => [],
    chmod: async (path) => void (await stat(path)),
    utimes: async (path) => void (await stat(path)),
    realpath: async (path) => (await stat(path), normalize(path)),
    symlink: async (_target, link) => {
      throw codeError("ENOSYS", "symbolic links are not supported", link);
    },
    link: async (_existing, link) => {
      throw codeError("ENOSYS", "hard links are not supported", link);
    },
    readlink: async (path) => {
      throw codeError("EINVAL", "not a symbolic link", path);
    },
    async createExclusive(path, options) {
      if (await exists(path)) throw codeError("EEXIST", "file exists", path);
      if (options?.directory) await fs.mkdir(at(path));
      else await fs.writeFile(at(path), new Uint8Array(0));
    },
  };
}
