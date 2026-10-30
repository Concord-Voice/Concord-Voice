// @vitest-environment node
import { afterEach, describe, expect, it } from 'vitest';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const owned: string[] = [];
afterEach(() => {
  for (const directory of owned.splice(0)) rmSync(directory, { recursive: true, force: true });
});

// Oracle: The bounded engine fixture remains alive when its last probe window
// closes so the next origin can run, then preserves its explicit terminal success
// and failure exits.
function runActualFixture(externalLifetimeOwner = false, failOrigin: string | null = null) {
  const directory = mkdtempSync(path.join(tmpdir(), 'engine-lifecycle-'));
  owned.push(directory);
  const assets = path.join(directory, 'dist/renderer/assets');
  mkdirSync(assets, { recursive: true });
  writeFileSync(path.join(assets, 'micProcessor.worklet-fixture.js'), 'fixture asset');
  const script = path.join(directory, 'lifecycle.cjs');
  const actualMain = fileURLToPath(new URL('./main.js', import.meta.url));
  writeFileSync(
    script,
    `const { EventEmitter } = require('node:events');
const Module = require('node:module');
const path = require('node:path');
const originalLoad = Module._load;
const active = new Set();
const evidence = { loadedOrigins: [], destroyedOrigins: [], windowClosedEvents: 0, defaultQuitCalls: 0, terminalExit: null, externalOwnerCalls: 0 };
const app = new EventEmitter();
app.commandLine = { appendSwitch() {} };
app.setPath = () => {};
app.whenReady = async () => {};
app.getAppMetrics = () => [{pid: 123, sandboxed: true}];
app.quitting = false;
app.quit = () => { evidence.defaultQuitCalls++; app.quitting = true; };
app.exit = code => {
  evidence.terminalExit = code;
  process.stdout.write('LIFECYCLE_RECEIPT ' + JSON.stringify(evidence) + '\\n');
  process.exit(code);
};
// Pinned Electron45 lib/browser/init.ts: default listener quits only when sole.
app.on('window-all-closed', () => {
  if (app.listenerCount('window-all-closed') === 1) app.quit();
});
if (${JSON.stringify(externalLifetimeOwner)}) app.on('window-all-closed', () => { evidence.externalOwnerCalls++; });
class BrowserWindow {
  constructor(options) {
    if (!options.webPreferences.sandbox || !options.webPreferences.contextIsolation || options.webPreferences.nodeIntegration) throw new Error('unsafe fixture options');
    active.add(this);
    this.dead = false;
    this.origin = null;
    let step = 0;
    this.webContents = new EventEmitter();
    this.webContents.getOSProcessId = () => 123;
    this.webContents.executeJavaScript = async () => {
      const stage = step++;
      if (stage === 1 && this.origin === ${JSON.stringify(failOrigin)}) return {ok: false, error: 'owned fixture renderer failure'};
      const values = [
        {sandboxed: true, contextIsolated: true, type: 'renderer'},
        {token: null, node: 'undefined', process: 'undefined', secure: true},
        {peak: 0.5011872053146362, stereoRatio: 0.5, delayFrames: 192,
         processorFailureCallbacks: 1, unexpectedProcessorErrors: 0,
         processorFailure: {injection: 'synthetic process() throw; unchanged production host', eventMode: 'error', standardProcessorError: 'BROKEN', compatibilityError: 'PASS', nativeEvents: 0, trustedEvents: 0, errorEvents: 1, trustedErrorEvents: 1, callbacks: 1, portCloses: 1, disconnects: 1},
         calibration: {quietWindows: 500, speechWindows: 1000, quietFrames: 480000, speechFrames: 960000, quietComplete: true, speechComplete: true, silentOutput: true, allWindowsComplete: true, speechPeak: Math.fround(0.01)}},
        true,
      ];
      if (stage >= values.length) throw new Error('unexpected extra renderer request');
      return {ok: true, value: values[stage]};
    };
  }
  async loadURL(url) {
    if (app.quitting) throw new Error('ERR_FAILED after default quit: ' + url);
    if (url.startsWith('data:')) return;
    this.origin = new URL(url).protocol.slice(0, -1);
    evidence.loadedOrigins.push(this.origin);
  }
  isDestroyed() { return this.dead; }
  destroy() {
    if (this.dead) return;
    this.dead = true;
    active.delete(this);
    evidence.destroyedOrigins.push(this.origin);
    if (active.size === 0) { evidence.windowClosedEvents++; app.emit('window-all-closed'); }
  }
}
Module._load = function(name, parent, isMain) {
  if (name === 'electron') return {
    app, BrowserWindow,
    protocol: { registerSchemesAsPrivileged() {}, handle() {} },
    session: {defaultSession: {registerPreloadScript() {}, webRequest: {onBeforeRequest() {}}}},
    safeStorage: {decryptStringAsync: async () => ({result: 'public-engine-probe-fixture'})},
  };
  if (name === 'esbuild') return {build: async () => ({outputFiles: [{text: 'fixture host bundle'}]})};
  if (name === path.join(${JSON.stringify(directory)}, 'dist/main/secureStorage.js')) return {encryptForDisk: async () => process.platform === 'linux' ? null : 'protected-fixture'};
  if (name === path.join(${JSON.stringify(directory)}, 'dist/main/ipc/attestation.js')) return {registerAttestationIpc() {}};
  return originalLoad.call(this, name, parent, isMain);
};
Object.defineProperty(process.versions, 'electron', {value: '45.0.0'});
Object.defineProperty(process.versions, 'chrome', {value: '156.0.8078.48'});
process.argv = [process.execPath, ${JSON.stringify(actualMain)}, ${JSON.stringify(directory)}, ${JSON.stringify(path.join(directory, 'profile'))}, '45.0.0'];
require(${JSON.stringify(actualMain)});
`
  );
  const child = spawnSync(process.execPath, [script], { encoding: 'utf8', timeout: 5000 });
  expect(child.error, 'the owned fixture child must finish rather than time out').toBeUndefined();
  const line = child.stdout.split('\n').find((value) => value.startsWith('LIFECYCLE_RECEIPT '));
  expect(line, 'actual main must reach an explicit app.exit terminal path').toBeDefined();
  if (!line) throw new Error('actual main did not emit its explicit terminal receipt');
  const receipt = JSON.parse(line.slice('LIFECYCLE_RECEIPT '.length)) as {
    loadedOrigins: string[];
    destroyedOrigins: Array<string | null>;
    windowClosedEvents: number;
    defaultQuitCalls: number;
    terminalExit: number;
    externalOwnerCalls: number;
  };
  return { receipt, status: child.status, stdout: child.stdout, stderr: child.stderr };
}

