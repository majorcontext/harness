import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PassThrough } from 'node:stream';
import { createPlugin } from '../harness-plugin.mjs';

async function manifestOf(definition) {
  const input = new PassThrough();
  const output = new PassThrough();
  const done = createPlugin(definition).run({ input, output, exitProcess: false });
  input.write(JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocol_version: 1 } }) + '\n');
  const line = await new Promise((resolve) => output.once('data', (b) => resolve(b.toString())));
  input.end();
  await done;
  return JSON.parse(line).result;
}

const noop = () => {};

test('denyOnFailure lists the wire hook in the manifest', async () => {
  const m = await manifestOf({ name: 'p', hooks: { toolExecuteBefore: noop }, denyOnFailure: ['toolExecuteBefore'] });
  assert.deepEqual(m.deny_on_failure, ['tool.execute.before']);
});

test('manifest omits deny_on_failure and hook_timeouts when unset', async () => {
  const m = await manifestOf({ name: 'p', hooks: { toolExecuteBefore: noop } });
  assert.equal('deny_on_failure' in m, false);
  assert.equal('hook_timeouts' in m, false);
});

test('denyOnFailure rejects a hook other than toolExecuteBefore', () => {
  assert.throws(
    () => createPlugin({ name: 'p', hooks: { toolExecuteAfter: noop }, denyOnFailure: ['toolExecuteAfter'] }),
    /denyOnFailure: toolExecuteAfter is valid only for toolExecuteBefore/,
  );
});

test('denyOnFailure rejects a hook with no function', () => {
  assert.throws(
    () => createPlugin({ name: 'p', denyOnFailure: ['toolExecuteBefore'] }),
    /denyOnFailure: toolExecuteBefore has no hook/,
  );
});

test('hookTimeouts maps hook keys to wire names', async () => {
  const m = await manifestOf({
    name: 'p',
    hooks: { toolExecuteBefore: noop, chatParams: noop },
    hookTimeouts: { toolExecuteBefore: 1500, chatParams: 600000 },
  });
  assert.deepEqual(m.hook_timeouts, { 'tool.execute.before': 1500, 'chat.params': 600000 });
});

test('hookTimeouts rejects a key with no hook', () => {
  assert.throws(
    () => createPlugin({ name: 'p', hookTimeouts: { chatParams: 10 } }),
    /hookTimeouts: chatParams has no hook/,
  );
});

test('denyOnFailure rejects a value that is not an array', () => {
  assert.throws(
    () => createPlugin({ name: 'p', hooks: { toolExecuteBefore: noop }, denyOnFailure: 'toolExecuteBefore' }),
    /denyOnFailure must be an array of hook keys/,
  );
});

test('hookTimeouts rejects onEvent', () => {
  assert.throws(
    () => createPlugin({ name: 'p', hooks: { onEvent: noop }, hookTimeouts: { onEvent: 10 } }),
    /hookTimeouts is not valid for onEvent/,
  );
});

test('hookTimeouts rejects a value outside 1 to 600000 or a non-integer', () => {
  for (const v of [0, -1, 600001, 1.5, '10', NaN]) {
    assert.throws(
      () => createPlugin({ name: 'p', hooks: { chatParams: noop }, hookTimeouts: { chatParams: v } }),
      /hookTimeouts: chatParams must be an integer from 1 to 600000/,
    );
  }
});
