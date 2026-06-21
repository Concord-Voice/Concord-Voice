/**
 * E2EE Web Worker — handles frame encryption/decryption via RTCRtpScriptTransform.
 *
 * This Worker owns a MediaEncryption instance and processes all voice/video
 * frames for a single voice channel session. The main thread derives keys
 * (via e2eeService + HKDF) and sends CryptoKey objects here via postMessage.
 *
 * Each call to `new RTCRtpScriptTransform(worker, options)` on the main thread
 * triggers an `rtctransform` event here with { readable, writable } streams.
 */

import { MediaEncryption } from '../services/mediaEncryption';
import type { E2EEWorkerMessage, E2EEMainMessage, E2EETransformOptions } from './e2eeProtocol';

const encryption = new MediaEncryption();

// Thresholds for self-healing (mirrors voiceService legacy path)
const RECOVERY_THRESHOLD = 50;
const PERSISTENT_FAILURE_THRESHOLD = 500;
const RECOVERY_COOLDOWN_MS = 5000;

// Per-sender recovery state
const recoveryState = new Map<string, { lastAttempt: number; inProgress: boolean }>();

function postToMain(msg: E2EEMainMessage): void {
  self.postMessage(msg);
}

function log(
  level: 'debug' | 'info' | 'warn' | 'error',
  message: string,
  data?: Record<string, unknown>
): void {
  postToMain({ type: 'log', level, message, data });
}

// ─── Message handler (key management from main thread) ────────────────

self.onmessage = (event: MessageEvent<E2EEWorkerMessage>) => {
  const msg = event.data;
  switch (msg.type) {
    case 'init':
      encryption.initFromKey(msg.encryptKey, msg.currentKeyId);
      log('debug', 'E2EE Worker: initialized', { keyId: msg.currentKeyId });
      break;

    case 'addDecryptKey':
      encryption.addDecryptKeyDirect(msg.senderUserId, msg.keyId, msg.key);
      log('debug', 'E2EE Worker: decrypt key added', {
        senderUserId: msg.senderUserId,
        keyId: msg.keyId,
      });
      break;

    case 'rotateKeys':
      encryption
        .rotateKeys()
        .then(() => {
          postToMain({ type: 'rotationComplete', newKeyId: encryption.getCurrentKeyId() });
        })
        .catch((err) => {
          log('error', 'E2EE Worker: rotateKeys failed', {
            error: err instanceof Error ? err.message : String(err),
          });
        });
      break;

    case 'catchUpToEpoch':
      encryption
        .catchUpToEpoch(msg.targetEpoch)
        .then(() => {
          postToMain({ type: 'rotationComplete', newKeyId: encryption.getCurrentKeyId() });
        })
        .catch((err) => {
          log('error', 'E2EE Worker: catchUpToEpoch failed', {
            error: err instanceof Error ? err.message : String(err),
          });
        });
      break;

    case 'destroy':
      encryption.destroy();
      recoveryState.clear();
      log('debug', 'E2EE Worker: destroyed');
      break;
  }
};

// ─── RTCRtpScriptTransform handler (frame processing) ─────────────────
// Use addEventListener — the onrtctransform IDL attribute is not supported
// in all Chromium/Electron Worker contexts, but the event itself dispatches.

self.addEventListener('rtctransform', ((event: RTCTransformEvent) => {
  const transformer = event.transformer;
  const opts = transformer.options as E2EETransformOptions;

  log('debug', `E2EE: rtctransform event fired`, { role: opts.role });

  if (opts.role === 'encrypt') {
    handleEncrypt(transformer.readable, transformer.writable);
  } else if (opts.role === 'decrypt') {
    if (!opts.senderUserId) {
      log('error', 'E2EE: decrypt transform missing senderUserId');
      return;
    }
    handleDecrypt(transformer.readable, transformer.writable, opts.senderUserId);
  } else {
    log('error', 'E2EE: unknown transform role', { role: String(opts.role) });
  }
}) as EventListener);

// ─── Encrypt pipeline ─────────────────────────────────────────────────

