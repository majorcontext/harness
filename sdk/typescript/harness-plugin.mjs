// harness-plugin.mjs — zero-dependency Node.js ESM SDK for harness plugins.
//
// Plugins are separate processes speaking JSON-RPC 2.0 over stdio, one
// message per line (NDJSON). This module is the TypeScript/JavaScript
// counterpart of the Go SDK (github.com/majorcontext/harness/plugin); see
// plugin/PROTOCOL.md in the harness repo for the versioned wire spec that
// this file implements. Log to stderr — stdout belongs to the protocol.
//
// Zero npm dependencies: only Node.js built-ins (node:readline, node:stream)
// are used, so plugins can ship as a single file plus this SDK file.
//
// @module harness-plugin

import { createInterface } from 'node:readline';

/** The hook protocol version implemented by this SDK. */
export const PROTOCOL_VERSION = 1;

const CODE_METHOD_NOT_FOUND = -32601;
const CODE_INTERNAL_ERROR = -32603;

const METHOD_INITIALIZE = 'initialize';
const METHOD_SHUTDOWN = 'shutdown';
const METHOD_TOOL_EXECUTE = 'tool/execute';
const HOOK_METHOD_PREFIX = 'hook/';

const METHOD_SESSION_MESSAGES = 'client/session.messages';
const METHOD_MCP_CALL = 'client/mcp.call';
const METHOD_GENERATE = 'client/generate';

/**
 * @typedef {Object} ToolDef
 * @property {string} name
 * @property {string} description
 * @property {Object} inputSchema JSON Schema for the tool's arguments.
 */

/**
 * @typedef {Object} Tool
 * @property {ToolDef} def
 * @property {(ctx: PluginContext, args: any) => Promise<Part[]>|Part[]} execute
 *   Called for `tool/execute`. Return canonical message.Parts (plain
 *   objects, e.g. `[{type: 'text', text: '...'}]`). Throwing turns into an
 *   `is_error` tool result automatically — it is never a protocol error.
 */

/**
 * @typedef {Object} Part
 * @property {string} type One of "text", "blob", "tool_call", "tool_result", "reasoning".
 */

/**
 * @typedef {Object} InitializeParams
 * @property {number} protocol_version
 * @property {string} [harness_version]
 * @property {string} [workspace_dir]
 * @property {Object<string,string>} [http_headers] Headers the harness wants
 *   stamped on all plugin outbound HTTP traffic.
 * @property {any} [config] This plugin's config block from the harness
 *   config file, verbatim (already JSON-decoded).
 */

/**
 * @typedef {Object} Hooks
 * @property {(ctx: PluginContext, events: object[]) => void|Promise<void>} [onEvent]
 *   Async, fire-and-forget: the event stream, batched.
 * @property {(ctx: PluginContext, req: {session_id: string, params: object}) => (object|void|Promise<object|void>)} [chatParams]
 *   Mutates model request parameters. Return `{params}` or nothing.
 * @property {(ctx: PluginContext, req: {session_id: string, message: object}) => (object|void|Promise<object|void>)} [chatMessage]
 *   Mutates a message before it enters the session log. Return `{message}` or nothing.
 * @property {(ctx: PluginContext, req: {session_id: string, model?: object}) => ({segments?: string[]}|void|Promise<{segments?: string[]}|void>)} [systemTransform]
 *   Additive: return `{segments: [...]}` to append to the system prompt.
 * @property {(ctx: PluginContext, req: {session_id: string, tool: string, command: string, dir?: string}) => ({env?: object}|void|Promise<{env?: object}|void>)} [shellEnv]
 *   Return `{env: {...}}` to merge into the command's environment.
 * @property {(ctx: PluginContext, req: {session_id: string, call_id: string, tool: string, args: any}) => ({args?: any, deny?: string}|void|Promise<{args?: any, deny?: string}|void>)} [toolExecuteBefore]
 *   Return `{args}` to rewrite arguments, or `{deny: "message"}` to block the call.
 * @property {(ctx: PluginContext, req: {session_id: string, call_id: string, tool: string, args: any, output: Part[]}) => ({output?: Part[]}|void|Promise<{output?: Part[]}|void>)} [toolExecuteAfter]
 *   Return `{output}` to replace the tool output.
 */

