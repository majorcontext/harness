// Unit tests for PluginContext.fetch's header merging. Confirms all three
// HeadersInit shapes (plain object, Headers instance, [key, value][] array)
// are preserved when merged with ctx.httpHeaders. Run with `node --test`.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { PluginContext } from '../harness-plugin.mjs';

/** Builds a PluginContext with the given http_headers and stubs global fetch
 * to capture the Headers it was called with. */
function makeCtx(httpHeaders) {
  const ctx = new PluginContext(null, { http_headers: httpHeaders });
  let captured;
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    captured = init.headers;
    return new Response('ok');
  };
  return {
    ctx,
    captured: () => captured,
    restore: () => {
      globalThis.fetch = originalFetch;
    },
  };
}

test('fetch merges plain object init.headers', async () => {
  const { ctx, captured, restore } = makeCtx({ 'X-Harness': 'stamped' });
  try {
    await ctx.fetch('http://example.test', { headers: { 'X-Plugin': 'plain' } });
    const headers = captured();
    assert.equal(headers.get('X-Harness'), 'stamped');
    assert.equal(headers.get('X-Plugin'), 'plain');
  } finally {
    restore();
  }
});

test('fetch merges Headers instance init.headers', async () => {
  const { ctx, captured, restore } = makeCtx({ 'X-Harness': 'stamped' });
  try {
    await ctx.fetch('http://example.test', { headers: new Headers({ 'X-Plugin': 'headers-instance' }) });
    const headers = captured();
    assert.equal(headers.get('X-Harness'), 'stamped');
    assert.equal(headers.get('X-Plugin'), 'headers-instance');
  } finally {
    restore();
  }
});

test('fetch merges array-of-tuples init.headers', async () => {
  const { ctx, captured, restore } = makeCtx({ 'X-Harness': 'stamped' });
  try {
    await ctx.fetch('http://example.test', { headers: [['X-Plugin', 'tuple']] });
    const headers = captured();
    assert.equal(headers.get('X-Harness'), 'stamped');
    assert.equal(headers.get('X-Plugin'), 'tuple');
  } finally {
    restore();
  }
});

test('fetch: init.headers wins on conflicts with httpHeaders', async () => {
  const { ctx, captured, restore } = makeCtx({ 'X-Harness': 'from-httpHeaders' });
  try {
    await ctx.fetch('http://example.test', { headers: new Headers({ 'X-Harness': 'from-init' }) });
    assert.equal(captured().get('X-Harness'), 'from-init');
  } finally {
    restore();
  }
});

test('fetch: no init.headers still stamps httpHeaders', async () => {
  const { ctx, captured, restore } = makeCtx({ 'X-Harness': 'stamped' });
  try {
    await ctx.fetch('http://example.test');
    assert.equal(captured().get('X-Harness'), 'stamped');
  } finally {
    restore();
  }
});
