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

import { MediaEncryption, type FrameKeyMissError } from '../services/mediaEncryption';
import type {
  CodecFamily,
  E2EEWorkerMessage,
  E2EEMainMessage,
  E2EETransformOptions,
} from './e2eeProtocol';

/**
 * #1878: discriminate the typed decrypt miss by error `name`, not `instanceof`.
 * The class is type-imported (no runtime value) so unit tests that fully mock
 * `mediaEncryption` need not re-export it, and the check is immune to the
 * cross-realm `instanceof` pitfall for errors crossing a Worker boundary. The
 * real `FrameKeyMissError` sets `name = 'FrameKeyMissError'` (asserted in
 * mediaEncryption.test.ts), so this is behavior-equivalent.
 */
function isFrameKeyMiss(err: unknown): err is FrameKeyMissError {
  return err instanceof Error && err.name === 'FrameKeyMissError';
}

const encryption = new MediaEncryption();

// Thresholds for self-healing (mirrors voiceService legacy path)
const RECOVERY_THRESHOLD = 50;
const PERSISTENT_FAILURE_THRESHOLD = 500;
const RECOVERY_COOLDOWN_MS = 5000;

// Per-sender recovery state
const recoveryState = new Map<string, { lastAttempt: number; inProgress: boolean }>();

// #1878: typed-miss on-demand key requests. A FrameKeyMissError means the
// receiver has no key for the frame's exact (sender, keyVersion, keyId), so we
// ask main to fetch + derive it. Dedup'd per key with a short backoff (the key
// may not be published yet — pending-404), capped so a permanent 403 (not a
// member) can't loop forever. The success case is handled by the existing
// keyframe-on-recovery once the key arrives and a frame decrypts.
// #1885: one consolidated tracking map keyed (sender:keyVersion:keyId) →
// { lastAttempt, attempts }. Two correctness properties this shape must hold:
//   (1) Gitar #3 — a legitimately slow-published key (pending-404 where the CSK
//       re-wrap takes longer than the burst window, e.g. 0–2s+ under load) must
//       NOT be permanently blacked. We retry in bursts (FRAME_KEY_BURST_CAP at
//       FRAME_KEY_BACKOFF_MS), then PAUSE; after FRAME_KEY_RETRY_RESET_MS of idle
//       the burst resets so requests resume — recovery latency is bounded to the
//       reset window, never infinite. (A true permanent 403 just re-requests at
//       the low ~1/reset-window rate; the main-thread rateLimitedUntil/429 path
//       throttles server amplification.)
//   (2) security-reviewer LOW — the map is globally size-capped so a misbehaving
//       in-room sender stamping a unique (keyVersion,keyId) per frame cannot grow
//       it unboundedly; at capacity we evict the least-recently-TOUCHED entry
//       (LRU — touches re-insert at the end), so an actively-requested legit key
//       is never the eviction victim during a churn attack (Gitar #1885).
// Exported for unit testing of the retry policy (burst cap / idle reset / size
// cap). Not consumed by the Worker runtime, which only uses the side effects.
export const frameKeyRequests = new Map<string, { lastAttempt: number; attempts: number }>();
const FRAME_KEY_BACKOFF_MS = 350; // min gap between requests for the same key
const FRAME_KEY_BURST_CAP = 8; // requests per burst before pausing
const FRAME_KEY_RETRY_RESET_MS = 15_000; // idle gap after which a burst resets (slow-key recovery)
const FRAME_KEY_MAX_TRACKED = 512; // global size cap (DoS bound)

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
      // #1878: bind the encrypt key's channel-key version so outgoing frames
      // stamp the authoritative CSK version (not a stale 0).
      encryption.setKeyVersion(msg.keyVersion);
      log('debug', 'E2EE Worker: initialized', {
        keyId: msg.currentKeyId,
        keyVersion: msg.keyVersion,
      });
      break;

    case 'addDecryptKey':
      // #1878: keyed by (senderUserId, keyVersion, keyId) to match the v3 frame
      // trailer. A successful add also clears any pending requestFrameKey backoff
      // so a later miss (e.g., a fresh keyVersion) re-requests promptly.
      encryption.addDecryptKeyDirectV3(msg.senderUserId, msg.keyVersion, msg.keyId, msg.key);
      clearFrameKeyRequest(msg.senderUserId, msg.keyVersion, msg.keyId);
      log('debug', 'E2EE Worker: decrypt key added', {
        senderUserId: msg.senderUserId,
        keyVersion: msg.keyVersion,
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
      frameKeyRequests.clear();
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
    handleEncrypt(transformer.readable, transformer.writable, opts.codecFamily);
  } else if (opts.role === 'decrypt') {
    if (!opts.senderUserId) {
      log('error', 'E2EE: decrypt transform missing senderUserId');
      return;
    }
    handleDecrypt(transformer.readable, transformer.writable, opts.senderUserId, opts.codecFamily);
  } else {
    log('error', 'E2EE: unknown transform role', { role: String(opts.role) });
  }
}) as EventListener);