/**
 * @typedef {Object} PluginDefinition
 * @property {string} name Required, must match the harness config.
 * @property {string} [version]
 * @property {Hooks} [hooks]
 * @property {Tool[]} [tools]
 */

class RpcError extends Error {
  constructor(code, message) {
    super(message);
    this.name = 'RpcError';
    this.code = code;
  }
}

/** Maps Hooks object keys to their wire hook names, in Hooks field order. */
const HOOK_WIRE_NAMES = [
  ['onEvent', 'event'],
  ['chatParams', 'chat.params'],
  ['chatMessage', 'chat.message'],
  ['systemTransform', 'system.transform'],
  ['shellEnv', 'shell.env'],
  ['toolExecuteBefore', 'tool.execute.before'],
  ['toolExecuteAfter', 'tool.execute.after'],
];

function hookList(hooks) {
  const out = [];
  for (const [key, wire] of HOOK_WIRE_NAMES) {
    if (typeof hooks[key] === 'function') out.push(wire);
  }
  return out;
}

/**
 * PluginContext is the plugin's handle to the harness: the client API plus
 * the initialize-time environment. It mirrors the Go SDK's *Client and is
 * passed to every hook and tool invocation.
 */
export class PluginContext {
  /** @param {Connection} conn @param {InitializeParams} init */
  constructor(conn, init) {
    this._conn = conn;
    this._init = init || {};
  }

  /** The harness workspace (project) directory. */
  get workspaceDir() {
    return this._init.workspace_dir || '';
  }

  /** This plugin's config block from the harness config file (already parsed). */
  get config() {
    return this._init.config;
  }

  /** Headers the harness wants stamped on all outbound HTTP traffic. */
  get httpHeaders() {
    return this._init.http_headers || {};
  }

  /**
   * Fetch wrapper that stamps httpHeaders on every request, mirroring the Go
   * SDK's Client.HTTPClient(). Existing headers in `init` win on conflicts.
   * @param {string|URL} url
   * @param {RequestInit} [init]
   */
  async fetch(url, init = {}) {
    const headers = { ...this.httpHeaders, ...(init.headers || {}) };
    return fetch(url, { ...init, headers });
  }

  /**
   * Returns the canonical message history for a session.
   * @param {string} sessionId
   */
  async sessionMessages(sessionId) {
    const resp = await this._conn.call(METHOD_SESSION_MESSAGES, { session_id: sessionId });
    return (resp && resp.messages) || [];
  }

  /**
   * Invokes a tool on one of the harness's configured MCP servers.
   * @param {string} server
   * @param {string} tool
   * @param {any} [args]
   */
  async mcpCall(server, tool, args) {
    return this._conn.call(METHOD_MCP_CALL, { server, tool, args });
  }

  /**
   * Makes an LLM call through the harness provider layer: plugins inherit
   * model routing, credentials, and observability, and never carry API
   * keys.
   * @param {{model: string, system?: string, messages: object[], max_tokens?: number}} req
   */
  async generate(req) {
    const resp = await this._conn.call(METHOD_GENERATE, req);
    return resp && resp.message;
  }
}

/**
 * Connection is a bidirectional JSON-RPC 2.0 connection over NDJSON lines.
 * Incoming requests are served without blocking reads, so a hook that calls
 * back into the host (e.g. sessionMessages) while in flight works exactly
 * like the Go SDK.
 */
class Connection {
  /**
   * @param {(line: string) => void} write Writes one already-terminated NDJSON line.
   * @param {(method: string, params: any, conn: Connection) => Promise<any>} handle
   */
  constructor(write, handle) {
    this._write = write;
    this._handle = handle;
    this._nextId = 0;
    this._pending = new Map();
    this._closed = false;
    this._closeErr = undefined;
  }

