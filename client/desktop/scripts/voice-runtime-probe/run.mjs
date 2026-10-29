import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { readFile, mkdir, writeFile, mkdtemp, rm } from 'node:fs/promises';
import { createRequire } from 'node:module';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import { setTimeout as delay } from 'node:timers/promises';
import { createServer } from 'vite';
import { _electron as electron } from '@playwright/test';
import { createLocalSfu } from './local-sfu.mjs';
import { createVideoProgressGate, initialConsumersVerified, replacementVerified, requireDuration,
  requireElectronAlignment, requireNoRuntimeFailure, verifiedConsumerId, launchProbeElectron,
  summarizeRuntimeWitnesses } from './witness-contract.mjs';

const directory = path.dirname(fileURLToPath(import.meta.url));
const desktop = path.resolve(directory, '../..');
const media = path.resolve(process.env.MEDIA_PROBE_SERVER_DIR ?? path.join(desktop, '../../services/media-plane'));
const require = createRequire(path.join(desktop, 'package.json'));
const electronDirectory = path.dirname(require.resolve('electron'));
const duration = requireDuration(Number(process.env.MEDIA_PROBE_DURATION_MS ?? 95_000));
const result = { status: 'FAIL', scope: 'production media hooks and sampler; fixture account/key/signaling setup',
  durationRequiredMs: duration, versions: {}, witnesses: [], detectorFaultControl: 'NOT RUN' };
let sfu, vite, app, profile;
let stage = 'setup';
const pages = [];
const markers = { selectedPair: 0, transformVerified: 0, transformFailure: false, priorityUpdateFailure: false,
  pageErrors: 0, workerInitialized: 0, transformEvents: 0, firstEncrypted: 0, firstDecrypted: 0 };
const verifiedByPeer = [new Set(), new Set()];
const pendingMarkers = new Set();

async function until(operation, predicate, label, timeout = 30_000) {
  const deadline = Date.now() + timeout;
  do {
    const value = await operation();
    if (predicate(value)) return value;
    await delay(500);
  } while (Date.now() < deadline);
  throw new Error(`Missing runtime witness: ${label}`);
}

