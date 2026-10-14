/** An ACP message: a parsed JSON-RPC object. */
export type AcpMessage = Record<string, unknown>;

/** A tool of the page. run returns a string, {text, isError}, or a promise of either. fs is h.fs; it is absent where the harness has none. */
export interface PageTool {
  name: string;
  description: string;
  schema?: object | string;
  run(input: any, options: { signal: AbortSignal; fs?: HarnessFs }): unknown;
}

/** What stat returns. */
export interface FsStat {
  isDirectory: boolean;
  size: number;
  mtimeMs: number;
}

/**
 * The file system of the module, which the file tools and the Go store use: /work, /tmp, and /persist.
 * Paths are absolute. A call rejects with an Error whose code is a POSIX name such as ENOENT, EEXIST, or ENOTEMPTY.
 * The calls are not atomic: a write of the page and a write of the module to one file can interleave.
 */
export interface HarnessFs {
  readFile(path: string): Promise<Uint8Array>;
  /** data is a string (UTF-8) or bytes. It creates the file and truncates it, or appends with append. */
  writeFile(path: string, data: string | ArrayBufferView | ArrayBuffer, options?: { append?: boolean }): Promise<void>;
  readdir(path: string): Promise<string[]>;
  stat(path: string): Promise<FsStat>;
  /** recursive creates the missing parents and accepts a directory that exists. */
  mkdir(path: string, options?: { recursive?: boolean }): Promise<void>;
  /** A directory needs recursive unless it is empty. force ignores a path that does not exist. */
  rm(path: string, options?: { recursive?: boolean; force?: boolean }): Promise<void>;
  /** The module cannot rename a directory below /persist. */
  rename(from: string, to: string): Promise<void>;
}

/** The transport that wasm() returns. */
export interface WasmTransport {
  kind: "wasm";
  module: string | URL | ArrayBuffer | ArrayBufferView | WebAssembly.Module;
  wasmExec: string | URL;
  worker: boolean;
}

/** The transport that subprocess() returns. */
export interface SubprocessTransport {
  kind: "subprocess";
  command: string;
  args: string[];
  env: Record<string, string>;
  cwd?: string;
  closeTimeout: number;
}

export interface CreateHarnessOptions {
  /** wasm() or subprocess(). subprocess refuses tools, builtinTools, proxy, and fetch. */
  transport: WasmTransport | SubprocessTransport;
  /** The config.Config JSON. It holds no key. With subprocess it is the user config file of the process; the project config still merges over it. */
  config?: object;
  /**
   * A same-origin proxy: baseURL fills every provider that sets none; headers go only on requests to the origin of baseURL.
   * providerBaseURLs maps a provider name to the base_url of its entry, also of a native provider such as codex, so a page sends
   * the ChatGPT backend traffic through a same-origin pass-through path that holds no credential. In a browser, the URL of a provider that uses the ChatGPT login must have the origin of the page, or the first session fails.
   */
  proxy?: { baseURL?: string | URL; headers?: Record<string, string>; providerBaseURLs?: Record<string, string | URL> };
  /** A page fetch that carries the model requests. */
  fetch?: (url: string, init: { method: string; headers: Record<string, string>; body?: Uint8Array; signal: AbortSignal }) => Promise<Response>;
  tools?: PageTool[];
  /** The allowed tool names of every session. Absent: defaultBuiltinTools and the names of the page tools with wasm, and the tool set of the binary with subprocess. A list that the caller passes is used as it is: page tools are not added to it. */
  builtinTools?: string[];
  /**
   * Under Node, dir is a directory that one process holds. In the browser, persist keeps the sessions in
   * the origin private file system below dir (default /persist/sessions), and the promise rejects with code "store_in_use" after 2 s while
   * another page of the origin holds the store. Otherwise the store is in memory.
   * With subprocess, dir is the session directory of the process (it sets HARNESS_SESSION_DIR); the process holds its own lock, and without dir the sessions go to its default directory, not to memory.
   */
  store?: { persist?: boolean; dir?: string };
  /** Receives each JSON log line of the module. */
  log?: (line: string) => void;
}

export interface Harness {
  /** ACP messages. It has no backpressure in either direction. */
  stream: { readable: ReadableStream<AcpMessage>; writable: WritableStream<AcpMessage> };
  /** Ends the connection and the Worker or the process. A second call returns the same promise. */
  close(): Promise<void>;
  /** The file system of the module. It exists only with the wasm transport in a browser; under Node, and with subprocess, it is undefined. Every pending call rejects at close(). */
  fs?: HarnessFs;
}

/** The tool set of a session when createHarness gets no builtinTools, before the names of the page tools. */
export const defaultBuiltinTools: readonly string[];

/** wasm names the WebAssembly transport. worker must be true. */
export function wasm(options: {
  module: WasmTransport["module"];
  wasmExec: WasmTransport["wasmExec"];
  worker?: boolean;
}): WasmTransport;

/**
 * subprocess names the native transport: command with args is `harness acp`. Node only. env is added to the environment of this process.
 * closeTimeout (ms, default 5000) is how long close() waits for the process to exit after each step: the end of its input, SIGTERM, SIGKILL.
 * close() rejects when the process exits with a status other than 0, dies from a signal, or needs a signal from close().
 */
export function subprocess(options: { command: string; args?: string[]; env?: Record<string, string>; cwd?: string; closeTimeout?: number }): SubprocessTransport;

/** createHarness starts the transport and resolves when it is ready. It rejects when the transport cannot honor an option. */
export function createHarness(options: CreateHarnessOptions): Promise<Harness>;