function handleEncrypt(
  readable: ReadableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>,
  writable: WritableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>
): void {
  let dropCount = 0;
  let firstLogged = false;

  const transform = new TransformStream<
    RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    RTCEncodedAudioFrame | RTCEncodedVideoFrame
  >({
    async transform(frame, controller) {
      try {
        await encryption.encryptFrame(frame);
        controller.enqueue(frame);
        if (!firstLogged) {
          firstLogged = true;
          log('debug', 'E2EE: first frame encrypted', {
            kind: 'type' in frame ? 'video' : 'audio',
            dataSize: frame.data.byteLength,
          });
        }
        if (dropCount > 0) {
          log('debug', `E2EE: encrypt recovered after ${dropCount} drops`);
          dropCount = 0;
        }
      } catch (err) {
        dropCount++;
        if (dropCount === 1 || dropCount % 100 === 0) {
          log('warn', 'E2EE: encrypt frame dropped', {
            totalDropped: dropCount,
            error: err instanceof Error ? err.message : String(err),
          });
        }
      }
    },
  });

  readable
    .pipeThrough(transform)
    .pipeTo(writable)
    .catch((err) => {
      log('error', 'E2EE: encrypt pipe broken', {
        error: err instanceof Error ? err.message : String(err),
      });
    });
}

// ─── Decrypt pipeline ─────────────────────────────────────────────────

/** Attempt self-healing recovery when decrypt drops hit the threshold */
function attemptRecovery(senderUserId: string, dropCount: number): void {
  const state = recoveryState.get(senderUserId) || { lastAttempt: 0, inProgress: false };
  if (state.inProgress || Date.now() - state.lastAttempt <= RECOVERY_COOLDOWN_MS) return;

  state.inProgress = true;
  state.lastAttempt = Date.now();
  recoveryState.set(senderUserId, state);
  log('debug', `E2EE: requesting recovery for ${senderUserId}`);
  postToMain({ type: 'requestRecovery', senderUserId, dropCount });
  setTimeout(() => {
    state.inProgress = false;
  }, RECOVERY_COOLDOWN_MS);
}

/** Handle a decryption failure: log, attempt recovery at threshold, flag persistent failures */
function handleDecryptError(
  senderUserId: string,
  dropCount: number,
  decryptErr: unknown,
  frameSize: number
): void {
  if (dropCount === 1 || dropCount % 100 === 0) {
    log('warn', `E2EE: dropping frame for ${senderUserId} (dropped: ${dropCount})`, {
      error:
        decryptErr instanceof Error
          ? `${decryptErr.name}: ${decryptErr.message}`
          : String(decryptErr),
      frameSize,
    });
  }

  if (dropCount === RECOVERY_THRESHOLD) {
    attemptRecovery(senderUserId, dropCount);
  }

  if (dropCount === PERSISTENT_FAILURE_THRESHOLD) {
    log(
      'error',
      `E2EE: persistent failure for ${senderUserId} — ${PERSISTENT_FAILURE_THRESHOLD} drops, rejoin may be required`
    );
  }
}

function handleDecrypt(
  readable: ReadableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>,
  writable: WritableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>,
  senderUserId: string
): void {
  let dropCount = 0;
  let firstLogged = false;

  const transform = new TransformStream<
    RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    RTCEncodedAudioFrame | RTCEncodedVideoFrame
  >({
    async transform(frame, controller) {
      try {
        await encryption.decryptFrame(frame, senderUserId);
        controller.enqueue(frame);
        if (!firstLogged) {
          firstLogged = true;
          log('debug', `E2EE: first frame decrypted for ${senderUserId}`);
        }
        if (dropCount > 0) {
          log('info', `E2EE: decrypt recovered after ${dropCount} drops for ${senderUserId}`);
          dropCount = 0;
        }
      } catch (decryptErr) {
        dropCount++;
        handleDecryptError(senderUserId, dropCount, decryptErr, frame.data.byteLength);
      }
    },
  });

  readable
    .pipeThrough(transform)
    .pipeTo(writable)
    .catch((err) => {
      log('warn', `E2EE: decrypt pipe error for ${senderUserId}`, {
        error: err instanceof Error ? err.message : String(err),
      });
    });
}