// ─── Encrypt pipeline ─────────────────────────────────────────────────

function handleEncrypt(
  readable: ReadableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>,
  writable: WritableStream<RTCEncodedAudioFrame | RTCEncodedVideoFrame>,
  codecFamily: CodecFamily | undefined
): void {
  let dropCount = 0;
  let firstLogged = false;

  const transform = new TransformStream<
    RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    RTCEncodedAudioFrame | RTCEncodedVideoFrame
  >({
    async transform(frame, controller) {
      try {
        await encryption.encryptFrame(frame, codecFamily);
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

/**
 * #1878: request the exact key for a typed decrypt miss. Dedup'd per
 * (sender, keyVersion, keyId) with a short backoff and a hard retry cap. Fail-
 * closed: while we wait, the frame is dropped (never enqueued as ciphertext).
 */
export function requestFrameKeyOnce(senderUserId: string, keyVersion: number, keyId: number): void {
  const k = `${senderUserId}:${keyVersion}:${keyId}`;
  const now = Date.now();
  let st = frameKeyRequests.get(k);

  // Idle-reset: a long gap since the last request means either a slow-published
  // key we paused on (resume so it can recover — Gitar #3) or a stale entry.
  if (st && now - st.lastAttempt >= FRAME_KEY_RETRY_RESET_MS) {
    frameKeyRequests.delete(k);
    st = undefined;
  }
  if (st) {
    if (st.attempts >= FRAME_KEY_BURST_CAP) return; // burst exhausted — wait for the reset window
    if (now - st.lastAttempt < FRAME_KEY_BACKOFF_MS) return; // rate-limit within the burst
    // LRU touch: delete so the re-set below moves this key to the most-recent
    // (last) position. That keeps the size-cap eviction least-recently-TOUCHED
    // rather than FIFO oldest-inserted, so an actively-requested legit key is
    // never the eviction victim while a sender churns unique keys (Gitar #1885).
    frameKeyRequests.delete(k);
  } else if (frameKeyRequests.size >= FRAME_KEY_MAX_TRACKED) {
    // Global size cap (DoS bound): evict the least-recently-touched entry (the
    // first key, since touches re-insert at the end) before adding the new one.
    const oldest = frameKeyRequests.keys().next().value;
    if (oldest !== undefined) frameKeyRequests.delete(oldest);
  }

  frameKeyRequests.set(k, { lastAttempt: now, attempts: (st?.attempts ?? 0) + 1 });
  postToMain({ type: 'requestFrameKey', senderUserId, keyVersion, keyId });
}

/** Clear the dedup/backoff state for a key once it has been provisioned. */
function clearFrameKeyRequest(senderUserId: string, keyVersion: number, keyId: number): void {
  frameKeyRequests.delete(`${senderUserId}:${keyVersion}:${keyId}`);
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
  senderUserId: string,
  codecFamily: CodecFamily | undefined
): void {
  let dropCount = 0;
  let firstLogged = false;

  const transform = new TransformStream<
    RTCEncodedAudioFrame | RTCEncodedVideoFrame,
    RTCEncodedAudioFrame | RTCEncodedVideoFrame
  >({
    async transform(frame, controller) {
      try {
        await encryption.decryptFrame(frame, senderUserId, codecFamily);
        controller.enqueue(frame);
        if (!firstLogged) {
          firstLogged = true;
          log('debug', `E2EE: first frame decrypted for ${senderUserId}`);
        }
        if (dropCount > 0) {
          log('info', `E2EE: decrypt recovered after ${dropCount} drops for ${senderUserId}`);
          if ('type' in frame) {
            postToMain({ type: 'requestKeyframe', senderUserId });
          }
          dropCount = 0;
        }
      } catch (decryptErr) {
        if (isFrameKeyMiss(decryptErr)) {
          // #1878: typed miss — request the exact key on demand. The frame is
          // dropped (fail-closed); it is never enqueued as ciphertext.
          requestFrameKeyOnce(decryptErr.senderUserId, decryptErr.keyVersion, decryptErr.keyId);
        } else {
          // OperationError (wrong-base GCM auth) / other — existing self-heal path.
          dropCount++;
          handleDecryptError(senderUserId, dropCount, decryptErr, frame.data.byteLength);
        }
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