describe('bounded engine fixture window lifetime', () => {
  it('remains alive after its last first-origin window closes so the next origin runs', () => {
    const result = runActualFixture();
    expect(result.receipt.destroyedOrigins).toContain('app');
    expect(result.receipt.windowClosedEvents).toBeGreaterThanOrEqual(1);
    expect(
      {
        defaultQuitCalls: result.receipt.defaultQuitCalls,
        loadedOrigins: result.receipt.loadedOrigins,
        explicitTerminalExit: result.receipt.terminalExit,
        childExit: result.status,
      },
      'fixture must remain alive after its last probe window closes so the next origin can run and explicitly succeed'
    ).toEqual({
      defaultQuitCalls: 0,
      loadedOrigins: ['app', 'spa-cache'],
      explicitTerminalExit: 0,
      childExit: 0,
    });
    expect(result.stdout).toContain('PROBE PASS desktop-engine');
  });

  it('positive control: an external lifetime owner prevents default quit and both origins reach app.exit(0)', () => {
    const result = runActualFixture(true);
    expect(result.receipt.loadedOrigins).toEqual(['app', 'spa-cache']);
    expect(result.receipt.destroyedOrigins).toEqual(['app', 'spa-cache']);
    expect(result.receipt.windowClosedEvents).toBe(2);
    expect(result.receipt.externalOwnerCalls).toBe(2);
    expect(result.receipt.defaultQuitCalls).toBe(0);
    expect(result.receipt.terminalExit).toBe(0);
    expect(result.status).toBe(0);
    expect(result.stdout).toContain('PROBE PASS desktop-engine');
  });

  it.each(['app', 'spa-cache'])('preserves explicit app.exit(1) for a failure in %s', (origin) => {
    const result = runActualFixture(true, origin);
    expect(result.receipt.loadedOrigins).toContain(origin);
    expect(result.receipt.defaultQuitCalls).toBe(0);
    expect(
      result.receipt.terminalExit,
      'lifetime ownership must preserve explicit terminal failure'
    ).toBe(1);
    expect(result.status).toBe(1);
    expect(result.stdout).not.toContain('PROBE PASS desktop-engine');
    expect(result.stderr).toContain('PROBE FAIL desktop-engine');
    expect(result.stderr).toContain('owned fixture renderer failure');
  });
});
