import assert from 'node:assert/strict';
import test from 'node:test';
import semver from 'semver';
import { mkdtemp, writeFile, chmod, readFile, rm } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import os from 'node:os';
import * as contract from './witness-contract.mjs';
import {
  createVideoProgressGate, initialConsumersVerified, replacementVerified, requireDuration,
  requireElectronAlignment, requireNoRuntimeFailure, verifiedConsumerId, launchProbeElectron,
} from './witness-contract.mjs';

test('the real Playwright launch boundary never adds no-sandbox on Linux',
  { skip: process.platform !== 'linux' }, async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), 'voice-sandbox-argv-'));
  try {
    const capture = path.join(root, 'argv.json');
    const executable = path.join(root, 'record-argv');
    await writeFile(executable, `#!${process.execPath}\nrequire('node:fs').writeFileSync(${JSON.stringify(capture)}, JSON.stringify(process.argv.slice(2)));process.exit(1);\n`);
    await chmod(executable, 0o755);
    const child = path.join(root, 'launch.mjs');
    const contractUrl = new URL('./witness-contract.mjs', import.meta.url).href;
    await writeFile(child, `import { createRequire } from 'node:module';\nimport { launchProbeElectron } from ${JSON.stringify(contractUrl)};\nconst require = createRequire(${JSON.stringify(fileURLToPath(import.meta.url))});\nconst { _electron } = require('@playwright/test');\nvoid launchProbeElectron(_electron, ${JSON.stringify({ executablePath: executable, args: ['fixture.cjs'], timeout: 5000 })}).catch(() => {});\n`);
    // The recorder deliberately exits instead of starting an engine. Isolate the
    // dependency's expected launch rejection; assert the actual native argv.
    const result = spawnSync(process.execPath, [child], {
      timeout: 7000, maxBuffer: 1024 * 1024, shell: false, encoding: 'utf8',
    });
    assert.equal(result.error, undefined);
    const args = JSON.parse(await readFile(capture, 'utf8'));
    assert(args.includes('fixture.cjs') && args.includes('--remote-debugging-port=0'));
    assert.equal(args.includes('--no-sandbox'), false, 'Playwright disabled the renderer sandbox');
    assert.equal(args.includes('--disable-setuid-sandbox'), false);
  } finally { await rm(root, { recursive: true }); }
});

const sample = (framesDecoded, presentedFrames, extra = {}) => ({
  videoConsumerId: 'original', framesDecoded, presentedFrames,
  decodedProgressAgeMs: 100, presentedProgressAgeMs: 100, ...extra,
});

test('partial diagnostics retain only typed transport and media counters', () => {
  const input = { transportStates: { send: 'connected', audioReceive: 'checking', videoReceive: 'failed' },
    iceGatheringStates: { send: 'complete', audioReceive: 'gathering', videoReceive: 'new' },
    audioContextState: 'running', audioSignalPresent: false, decodedVideoPresent: true,
    audioPacketsReceived: 12, videoPacketsReceived: 18, videoBytesReceived: 4096,
    framesDecoded: 9, presentedFrames: 8, outboundPacketsSent: 40, outboundVideoFramesEncoded: 10,
    e2eeEntered: [11, 22, 33], sourceTracksLive: true,
    sdp: 'never retain this', testKey: [1, 2, 3], consumerId: 'private identity' };
  const expected = { transportStates: { send: 'connected', audioReceive: 'unknown', videoReceive: 'failed' },
    iceGatheringStates: { send: 'complete', audioReceive: 'gathering', videoReceive: 'new' },
    audioContextState: 'running', audioSignalPresent: false, decodedVideoPresent: true,
    audioPacketsReceived: 12, videoPacketsReceived: 18, videoBytesReceived: 4096,
    framesDecoded: 9, presentedFrames: 8, outboundPacketsSent: 40, outboundVideoFramesEncoded: 10,
    e2eeEntered: [11, 22], sourceTracksLive: true };
  assert.deepEqual(contract.summarizeRuntimeWitnesses?.([input, input, input]), [expected, expected]);
  const bad = { ...input, audioContextState: 'contains private text', framesDecoded: Infinity,
    audioPacketsReceived: -1, e2eeEntered: ['secret'], audioSignalPresent: 'yes' };
  const summary = contract.summarizeRuntimeWitnesses?.([bad])?.[0];
  assert.equal(summary?.framesDecoded, null);
  assert.equal(summary?.audioPacketsReceived, null);
  assert.deepEqual(summary?.e2eeEntered, [null]);
  assert.equal(summary?.audioSignalPresent, null);
  assert.equal(summary?.audioContextState, 'unknown');
});

test('the fixture fails closed before setup if its production service contract changes', () => {
  const methods = ['createE2EEWorker', 'produceEncrypted', 'creationAttachConsumeOption',
    'applyDecryptTransform', 'recordSelectedCandidatePair', 'liveUpdateVideoPriority',
    'startAvSyncSampling', 'terminateE2EEWorker'];
  const service = { ...Object.fromEntries(methods.map((name) => [name, () => {}])),
    consumers: new Map(), avSyncSamplingTimer: null, avSyncObservationCount: 0, avSyncDriftEmitted: false };
  assert.equal(typeof contract.requireVoiceProbeContract, 'function', 'Missing fail-closed contract preflight');
  assert.doesNotThrow(() => contract.requireVoiceProbeContract(service));
  for (const name of methods) {
    assert.throws(() => contract.requireVoiceProbeContract({ ...service, [name]: undefined }), /fixture contract missing/);
  }
  for (const bad of [{ consumers: {} }, { avSyncObservationCount: NaN },
    { avSyncSamplingTimer: 1 }, { avSyncDriftEmitted: undefined }]) {
    assert.throws(() => contract.requireVoiceProbeContract({ ...service, ...bad }), /fixture contract missing/);
  }
});

