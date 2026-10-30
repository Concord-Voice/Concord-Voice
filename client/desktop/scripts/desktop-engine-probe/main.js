'use strict';

// Real Electron boundary checks. No server/account or signing key is needed.
// The fixture handlers exercise the two privileged renderer origins; signed
// cache admission and physical microphone/camera quality are separate gates.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { app, BrowserWindow, protocol, session, safeStorage } = require('electron');
const { build } = require('esbuild');
const { assertPreloadEnvironment, runRendererProbe } = require('./renderer-probe.js');
const { probeNativeProcessorError } = require('./processor-error-probe.js');

const root = path.resolve(__dirname, '../..');
const bundleRoot = process.argv[2] ? path.resolve(process.argv[2]) : root;
const profile = process.argv[3];
assert(profile, 'the bounded runner must supply an isolated profile');
assert.equal(
  process.versions.electron,
  process.argv[4],
  'Running Electron differs from manifest, lock and installed package'
);
app.setPath('userData', profile);
// The bounded fixture opens its two origin windows in sequence. Own its
// lifetime until the explicit success/failure exits below.
app.on('window-all-closed', () => undefined);
app.commandLine.appendSwitch('autoplay-policy', 'no-user-gesture-required');
// Exercise the unavailable-keychain arm without reading any host keyring.
if (process.platform === 'linux') app.commandLine.appendSwitch('password-store', 'basic');
protocol.registerSchemesAsPrivileged(
  ['app', 'spa-cache'].map((scheme) => ({
    scheme,
    privileges: { standard: true, secure: true, supportFetchAPI: true, corsEnabled: true },
  }))
);
const checks = [];
const windows = [];

