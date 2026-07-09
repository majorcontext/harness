// Unit tests for the NDJSON framing + handshake layer, driven by a scripted
// fake host (no real harness process involved). Run with `node --test`.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PassThrough, Writable } from 'node:stream';
import { createPlugin, PROTOCOL_VERSION } from '../harness-plugin.mjs';

/**
 * FakeHost drives a plugin's run() over a pair of in-memory streams, exactly
 * as the real harness drives a plugin process over stdio: it writes NDJSON
 * requests/notifications to the plugin's "stdin" and reads NDJSON
 * responses/requests from the plugin's "stdout".
 */
class FakeHost {
  constructor() {
    this.toPlugin = new PassThrough(); // host writes here; plugin reads
    this.fromPlugin = new PassThrough(); // plugin writes here; host reads
    this._buf = '';
    this._waiters = [];
    this.fromPlugin.on('data', (chunk) => {
      this._buf += chunk.toString('utf8');
      let idx;
      while ((idx = this._buf.indexOf('\n')) !== -1) {
        const line = this._buf.slice(0, idx);
        this._buf = this._buf.slice(idx + 1);
        if (line.trim() === '') continue;
        const msg = JSON.parse(line);
        const waiter = this._waiters.shift();
        if (waiter) waiter(msg);
        else this._queued = (this._queued || []).concat([msg]);
      }
    });
  }

  send(msg) {
    this.toPlugin.write(JSON.stringify(msg) + '\n');
  }

  /** Resolves with the next message the plugin writes to stdout. */
  next() {
    if (this._queued && this._queued.length) {
      return Promise.resolve(this._queued.shift());
    }
    return new Promise((resolve) => this._waiters.push(resolve));
  }

  closeStdin() {
    this.toPlugin.end();
  }
}

function startPlugin(definition, host) {
  const plugin = createPlugin(definition);
  const done = plugin.run({
    input: host.toPlugin,
    output: host.fromPlugin,
    exitProcess: false,
  });
  return done;
}

test('initialize handshake returns a well-formed manifest', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    {
      name: 'test-plugin',
      version: '0.0.1',
      hooks: {
        systemTransform: async () => ({ segments: ['hi'] }),
        toolExecuteAfter: async (_ctx, req) => ({ output: req.output }),
      },
      tools: [
        {
          def: {
            name: 'echo',
            description: 'echoes',
            inputSchema: { type: 'object', properties: {} },
          },
          execute: async () => [{ type: 'text', text: 'echo' }],
        },
      ],
    },
    host,
  );

  host.send({
    jsonrpc: '2.0',
    id: 1,
    method: 'initialize',
    params: {
      protocol_version: PROTOCOL_VERSION,
      harness_version: 'test',
      workspace_dir: '/tmp/ws',
      http_headers: { 'X-Test': '1' },
      config: { foo: 'bar' },
    },
  });

  const resp = await host.next();
  assert.equal(resp.id, 1);
  assert.equal(resp.error, undefined);
  const manifest = resp.result;
  assert.equal(manifest.name, 'test-plugin');
  assert.equal(manifest.version, '0.0.1');
  assert.equal(manifest.protocol_version, PROTOCOL_VERSION);
  assert.deepEqual(manifest.hooks.sort(), ['system.transform', 'tool.execute.after']);
  assert.equal(manifest.tools.length, 1);
  assert.equal(manifest.tools[0].name, 'echo');
  assert.equal(manifest.tools[0].description, 'echoes');
  assert.deepEqual(manifest.tools[0].input_schema, { type: 'object', properties: {} });

  host.send({ jsonrpc: '2.0', method: 'shutdown' });
  host.closeStdin();
  await done;
});

test('protocol version mismatch is rejected as an initialize error', async () => {
  const host = new FakeHost();
  const done = startPlugin({ name: 'mismatched' }, host);

  host.send({
    jsonrpc: '2.0',
    id: 1,
    method: 'initialize',
    params: { protocol_version: PROTOCOL_VERSION + 99 },
  });

  const resp = await host.next();
  assert.equal(resp.id, 1);
  assert.ok(resp.error, 'expected an error response');
  assert.match(resp.error.message, /protocol version mismatch/);

  host.closeStdin();
  await done;
});