  handleLine(line) {
    if (!line || !line.trim()) return;
    let msg;
    try {
      msg = JSON.parse(line);
    } catch {
      return; // malformed lines are dropped, matching the Go SDK
    }
    if (msg.method) {
      this._serveRequest(msg);
      return;
    }
    if (msg.id === undefined || msg.id === null) return;
    const pending = this._pending.get(msg.id);
    if (!pending) return;
    this._pending.delete(msg.id);
    if (msg.error) pending.reject(new RpcError(msg.error.code, msg.error.message));
    else pending.resolve(msg.result);
  }

  async _serveRequest(msg) {
    let result;
    let err;
    try {
      result = await this._handle(msg.method, msg.params === undefined ? null : msg.params, this);
    } catch (e) {
      err = e;
    }
    if (msg.id === undefined || msg.id === null) return; // notification: no response
    const resp = { jsonrpc: '2.0', id: msg.id };
    if (err) {
      resp.error =
        err instanceof RpcError
          ? { code: err.code, message: err.message }
          : { code: CODE_INTERNAL_ERROR, message: String((err && err.message) || err) };
    } else {
      resp.result = result === undefined || result === null ? {} : result;
    }
    this._write(JSON.stringify(resp));
  }

  /**
   * Sends a request and resolves with the peer's result.
   * @param {string} method @param {any} params
   */
  call(method, params) {
    const id = ++this._nextId;
    return new Promise((resolve, reject) => {
      if (this._closed) {
        reject(new Error(`harness-plugin: connection closed: ${this._closeErr}`));
        return;
      }
      this._pending.set(id, { resolve, reject });
      this._write(JSON.stringify({ jsonrpc: '2.0', id, method, params: params === undefined ? {} : params }));
    });
  }

  /** Sends a notification (no response expected). @param {string} method @param {any} params */
  notify(method, params) {
    this._write(JSON.stringify({ jsonrpc: '2.0', method, params: params === undefined ? {} : params }));
  }

  fail(err) {
    if (this._closed) return;
    this._closed = true;
    this._closeErr = err;
    for (const [, p] of this._pending) p.reject(err);
    this._pending.clear();
  }
}

class PluginServer {
  /** @param {PluginDefinition} definition */
  constructor(definition) {
    this.name = definition.name;
    this.version = definition.version;
    this.hooks = definition.hooks || {};
    this.toolsByName = new Map();
    for (const t of definition.tools || []) {
      this.toolsByName.set(t.def.name, t);
    }
    this.ctx = null;
    this.shuttingDown = false;
  }

  manifest() {
    return {
      name: this.name,
      version: this.version,
      protocol_version: PROTOCOL_VERSION,
      hooks: hookList(this.hooks),
      tools: [...this.toolsByName.values()].map((t) => ({
        name: t.def.name,
        description: t.def.description,
        input_schema: t.def.inputSchema,
      })),
    };
  }

