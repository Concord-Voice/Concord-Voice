export const MIN_DURATION_MS = 95_000;
export const VIDEO_STALL_MS = 10_000;

export function launchProbeElectron(electron, options) {
  // Playwright otherwise prepends --no-sandbox on Linux, which this fixture refuses.
  return electron.launch({ ...options, chromiumSandbox: true });
}

/** Fail before key setup if the test-only integration seam has changed. */
export function requireVoiceProbeContract(service) {
  for (const name of ['createE2EEWorker', 'produceEncrypted', 'creationAttachConsumeOption',
    'applyDecryptTransform', 'recordSelectedCandidatePair', 'liveUpdateVideoPriority',
    'startAvSyncSampling', 'terminateE2EEWorker']) {
    if (typeof service?.[name] !== 'function') throw new Error(`Voice runtime fixture contract missing: ${name}`);
  }
  if (!(service.consumers instanceof Map) || service.avSyncSamplingTimer !== null ||
      !Number.isSafeInteger(service.avSyncObservationCount) || service.avSyncObservationCount < 0 ||
      typeof service.avSyncDriftEmitted !== 'boolean') {
    throw new Error('Voice runtime fixture contract missing: consumer/sampler state');
  }
}

/** Partial failure evidence must never retain raw stats, identities, keys or SDP. */
export function summarizeRuntimeWitnesses(witnesses) {
  const counter = (value) => Number.isSafeInteger(value) && value >= 0 ? value : null;
  const boolean = (value) => typeof value === 'boolean' ? value : null;
  const state = (value, allowed) => allowed.includes(value) ? value : 'unknown';
  const transportStates = ['new', 'connecting', 'connected', 'disconnected', 'failed', 'closed'];
  const iceStates = ['new', 'gathering', 'complete'];
  const states = (value, allowed) => Object.fromEntries(['send', 'audioReceive', 'videoReceive']
    .map((name) => [name, state(value?.[name], allowed)]));
  if (!Array.isArray(witnesses)) return [];
  return witnesses.slice(0, 2).map((entry) => {
    const value = entry ?? {};
    return {
      transportStates: states(value.transportStates, transportStates),
      iceGatheringStates: states(value.iceGatheringStates, iceStates),
      audioContextState: state(value.audioContextState, ['running', 'suspended', 'closed', 'interrupted']),
      audioSignalPresent: boolean(value.audioSignalPresent), decodedVideoPresent: boolean(value.decodedVideoPresent),
      audioPacketsReceived: counter(value.audioPacketsReceived), videoPacketsReceived: counter(value.videoPacketsReceived),
      videoBytesReceived: counter(value.videoBytesReceived), framesDecoded: counter(value.framesDecoded),
      presentedFrames: counter(value.presentedFrames), outboundPacketsSent: counter(value.outboundPacketsSent),
      outboundVideoFramesEncoded: counter(value.outboundVideoFramesEncoded),
      e2eeEntered: Array.isArray(value.e2eeEntered) ? value.e2eeEntered.slice(0, 2).map(counter) : null,
      sourceTracksLive: boolean(value.sourceTracksLive),
    };
  });
}

export function requireDuration(duration) {
  if (!Number.isSafeInteger(duration) || duration < MIN_DURATION_MS || duration > 180_000) {
    throw new Error('MEDIA_PROBE_DURATION_MS must be 95000..180000');
  }
  return duration;
}

/** Compare actual decode/presentation counters between bounded runtime polls. */
export function createVideoProgressGate() {
  let previous;
  return (witness) => {
    const finite = (value) => typeof value === 'number' && Number.isFinite(value) && value >= 0;
    if (typeof witness.videoConsumerId !== 'string' || !witness.videoConsumerId ||
        ![witness.framesDecoded, witness.presentedFrames, witness.decodedProgressAgeMs,
          witness.presentedProgressAgeMs].every(finite)) return false;
    const advancing = previous?.videoConsumerId === witness.videoConsumerId &&
      witness.framesDecoded > previous.framesDecoded && witness.presentedFrames > previous.presentedFrames &&
      witness.decodedProgressAgeMs <= VIDEO_STALL_MS && witness.presentedProgressAgeMs <= VIDEO_STALL_MS;
    previous = {
      videoConsumerId: witness.videoConsumerId,
      framesDecoded: witness.framesDecoded, presentedFrames: witness.presentedFrames,
    };
    return Boolean(advancing);
  };
}

export function verifiedConsumerId(payload) {
  return payload && typeof payload.consumerId === 'string' && payload.consumerId &&
    Number.isFinite(payload.entered) && payload.entered > 0 &&
    Number.isFinite(payload.packetsReceived) && payload.packetsReceived > 0
    ? payload.consumerId : null;
}

export function initialConsumersVerified(witness, verifiedIds) {
  const evidence = witness.consumerEvidence;
  return Array.isArray(evidence) && evidence.length === 2 &&
    evidence.some((entry) => entry.kind === 'audio') && evidence.some((entry) => entry.kind === 'video') &&
    evidence.every((entry) => typeof entry.id === 'string' && verifiedIds.has(entry.id) && entry.entered > 0);
}

export function replacementVerified(witness, previousId, verifiedIds, originalAudioId) {
  return typeof witness.videoConsumerId === 'string' && witness.videoConsumerId !== previousId &&
    verifiedIds.has(witness.videoConsumerId) && witness.e2eeVideoEntered > 0 && witness.audioSignalPresent &&
    witness.consumerEvidence.some((entry) => entry.kind === 'audio' && entry.id === originalAudioId && entry.entered > 0);
}

export function requireNoRuntimeFailure(markers) {
  if (markers.transformFailure || markers.priorityUpdateFailure || markers.pageErrors !== 0) {
    throw new Error('Runtime media error');
  }
}

/** Manifest text, root lock declaration, resolved lock, package and executable must agree. */
export function requireElectronAlignment({ manifest, lockManifest, locked, installed, runtime }, satisfies) {
  if (typeof manifest !== 'string' || manifest !== lockManifest ||
      typeof locked !== 'string' || !satisfies(locked, manifest) || installed !== locked ||
      (runtime !== undefined && runtime !== locked)) {
    throw new Error('Electron manifest/lock/installed/runtime mismatch');
  }
}
