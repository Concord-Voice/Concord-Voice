'use strict';

const assert = require('node:assert/strict');

function assertPreloadEnvironment(value) {
  assert.deepEqual(
    value,
    { sandboxed: true, contextIsolated: true, type: 'renderer' },
    'the actual sandboxed preload must report an isolated renderer'
  );
}

async function runRendererProbe(contents, label, source, userGesture = false) {
  // executeJavaScript otherwise replaces a renderer exception with a generic
  // rejection. Only bounded fixture errors cross; console arguments stay private.
  const result = await contents.executeJavaScript(
    `(async () => {
    try { return { ok: true, value: await (${source}) }; }
    catch (error) { return { ok: false, error: String(error?.message ?? error).slice(0, 512) }; }
  })()`,
    userGesture
  );
  if (!result || typeof result !== 'object' || typeof result.ok !== 'boolean')
    throw new Error(`${label}: renderer returned no probe result`);
  if (!result.ok) throw new Error(`${label}: ${result.error}`);
  return result.value;
}

module.exports = { assertPreloadEnvironment, runRendererProbe };