async function main() {
  const assetRoot = path.join(bundleRoot, 'dist/renderer/assets');
  const worklets = fs
    .readdirSync(assetRoot)
    .filter((name) => /^micProcessor\.worklet-.*\.js$/.test(name));
  assert.equal(worklets.length, 1, 'exactly one emitted microphone worklet is required');
  const workletPath = `/assets/${worklets[0]}`;
  const faultWorkletPath = '/__engine_probe/process-error.worklet.js';
  const buildHost = (processorPath, globalName) =>
    build({
      entryPoints: [path.join(root, 'src/renderer/services/voice/micProcessor.ts')],
      bundle: true,
      write: false,
      format: 'iife',
      globalName,
      platform: 'browser',
      plugins: [
        {
          name: 'emitted-worklet-url',
          setup(builder) {
            builder.onResolve({ filter: /micProcessor\.worklet\.js\?url&no-inline$/ }, () => ({
              path: processorPath,
              namespace: 'probe-worklet',
            }));
            builder.onLoad({ filter: /.*/, namespace: 'probe-worklet' }, () => ({
              contents: `export default ${JSON.stringify(processorPath)}`,
              loader: 'js',
            }));
          },
        },
      ],
    });
  // Both bundles compile the unchanged production host. Only the explicit fault
  // arm substitutes its URL; healthy signal/calibration use the emitted asset.
  const host = await buildHost(workletPath, 'MicProbe');
  const faultHost = await buildHost(faultWorkletPath, 'MicFaultProbe');
  await app.whenReady();
  // Session frame preloads run before the unchanged built application preload.
  session.defaultSession.registerPreloadScript({
    type: 'frame',
    filePath: path.join(__dirname, 'environment-preload.js'),
  });
  const { encryptForDisk } = require(path.join(bundleRoot, 'dist/main/secureStorage.js'));
  const storageProbe = await encryptForDisk('public-engine-probe-fixture');
  if (process.platform === 'linux') {
    assert.equal(storageProbe, null, 'the basic-text backend must never persist credentials');
    checks.push({ safeStorage: 'PASS: unavailable keyring refuses disk persistence' });
  } else {
    assert(storageProbe, 'the ephemeral runner must provide OS-protected encryption');
    const decrypted = await safeStorage.decryptStringAsync(storageProbe);
    assert.equal(decrypted.result, 'public-engine-probe-fixture');
    checks.push({ safeStorage: 'PASS: OS-protected async roundtrip' });
  }
  // Test fixture pages and emitted assets are the entire network surface.
  session.defaultSession.webRequest.onBeforeRequest((details, callback) => {
    const url = new URL(details.url);
    callback({
      cancel:
        !(['app:', 'spa-cache:'].includes(url.protocol) && url.host === 'concord') &&
        url.protocol !== 'data:',
    });
  });
  const { registerAttestationIpc } = require(path.join(bundleRoot, 'dist/main/ipc/attestation.js'));
  registerAttestationIpc(() => null);
  for (const scheme of ['app', 'spa-cache']) {
    protocol.handle(scheme, (request) => {
      const url = new URL(request.url);
      if (url.host !== 'concord') return new Response(null, { status: 404 });
      if (url.pathname === '/')
        return new Response(
          '<!doctype html><meta charset="utf-8"><script src="/host.js"></script><script src="/fault-host.js"></script><body>Engine fixture</body>',
          { headers: { 'content-type': 'text/html; charset=utf-8' } }
        );
      if (url.pathname === '/host.js')
        return new Response(host.outputFiles[0].text, {
          headers: { 'content-type': 'text/javascript; charset=utf-8' },
        });
      if (url.pathname === '/fault-host.js')
        return new Response(faultHost.outputFiles[0].text, {
          headers: { 'content-type': 'text/javascript; charset=utf-8' },
        });
      if (url.pathname === faultWorkletPath)
        return new Response(fs.readFileSync(path.join(__dirname, 'process-error.worklet.js')), {
          headers: { 'content-type': 'text/javascript; charset=utf-8' },
        });
      if (url.pathname === workletPath)
        return new Response(fs.readFileSync(path.join(assetRoot, worklets[0])), {
          headers: { 'content-type': 'text/javascript; charset=utf-8' },
        });
      return new Response(null, { status: 404 });
    });
    const win = new BrowserWindow({
      show: true,
      webPreferences: {
        preload: path.join(bundleRoot, 'dist/preload/preload.js'),
        contextIsolation: true,
        sandbox: true,
        nodeIntegration: false,
      },
    });
    windows.push(win);
    win.webContents.on('preload-error', (_event, preload, error) => {
      throw new Error(`preload failed: ${path.basename(preload)}: ${error.message}`);
    });
    await win.loadURL(`${scheme}://concord/`);
    const isolation = await runRendererProbe(
      win.webContents,
      `${scheme}:preload-environment`,
      'window.engineProbeEnvironment'
    );
    assertPreloadEnvironment(isolation);
    if (process.platform !== 'linux') {
      const rendererPid = win.webContents.getOSProcessId();
      let metric;
      for (let attempt = 0; attempt < 20 && !metric; attempt++) {
        metric = app.getAppMetrics().find((entry) => entry.pid === rendererPid);
        if (!metric) await new Promise((resolve) => setTimeout(resolve, 10));
      }
      assert.equal(metric?.sandboxed, true, 'the real renderer must be sandboxed at OS level');
    }
    const bridge = await runRendererProbe(
      win.webContents,
      `${scheme}:trusted-attestation`,
      `(async () => ({ token: await window.electron.attestation.getToken(), node: typeof require, process: typeof process, secure: isSecureContext }))()`
    );
    assert.deepEqual(bridge, {
      token: null,
      node: 'undefined',
      process: 'undefined',
      secure: true,
    });
    checks.push({ origin: scheme, sandboxedPreloadAndTrustedIpc: 'PASS', isolation });
    const audio = await runRendererProbe(
      win.webContents,
      `${scheme}:microphone-worklet`,
      `(async () => {
      const response = await fetch(${JSON.stringify(workletPath)});
      if (!response.ok || !response.headers.get('content-type').startsWith('text/javascript')) throw new Error('worklet URL or MIME is wrong');
      const context = new OfflineAudioContext(2, 4800, 48000);
      let errors = 0;
      const processor = await MicProbe.createMicProcessor(context, { protectAgcPeaks: true, gate: { kind: 'off' } }, () => {}, () => errors++);
      const buffer = context.createBuffer(2, 4800, 48000);
      buffer.getChannelData(0)[0] = 1; buffer.getChannelData(1)[0] = 0.5;
      const source = context.createBufferSource(); source.buffer = buffer;
      source.connect(processor.node).connect(context.destination); source.start();
      const rendered = await context.startRendering();
      const left = rendered.getChannelData(0); const right = rendered.getChannelData(1);
      const peak = Math.max(...left); const frame = left.indexOf(peak);
      processor.close();
      // A deliberate process() throw tests native event delivery and production
      // host retirement. This does not claim a MessagePort exception emits it.
      const badContext = new OfflineAudioContext(2, 4800, 48000);
      const processorFailure = await (${probeNativeProcessorError.toString()})(badContext, MicFaultProbe.createMicProcessor);
      const calibrationContext = new OfflineAudioContext(1, 31 * 48000, 48000);
      const events = [];
      const calibration = await MicProbe.createMicProcessor(calibrationContext,
        { protectAgcPeaks: true, gate: { kind: 'fixed', thresholdDbfs: -40 } },
        () => {}, () => errors++, undefined, event => events.push(event));
      const calibrationBuffer = calibrationContext.createBuffer(1, 31 * 48000, 48000);
      calibrationBuffer.getChannelData(0).fill(0.001);
      calibrationBuffer.getChannelData(0).fill(0.01, Math.round(10.08 * 48000));
      const calibrationSource = calibrationContext.createBufferSource();
      calibrationSource.buffer = calibrationBuffer;
      const silent = calibrationContext.createGain(); silent.gain.value = 0;
      calibrationSource.connect(calibration.node).connect(silent).connect(calibrationContext.destination);
      calibration.setCalibrationPhase(1, 'quiet');
      const partial = calibrationContext.suspend(0.03);
      const speech = calibrationContext.suspend(10.08);
      calibrationSource.start(); const rendering = calibrationContext.startRendering();
      await partial;
      // Supersede a partially captured window through the production host API.
      calibration.setCalibrationPhase(2, 'quiet');
      await calibrationContext.resume();
      await speech;
      calibration.setCalibrationPhase(3, 'speech');
      await calibrationContext.resume();
      const calibrated = await rendering;
      const deadline = performance.now() + 2000;
      while (!events.some(event => event.type === 'calibrationComplete' && event.epoch === 3) && performance.now() < deadline)
        await new Promise(resolve => setTimeout(resolve, 10));
      calibration.close();
      const quiet = events.filter(event => event.type === 'calibrationWindow' && event.epoch === 2);
      const spoken = events.filter(event => event.type === 'calibrationWindow' && event.epoch === 3);
      const complete = events.filter(event => event.type === 'calibrationComplete');
      return { peak, stereoRatio: right[frame] / peak, delayFrames: frame,
        processorFailureCallbacks: processorFailure.callbacks, unexpectedProcessorErrors: errors,
        processorFailure: { injection: 'synthetic process() throw; unchanged production host', ...processorFailure },
        calibration: { quietWindows: quiet.length, speechWindows: spoken.length,
          quietFrames: quiet.reduce((sum, event) => sum + event.frames, 0),
          speechFrames: spoken.reduce((sum, event) => sum + event.frames, 0),
          quietComplete: complete.some(event => event.epoch === 2 && event.frames === 480000),
          speechComplete: complete.some(event => event.epoch === 3 && event.frames === 960000),
          silentOutput: calibrated.getChannelData(0).every(value => value === 0),
          allWindowsComplete: [...quiet, ...spoken].every(event => event.valid && event.frames === 960),
          speechPeak: spoken.length ? spoken[spoken.length - 1].peak : null } };
    })()`,
      true
    );
    assert(
      Math.abs(audio.peak - 0.5011872053146362) < 0.00001,
      'the emitted processor must produce the limited impulse'
    );
    assert.equal(audio.stereoRatio, 0.5);
    assert.equal(audio.delayFrames, 192);
    assert.equal(audio.processorFailureCallbacks, 1, 'the actual worklet failure must notify once');
    assert.equal(audio.unexpectedProcessorErrors, 0, 'healthy production worklets must not fail');
    const eventMode = audio.processorFailure.eventMode;
    const standard = eventMode === 'processorerror' || eventMode === 'both';
    const compatibility = eventMode === 'error' || eventMode === 'both';
    assert(standard || compatibility, 'the native failure event kind must be explicit');
    assert.deepEqual(audio.processorFailure, {
      injection: 'synthetic process() throw; unchanged production host',
      eventMode,
      standardProcessorError: standard ? 'PASS' : 'BROKEN',
      compatibilityError: compatibility ? 'PASS' : 'NOT RUN',
      nativeEvents: Number(standard),
      trustedEvents: Number(standard),
      errorEvents: Number(compatibility),
      trustedErrorEvents: Number(compatibility),
      callbacks: 1,
      portCloses: 1,
      disconnects: 1,
    });
    assert.deepEqual(audio.calibration, {
      quietWindows: 500,
      speechWindows: 1000,
      quietFrames: 480000,
      speechFrames: 960000,
      quietComplete: true,
      speechComplete: true,
      silentOutput: true,
      allWindowsComplete: true,
      speechPeak: Math.fround(0.01),
    });
    checks.push({ origin: scheme, microphoneWorklet: audio });
    await win.loadURL('data:text/html,untrusted-frame');
    const refused = await runRendererProbe(
      win.webContents,
      `${scheme}:untrusted-attestation`,
      `window.electron.attestation.getToken().then(() => false, error => /untrusted sender frame/.test(error.message))`
    );
    assert.equal(refused, true, 'production IPC must refuse an untrusted renderer');
    checks.push({ origin: scheme, untrustedSenderRefusal: 'PASS' });
    win.destroy();
  }
  process.stdout.write(
    JSON.stringify({
      electron: process.versions.electron,
      chromium: process.versions.chrome,
      node: process.versions.node,
      bundleRoot: path.basename(bundleRoot),
      checks,
    }) + '\n'
  );
  process.stdout.write('PROBE PASS desktop-engine\n');
}

main()
  .then(() => app.exit(0))
  .catch((error) => {
    console.error(`PROBE FAIL desktop-engine: ${error.stack}`);
    for (const win of windows) if (!win.isDestroyed()) win.destroy();
    app.exit(1);
  });