test('advancing real-counter snapshots pass; frozen startup image and one-sided progress fail', () => {
  const progress = createVideoProgressGate();
  assert.equal(progress(sample(100, 100)), false);
  assert.equal(progress(sample(150, 150)), true);
  assert.equal(progress(sample(150, 150)), false); // colored retained frame cannot prove delivery
  assert.equal(progress(sample(200, 150)), false); // decode without presentation
  assert.equal(progress(sample(200, 200)), false); // presentation without decode
  assert.equal(progress(sample(250, 250)), true);
  assert.equal(progress(sample(300, 300, { presentedProgressAgeMs: 10_001 })), false);
  assert.equal(progress(sample(350, 350, { decodedProgressAgeMs: 10_001 })), false);
});

test('a replacement consumer needs its own baseline and subsequent advancing poll', () => {
  const progress = createVideoProgressGate();
  progress(sample(100, 100));
  assert.equal(progress(sample(150, 150)), true);
  assert.equal(progress(sample(10, 10, { videoConsumerId: 'replacement' })), false);
  assert.equal(progress(sample(20, 20, { videoConsumerId: 'replacement' })), true);
  assert.equal(progress(sample(20, 20, { videoConsumerId: 'replacement' })), false);
});

test('original markers and entered frames cannot verify a replacement consumer', () => {
  const verified = new Set(['original-video', 'original-audio']);
  const replacement = { videoConsumerId: 'replacement', e2eeVideoEntered: 15, audioSignalPresent: true,
    consumerEvidence: [{ id: 'original-audio', kind: 'audio', entered: 20 }] };
  assert.equal(replacementVerified(replacement, 'original-video', verified, 'original-audio'), false);
  assert.equal(verifiedConsumerId({ consumerId: 'replacement', entered: 0, packetsReceived: 10 }), null);
  assert.equal(verifiedConsumerId({ consumerId: 'replacement', entered: 10, packetsReceived: 0 }), null);
  verified.add(verifiedConsumerId({ consumerId: 'replacement', entered: 10, packetsReceived: 10 }));
  assert.equal(replacementVerified(replacement, 'original-video', verified, 'original-audio'), true);
  assert.equal(replacementVerified({ ...replacement, videoConsumerId: 'original-video' }, 'original-video', verified, 'original-audio'), false);
  assert.equal(replacementVerified({ ...replacement, audioSignalPresent: false }, 'original-video', verified, 'original-audio'), false);
  assert.equal(replacementVerified({ ...replacement, consumerEvidence: [{ id: 'original-audio', kind: 'audio', entered: 0 }] }, 'original-video', verified, 'original-audio'), false);
  assert.equal(replacementVerified(replacement, 'original-video', verified, 'different-audio'), false);
});

test('an original audio consumer requires its own production verdict before measurement', () => {
  const witness = { consumerEvidence: [
    { id: 'audio', kind: 'audio', entered: 100 }, { id: 'video', kind: 'video', entered: 100 },
  ] };
  const verified = new Set(['video', 'unrelated-1', 'unrelated-2', 'unrelated-3']);
  assert.equal(initialConsumersVerified(witness, verified), false);
  verified.add('audio');
  assert.equal(initialConsumersVerified(witness, verified), true);
  assert.equal(initialConsumersVerified({ consumerEvidence: witness.consumerEvidence.map((entry) =>
    entry.kind === 'audio' ? { ...entry, entered: 0 } : entry) }, verified), false);
});

test('a recreation-phase error blocks the final pass gate', () => {
  const clean = { transformFailure: false, priorityUpdateFailure: false, pageErrors: 0 };
  assert.doesNotThrow(() => requireNoRuntimeFailure(clean));
  for (const bad of [{ transformFailure: true }, { priorityUpdateFailure: true }, { pageErrors: 1 }]) {
    assert.throws(() => requireNoRuntimeFailure({ ...clean, ...bad }), /Runtime media error/);
  }
});

test('candidate Electron binding refuses stale installed package, runtime and lock declarations', () => {
  const versions = { manifest: '^44.9.0', lockManifest: '^44.9.0', locked: '44.9.1', installed: '44.9.1', runtime: '44.9.1' };
  assert.doesNotThrow(() => requireElectronAlignment(versions, semver.satisfies));
  for (const bad of [{ installed: '44.9.0' }, { runtime: '44.9.0' }, { lockManifest: '^45.0.0' }, { locked: '45.0.0', installed: '45.0.0', runtime: '45.0.0' }]) {
    assert.throws(() => requireElectronAlignment({ ...versions, ...bad }, semver.satisfies), /Electron manifest\/lock\/installed\/runtime mismatch/);
  }
});

test('a ninety-second override cannot reduce the requested ninety-five-second gate', () => {
  assert.equal(requireDuration(95_000), 95_000);
  assert.equal(requireDuration(180_000), 180_000);
  for (const bad of [90_000, 94_999, 95_000.5, NaN, 180_001]) {
    assert.throws(() => requireDuration(bad), /95000\.\.180000/);
  }
});