test('unsubscribed hooks respond with method-not-found', async () => {
  const host = new FakeHost();
  const done = startPlugin({ name: 'no-hooks' }, host);

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', id: 2, method: 'hook/system.transform', params: { session_id: 's1' } });
  const resp = await host.next();
  assert.equal(resp.id, 2);
  assert.ok(resp.error);
  assert.equal(resp.error.code, -32601);

  host.closeStdin();
  await done;
});

test('unknown method responds with method-not-found', async () => {
  const host = new FakeHost();
  const done = startPlugin({ name: 'no-hooks' }, host);

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', id: 2, method: 'not/a/real/method', params: {} });
  const resp = await host.next();
  assert.equal(resp.error.code, -32601);

  host.closeStdin();
  await done;
});

test('a hook response of undefined/null becomes an empty-object response', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    { name: 'noop', hooks: { shellEnv: async () => undefined } },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', id: 2, method: 'hook/shell.env', params: { session_id: 's1', tool: 'bash', command: 'ls' } });
  const resp = await host.next();
  assert.deepEqual(resp.result, {});

  host.closeStdin();
  await done;
});

test('tool.execute.after hook mutation round trip', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    {
      name: 'redactor-ish',
      hooks: {
        toolExecuteAfter: async (_ctx, req) => ({
          output: req.output.map((p) => (p.type === 'text' ? { type: 'text', text: p.text.replace('SECRET', '***') } : p)),
        }),
      },
    },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({
    jsonrpc: '2.0',
    id: 2,
    method: 'hook/tool.execute.after',
    params: {
      session_id: 's1',
      call_id: 'c1',
      tool: 'bash',
      args: {},
      output: [{ type: 'text', text: 'token=SECRET' }],
    },
  });
  const resp = await host.next();
  assert.deepEqual(resp.result, { output: [{ type: 'text', text: 'token=***' }] });

  host.closeStdin();
  await done;
});

test('tool/execute runs a plugin-provided tool', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    {
      name: 'tooler',
      tools: [
        {
          def: { name: 'reverse', description: 'reverses text', inputSchema: {} },
          execute: async (_ctx, args) => [{ type: 'text', text: [...args.text].reverse().join('') }],
        },
      ],
    },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({
    jsonrpc: '2.0',
    id: 2,
    method: 'tool/execute',
    params: { session_id: 's1', call_id: 'c1', tool: 'reverse', args: { text: 'abc' } },
  });
  const resp = await host.next();
  assert.deepEqual(resp.result, { output: [{ type: 'text', text: 'cba' }] });

  host.closeStdin();
  await done;
});

test('tool/execute errors become is_error tool output, not protocol errors', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    {
      name: 'failer',
      tools: [
        {
          def: { name: 'boom', description: 'always fails', inputSchema: {} },
          execute: async () => {
            throw new Error('kaboom');
          },
        },
      ],
    },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({
    jsonrpc: '2.0',
    id: 2,
    method: 'tool/execute',
    params: { session_id: 's1', call_id: 'c1', tool: 'boom', args: {} },
  });
  const resp = await host.next();
  assert.equal(resp.error, undefined);
  assert.equal(resp.result.is_error, true);
  assert.equal(resp.result.output[0].type, 'text');
  assert.match(resp.result.output[0].text, /kaboom/);

  host.closeStdin();
  await done;
});

test('unknown tool name is a method-not-found error', async () => {
  const host = new FakeHost();
  const done = startPlugin({ name: 'no-tools' }, host);

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', id: 2, method: 'tool/execute', params: { tool: 'nope', args: {} } });
  const resp = await host.next();
  assert.equal(resp.error.code, -32601);

  host.closeStdin();
  await done;
});