try {
  const mediaLock = JSON.parse(await readFile(path.join(media, 'package-lock.json'), 'utf8'));
  sfu = await createLocalSfu(media);
  assert.equal(sfu.version, mediaLock.packages['node_modules/mediasoup'].version, 'Actual SFU must match selected lockfile');
  result.versions.mediasoupPackage = sfu.version;
  result.nativeWorker = sfu.nativeWorker;
  result.versions.mediasoupClient = require('mediasoup-client').version;
  const clientLock = JSON.parse(await readFile(path.join(desktop, 'package-lock.json'), 'utf8'));
  const clientManifest = JSON.parse(await readFile(path.join(desktop, 'package.json'), 'utf8'));
  const electronBinding = {
    manifest: clientManifest.devDependencies.electron,
    lockManifest: clientLock.packages[''].devDependencies.electron,
    locked: clientLock.packages['node_modules/electron'].version,
    installed: JSON.parse(await readFile(path.join(electronDirectory, 'package.json'), 'utf8')).version,
  };
  requireElectronAlignment(electronBinding, require('semver').satisfies);
  assert.equal(result.versions.mediasoupClient, clientLock.packages['node_modules/mediasoup-client'].version);
  vite = await createServer({
    configFile: path.join(desktop, 'vite.config.ts'), root: desktop,
    server: { host: '127.0.0.1', port: 0, strictPort: true, hmr: false },
    plugins: [{ name: 'local-voice-runtime-probe', configureServer(server) {
      server.middlewares.use(async (request, response, next) => {
        const url = new URL(request.url, 'http://127.0.0.1');
        if (url.pathname === '/__voice_runtime_probe') {
          response.setHeader('Content-Type', 'text/html');
          response.end(await server.transformIndexHtml(request.url,
            '<!doctype html><html><head><title>Voice runtime probe</title></head><body><script type="module" src="/scripts/voice-runtime-probe/page.mjs"></script></body></html>'));
          return;
        }
        if (url.pathname !== '/__voice_runtime_probe/rpc') { next(); return; }
        // The fixture binds loopback and admits only same-origin POSTs. It is
        // never a deployed or user-authenticated signaling endpoint.
        if (request.method !== 'POST' || request.headers.origin !== `http://${request.headers.host}`) {
          response.writeHead(403); response.end(); return;
        }
        try {
          let body = '';
          for await (const chunk of request) {
            body += chunk;
            if (Buffer.byteLength(body) > 32_768) throw new Error('Fixture body limit');
          }
          const data = await sfu.rpc(JSON.parse(body));
          response.setHeader('Content-Type', 'application/json'); response.end(JSON.stringify(data));
        } catch {
          response.writeHead(400); response.end('Invalid local fixture operation');
        }
      });
    } }],
  });
  await vite.listen();
  const address = vite.httpServer.address();
  assert(address && typeof address === 'object');
  const origin = `http://127.0.0.1:${address.port}`;
  // Resolve without importing electron/index.js: recent releases download at
  // import time, which would hide a missing normal installation prerequisite.
  const executablePath = process.env.MEDIA_PROBE_ELECTRON ?? path.join(electronDirectory, 'dist',
    (await readFile(path.join(electronDirectory, 'path.txt'), 'utf8')).trim());
  profile = await mkdtemp(path.join(os.tmpdir(), 'concord-voice-runtime-'));
  stage = 'engine-launch';
  app = await launchProbeElectron(electron, { executablePath, timeout: 30_000,
    args: [path.join(directory, 'main.cjs'), `--user-data-dir=${profile}`, '--allow-loopback-in-peer-connection',
      '--autoplay-policy=no-user-gesture-required'],
  });
  stage = 'renderer-setup';
  result.runtimeDiagnostics = { windows: {}, peers: [] };
  await until(async () => {
    result.runtimeDiagnostics.windows = {
      ...await app.evaluate(({ BrowserWindow }) => ({
        native: BrowserWindow.getAllWindows().length,
        rendererCrashes: globalThis.voiceProbeLifecycle?.rendererCrashes ?? 0,
        navigationFailures: globalThis.voiceProbeLifecycle?.navigationFailures ?? 0,
      })),
      playwright: app.windows().length,
    };
    return app.windows().length;
  }, (count) => count === 2, 'two isolated renderer windows');
  result.versions = { ...result.versions, ...await app.evaluate(({ BrowserWindow }) => ({
    electron: process.versions.electron, chromium: process.versions.chrome, node: process.versions.node,
    sandbox: BrowserWindow.getAllWindows().every((window) => {
      const preferences = window.webContents.getLastWebPreferences();
      return preferences.sandbox && preferences.contextIsolation && !preferences.nodeIntegration && preferences.webSecurity;
    }),
  })) };
  requireElectronAlignment({ ...electronBinding, runtime: result.versions.electron }, require('semver').satisfies);
  assert(result.versions.sandbox, 'Renderer isolation is required');
  await app.context().route('**/*', (route) => {
    const url = new URL(route.request().url());
    return url.origin === origin ? route.continue() : route.abort('blockedbyclient');
  });
  pages.push(...app.windows());
  const testKey = [...randomBytes(32)];
  for (const [index, page] of pages.entries()) {
    page.on('pageerror', () => { markers.pageErrors++; });
    page.on('console', (message) => {
      // Do not retain or print the original console arguments, SDP, keys or stats.
      const text = message.text();
      if (text.startsWith('E2EE Worker: initialized')) markers.workerInitialized++;
      if (text.startsWith('E2EE: rtctransform event fired')) markers.transformEvents++;
      if (text.startsWith('E2EE: first frame encrypted')) markers.firstEncrypted++;
      if (text.startsWith('E2EE: first frame decrypted')) markers.firstDecrypted++;
      if (text.startsWith('[ice] selected-pair ') || text === '[ice] selected-pair') markers.selectedPair++;
      if (text.startsWith('E2EE: receive transform verified')) {
        const pending = (async () => {
          const id = verifiedConsumerId(await message.args()[1]?.jsonValue());
          if (!id) { markers.transformFailure = true; return; }
          verifiedByPeer[index].add(id); markers.transformVerified++;
        })().catch(() => { markers.transformFailure = true; });
        pendingMarkers.add(pending);
        void pending.finally(() => pendingMarkers.delete(pending));
      }
      if (/BYPASSED|still bypassed|E2EE Worker error|encrypt transform failed/.test(text)) markers.transformFailure = true;
      if (text.startsWith('[video-settings] Video priority update failed')) markers.priorityUpdateFailure = true;
    });
    await page.goto(`${origin}/__voice_runtime_probe?peer=${index === 0 ? 'a' : 'b'}`);
    await page.waitForFunction(() => Boolean(globalThis.mediaProbe));
    const setup = await page.evaluate((key) => globalThis.mediaProbe.setup(key), testKey);
    assert.equal(setup.nodeVisibleInRenderer, false);
  }
  stage = 'publish-consume';
  const descriptors = [];
  for (const page of pages) descriptors.push(await page.evaluate(() => globalThis.mediaProbe.publish()));
  for (let index = 0; index < pages.length; index++) {
    for (const descriptor of descriptors[1 - index]) {
      await pages[index].evaluate((entry) => globalThis.mediaProbe.consume(entry), descriptor);
    }
  }
  const readWitnesses = async () => {
    const witnesses = await Promise.all(pages.map((page) => page.evaluate(() => globalThis.mediaProbe.witness())));
    await Promise.all([...pendingMarkers]);
    result.runtimeDiagnostics.peers = summarizeRuntimeWitnesses(witnesses);
    result.runtimeDiagnostics.verifiedConsumers = verifiedByPeer.map((ids) => ids.size);
    return witnesses;
  };
  await until(readWitnesses, (witnesses) => witnesses.every((entry) => entry.audioSignalPresent && entry.decodedVideoPresent && entry.e2eeEntered.every((count) => count > 0)), 'decrypted audio and video');
  for (const page of pages) await page.evaluate(() => globalThis.mediaProbe.updatePriority());
  await until(readWitnesses, (witnesses) => witnesses.every((entry) => entry.layeredPriorityApplied), 'live layered encoding priority update');
  const initialWitnesses = await until(readWitnesses, (witnesses) => witnesses.every((entry, index) =>
    initialConsumersVerified(entry, verifiedByPeer[index])), 'each original audio/video consumer production verification');
  const originalAudioIds = initialWitnesses.map((entry) => entry.consumerEvidence.find((consumer) => consumer.kind === 'audio').id);
  stage = 'continuous-call';
  const progress = pages.map(() => createVideoProgressGate());
  (await readWitnesses()).forEach((entry, index) => progress[index](entry));
  for (const page of pages) await page.evaluate(() => globalThis.mediaProbe.arm());
  // No getStats replacement, mocked RTC or manually advanced observation counter.
  const deadline = Date.now() + duration;
  while (Date.now() < deadline) {
    await delay(5000);
    const live = await readWitnesses();
    assert(live.every((entry, index) => entry.audioSignalPresent && entry.decodedVideoPresent &&
      entry.sourceTracksLive && progress[index](entry)), 'Continuous decoded/presented delivery stopped');
    requireNoRuntimeFailure(markers);
  }
  result.witnesses = await readWitnesses();
  for (const entry of result.witnesses) {
    assert(entry.measuredDurationMs >= duration && entry.productionTimerArmed && entry.productionObservationCount > 0);
    assert(entry.independentUsableCount > 0 && !entry.independentDrift && !entry.productionDrift);
    assert(entry.e2eeEntered.length === 2 && entry.e2eeEntered.every((count) => count > 0));
    assert(entry.layeredPriorityApplied && entry.audioExtensionsNegotiated && entry.videoExtensionsNegotiated);
    assert(entry.injectedDetectorTripped, 'Separate detector fault control did not trip');
  }
  result.detectorFaultControl = 'PASS: separate copied-report 2 ms/s injection; production RTC/sampler untouched';
  stage = 'camera-reproduce';
  const previousVideoIds = result.witnesses.map((entry) => entry.videoConsumerId);
  for (const page of pages) await page.evaluate(() => globalThis.mediaProbe.closeRemoteVideo());
  const replacement = [];
  for (const page of pages) replacement.push(await page.evaluate(() => globalThis.mediaProbe.reproduceVideo()));
  for (let index = 0; index < pages.length; index++) {
    await pages[index].evaluate((entry) => globalThis.mediaProbe.consume(entry), replacement[1 - index]);
  }
  const replacementProgress = pages.map(() => createVideoProgressGate());
  result.replacementWitnesses = await until(readWitnesses, (witnesses) => witnesses.every((entry, index) => {
    const advancing = replacementProgress[index](entry);
    return entry.decodedVideoPresent && entry.sourceTracksLive && advancing &&
      replacementVerified(entry, previousVideoIds[index], verifiedByPeer[index], originalAudioIds[index]);
  }), 'each replacement consumer progressing and independently verified by production E2EE probe');
  await Promise.all([...pendingMarkers]);
  requireNoRuntimeFailure(markers);
  assert(markers.selectedPair >= 2, 'Production ICE markers missing');
  result.cameraReproduce = 'PASS'; result.markers = markers; result.status = 'PASS';
} catch (error) {
  result.markers = { ...markers };
  result.stage = stage;
  result.status = stage === 'engine-launch' ? 'NOT RUN' : 'FAIL';
  // Keep failures reviewable without browser launch logs or raw console/stat dumps.
  result.reason = stage === 'engine-launch' ? 'Sandboxed Electron failed to start; inspect runner sandbox support' : error.message;
  process.exitCode = 1;
} finally {
  for (const page of pages) {
    try { await page.evaluate(() => globalThis.mediaProbe?.cleanup()); } catch { /* renderer may have exited */ }
  }
  try { await app?.close(); } catch { result.status = 'FAIL'; result.cleanup = 'Electron close failed'; process.exitCode = 1; }
  if (profile) {
    try { await rm(profile, { recursive: true }); }
    catch { result.status = 'FAIL'; result.cleanup = 'Owned temporary profile cleanup failed'; process.exitCode = 1; }
  }
  try { await vite?.close(); } catch { result.status = 'FAIL'; result.cleanup = 'Vite close failed'; process.exitCode = 1; }
  try { await sfu?.close(); result.nativeSubprocessClosed = Boolean(sfu); }
  catch { result.status = 'FAIL'; result.cleanup = 'Native worker exit unconfirmed'; process.exitCode = 1; }
  if (process.env.MEDIA_PROBE_OUTPUT) {
    const output = path.resolve(process.env.MEDIA_PROBE_OUTPUT);
    await mkdir(path.dirname(output), { recursive: true });
    await writeFile(output, `${JSON.stringify(result, null, 2)}\n`);
  }
  process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
}
