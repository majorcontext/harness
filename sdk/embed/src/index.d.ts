/** An ACP message: a parsed JSON-RPC object. */
export type AcpMessage = Record<string, unknown>;

/** A tool of the page. run returns a string, {text, isError}, or a promise of either. */
export interface PageTool {
  name: string;
  description: string;
  schema?: object | string;
  run(input: any, options: { signal: AbortSignal }): unknown;
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
  /** The config.Config JSON. It holds no key. With subprocess it replaces the user config file of the process. */
  config?: object;
  /** A same-origin proxy: baseURL fills every provider that sets none; headers go only on requests to the origin of baseURL. */
  proxy?: { baseURL?: string | URL; headers?: Record<string, string> };
  /** A page fetch that carries the model requests. */
  fetch?: (url: string, init: { method: string; headers: Record<string, string>; body?: Uint8Array; signal: AbortSignal }) => Promise<Response>;
  tools?: PageTool[];
  /** The allowed tool names of every session. Absent: defaultBuiltinTools and the names of the page tools with wasm, and the tool set of the binary with subprocess. A list that the caller passes is used as it is: page tools are not added to it. */
  builtinTools?: string[];
  /**
   * Under Node, dir is a directory that one process holds. In the browser, persist keeps the sessions in
   * the origin private file system below dir (default /persist/sessions), and the promise rejects with code "store_in_use" after 2 s while
   * another page of the origin holds the store. Otherwise the store is in memory.
   * With subprocess, dir is the session directory of the process (it sets HARNESS_SESSION_DIR); the process holds its own lock, and without dir it uses its default directory.
   */
  store?: { persist?: boolean; dir?: string };
  /** Receives each JSON log line of the module. */
  log?: (line: string) => void;
}

export interface Harness {
  /** ACP messages. It has no backpressure in either direction. */
  stream: { readable: ReadableStream<AcpMessage>; writable: WritableStream<AcpMessage> };
  /** Ends the connection and the Worker. A second call returns the same promise. */
  close(): Promise<void>;
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