  /** @param {string} method @param {any} params @param {Connection} conn */
  async handle(method, params, conn) {
    switch (method) {
      case METHOD_INITIALIZE: {
        const init = params || {};
        if (init.protocol_version !== PROTOCOL_VERSION) {
          throw new RpcError(
            CODE_INTERNAL_ERROR,
            `plugin: protocol version mismatch: harness=${init.protocol_version} plugin=${PROTOCOL_VERSION}`,
          );
        }
        this.ctx = new PluginContext(conn, init);
        return this.manifest();
      }

      case METHOD_SHUTDOWN:
        this.shuttingDown = true;
        return undefined;

      case METHOD_TOOL_EXECUTE: {
        const tool = this.toolsByName.get(params.tool);
        if (!tool) {
          throw new RpcError(CODE_METHOD_NOT_FOUND, `unknown tool "${params.tool}"`);
        }
        try {
          const output = (await tool.execute(this.ctx, params.args)) || [];
          return { output };
        } catch (e) {
          // Tool errors go back to the model as error results, not as
          // protocol failures — matching the Go SDK.
          return {
            output: [{ type: 'text', text: String((e && e.message) || e) }],
            is_error: true,
          };
        }
      }

      case HOOK_METHOD_PREFIX + 'event': {
        if (!this.hooks.onEvent) return undefined;
        await this.hooks.onEvent(this.ctx, (params && params.events) || []);
        return undefined;
      }

      case HOOK_METHOD_PREFIX + 'chat.params':
        return this._dispatchHook(this.hooks.chatParams, params);
      case HOOK_METHOD_PREFIX + 'chat.message':
        return this._dispatchHook(this.hooks.chatMessage, params);
      case HOOK_METHOD_PREFIX + 'system.transform':
        return this._dispatchHook(this.hooks.systemTransform, params);
      case HOOK_METHOD_PREFIX + 'shell.env':
        return this._dispatchHook(this.hooks.shellEnv, params);
      case HOOK_METHOD_PREFIX + 'tool.execute.before':
        return this._dispatchHook(this.hooks.toolExecuteBefore, params);
      case HOOK_METHOD_PREFIX + 'tool.execute.after':
        return this._dispatchHook(this.hooks.toolExecuteAfter, params);

      default:
        throw new RpcError(CODE_METHOD_NOT_FOUND, `unknown method "${method}"`);
    }
  }

  async _dispatchHook(fn, params) {
    if (!fn) {
      throw new RpcError(CODE_METHOD_NOT_FOUND, 'hook not subscribed');
    }
    const resp = await fn(this.ctx, params || {});
    // A nil/undefined response means "no changes"; send an empty object,
    // matching the Go SDK.
    return resp || {};
  }
}

/**
 * A running plugin instance, returned by createPlugin(). Call .run() to
 * start speaking the protocol.
 */
class Plugin {
  /** @param {PluginDefinition} definition */
  constructor(definition) {
    if (!definition || !definition.name) {
      throw new Error('harness-plugin: manifest name is required');
    }
    this.definition = definition;
  }

  /**
   * Runs the plugin: reads NDJSON requests from `input` (default:
   * process.stdin), serves them, and writes NDJSON responses to `output`
   * (default: process.stdout) until the harness shuts the plugin down or
   * the input stream ends. Log to stderr — stdout belongs to the protocol.
   *
   * @param {Object} [options]
   * @param {NodeJS.ReadableStream} [options.input]
   * @param {NodeJS.WritableStream} [options.output]
   * @param {boolean} [options.exitProcess] Call process.exit() when the
   *   stream ends (default: true). Set to false in tests / embedders.
   * @returns {Promise<void>} Resolves when the connection closes.
   */
  run(options = {}) {
    const input = options.input || process.stdin;
    const output = options.output || process.stdout;
    const exitProcess = options.exitProcess !== false;

    const server = new PluginServer(this.definition);
    const write = (line) => {
      output.write(line + '\n');
    };
    const conn = new Connection(write, (method, params, c) => server.handle(method, params, c));

    const rl = createInterface({ input, crlfDelay: Infinity });

    return new Promise((resolve, reject) => {
      rl.on('line', (line) => conn.handleLine(line));
      rl.on('close', () => {
        conn.fail(new Error('stdin closed'));
        if (exitProcess) {
          process.exit(server.shuttingDown ? 0 : 1);
        }
        resolve();
      });
      input.on('error', (err) => {
        conn.fail(err);
        if (exitProcess) {
          process.exit(1);
        }
        reject(err);
      });
    });
  }
}

/**
 * Creates a harness plugin from a definition. Only `name` is required; the
 * manifest's hook list and tool list are derived automatically.
 *
 * @example
 * import { createPlugin } from './harness-plugin.mjs';
 *
 * createPlugin({
 *   name: 'my-plugin',
 *   version: '0.1.0',
 *   hooks: {
 *     systemTransform: async () => ({ segments: ['Be concise.'] }),
 *   },
 * }).run();
 *
 * @param {PluginDefinition} definition
 * @returns {Plugin}
 */
export function createPlugin(definition) {
  return new Plugin(definition);
}