test('hook/event notifications call onEvent with no response sent', async () => {
  const host = new FakeHost();
  let received;
  const done = startPlugin(
    {
      name: 'eventer',
      hooks: {
        onEvent: async (_ctx, events) => {
          received = events;
        },
      },
    },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', method: 'hook/event', params: { events: [{ type: 'session.status' }] } });

  // No response is expected for a notification; instead assert indirectly by
  // sending a follow-up request and confirming the event was processed by
  // the time it's answered (single-threaded event loop ordering).
  host.send({ jsonrpc: '2.0', id: 2, method: 'hook/shell.env', params: {} });
  const resp = await host.next();
  assert.equal(resp.id, 2);
  assert.deepEqual(received, [{ type: 'session.status' }]);

  host.closeStdin();
  await done;
});

test('a hook can call back into the host (client API) while in flight', async () => {
  const host = new FakeHost();
  const done = startPlugin(
    {
      name: 'caller',
      hooks: {
        systemTransform: async (ctx) => {
          const messages = await ctx.sessionMessages('s1');
          return { segments: [`saw ${messages.length} messages`] };
        },
      },
    },
    host,
  );

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', id: 2, method: 'hook/system.transform', params: { session_id: 's1' } });

  // The plugin should call back with client/session.messages before
  // answering the hook request.
  const clientReq = await host.next();
  assert.equal(clientReq.method, 'client/session.messages');
  assert.deepEqual(clientReq.params, { session_id: 's1' });

  host.send({
    jsonrpc: '2.0',
    id: clientReq.id,
    result: { messages: [{ id: 'm1', role: 'user', parts: [] }] },
  });

  const resp = await host.next();
  assert.equal(resp.id, 2);
  assert.deepEqual(resp.result, { segments: ['saw 1 messages'] });

  host.closeStdin();
  await done;
});

test('graceful shutdown: run() resolves once stdin closes after a shutdown notification', async () => {
  const host = new FakeHost();
  const done = startPlugin({ name: 'shutter' }, host);

  host.send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } });
  await host.next();

  host.send({ jsonrpc: '2.0', method: 'shutdown' });
  host.closeStdin();

  await assert.doesNotReject(done);
});

test('graceful shutdown drains pending stdout writes before exiting the process', async () => {
  // Regression test: process.exit() fired synchronously the instant a line
  // was handed to output.write() could race an asynchronous stream (a pipe
  // on some platforms does not complete writes on the same tick) and drop
  // the plugin's final response(s) to the host. This stub output stream
  // defers write completion to a later tick, exactly like that case.
  let writesCompleted = 0;
  const written = [];
  const output = new Writable({
    write(chunk, _enc, callback) {
      setImmediate(() => {
        written.push(chunk.toString('utf8'));
        writesCompleted++;
        callback();
      });
    },
  });
  const input = new PassThrough();

  const plugin = createPlugin({ name: 'flusher' });

  const origExit = process.exit;
  let exitCode;
  let writesCompletedAtExitTime;
  process.exit = (code) => {
    exitCode = code;
    writesCompletedAtExitTime = writesCompleted;
    // Do not actually terminate the test process.
  };

  let done;
  try {
    done = plugin.run({ input, output, exitProcess: true });

    input.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: PROTOCOL_VERSION } }) + '\n');
    // Let the (still in-flight, per the stub above) initialize response
    // get queued on `output` before shutdown/close race it.
    await new Promise((resolve) => setImmediate(resolve));

    input.write(JSON.stringify({ jsonrpc: '2.0', method: 'shutdown' }) + '\n');
    input.end();

    await done;
  } finally {
    process.exit = origExit;
  }

  assert.equal(exitCode, 0);
  assert.equal(
    writesCompletedAtExitTime,
    writesCompleted,
    'process.exit() must not fire until all queued stdout writes have completed',
  );
  assert.ok(written.join('').includes('"id":1'), 'the initialize response must have reached the output stream');
});
