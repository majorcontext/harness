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

export interface CreateHarnessOptions {
  transport: WasmTransport;
  /** The config.Config JSON. It holds no key. */
  config?: object;
  /** A same-origin proxy: baseURL fills every provider that sets none; headers go on each model request. */
  proxy?: { baseURL?: string | URL; headers?: Record<string, string> };
  /** A page fetch that carries the model requests. */
  fetch?: (url: string, init: { method: string; headers: Record<string, string>; body?: Uint8Array; signal: AbortSignal }) => Promise<Response>;
  tools?: PageTool[];
  /** The allowed tool names of every session. Absent: every tool. */
  builtinTools?: string[];
  /** Under Node, dir is a directory that one process holds. Without dir the store is in memory. */
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

/** wasm names the WebAssembly transport. worker must be true. */
export function wasm(options: {
  module: WasmTransport["module"];
  wasmExec: WasmTransport["wasmExec"];
  worker?: boolean;
}): WasmTransport;

/** createHarness starts the module in a Worker and resolves when it is ready. */
export function createHarness(options: CreateHarnessOptions): Promise<Harness>;
