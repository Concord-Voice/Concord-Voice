/**
 * Legacy E2EE decrypt pipeline extracted from VoiceService to reduce
 * cognitive complexity. Used on Chromium 86-130 (createEncodedStreams path).
 *
 * The modern RTCRtpScriptTransform path stays inline in VoiceService
 * because it's a 6-line branch with no stateful logic.
 */

import type { MediaEncryption, FrameKeyMissError } from './mediaEncryption';
import { errorMessage } from '../utils/redactError';
import type { CodecFamily } from '../workers/e2eeProtocol';

/**
 * #1895/#1878: discriminate the typed decrypt miss by error `name` (not
 * `instanceof`) — identical to the Worker's check (e2eeWorker.ts). Name-based
 * matching is immune to cross-realm/duplicate-class pitfalls and lets unit
 * tests mock `mediaEncryption` without re-exporting the class.
 */
function isFrameKeyMiss(err: unknown): err is FrameKeyMissError {
  return err instanceof Error && err.name === 'FrameKeyMissError';
}

/** Min gap between on-demand requests for the same key (mirrors e2eeWorker.ts). */
const FRAME_KEY_BACKOFF_MS = 350;
/** Requests per burst before pausing (mirrors e2eeWorker.ts). */
const FRAME_KEY_BURST_CAP = 8;
/** Idle gap after which a burst resets, so a slow-published key still recovers. */
const FRAME_KEY_RETRY_RESET_MS = 15_000;
/** Global size cap per pipeline (DoS bound — mirrors e2eeWorker.ts). */
const FRAME_KEY_MAX_TRACKED = 512;

/**
 * Decide whether to issue an on-demand key request for a typed miss, bounding
 * a persistent miss to bursts instead of one fetch per frame. Same policy as
 * the Worker's requestFrameKeyOnce: burst, pause, reset after an idle gap — so
 * a legitimately slow-published key (pending-404) still recovers, while a
 * permanent 403 settles to roughly one request per reset window.
 *
 * Two properties this shape must hold, both mirroring the Worker:
 *   (1) An idle-reset DELETES the stale entry rather than overwriting it on the
 *       next recurrence. Overwrite-only leaves an entry per distinct
 *       (sender, keyVersion, keyId) alive for the whole session.
 *   (2) The map is size-capped with least-recently-TOUCHED eviction, so a
 *       sender stamping a unique (keyVersion, keyId) per frame cannot grow it
 *       unboundedly. The LRU touch requires delete-then-set: a plain `set` on
 *       an existing key does NOT move it in Map insertion order, which would
 *       silently make eviction FIFO-oldest-inserted and evict the key being
 *       actively requested.
 *
 * Exported for unit testing of the retry policy; the pipeline uses it via the
 * closure-local map.
 */
export function shouldRequestFrameKey(
  requests: Map<string, { lastAttempt: number; attempts: number }>,
  miss: Pick<FrameKeyMissError, 'senderUserId' | 'keyVersion' | 'keyId'>
): boolean {
  const key = `${miss.senderUserId}:${miss.keyVersion}:${miss.keyId}`;
  const now = Date.now();
  let state = requests.get(key);

  if (state && now - state.lastAttempt >= FRAME_KEY_RETRY_RESET_MS) {
    requests.delete(key);
    state = undefined;
  }

  if (state) {
    if (state.attempts >= FRAME_KEY_BURST_CAP) return false;
    if (now - state.lastAttempt < FRAME_KEY_BACKOFF_MS) return false;
    requests.delete(key);
  } else if (requests.size >= FRAME_KEY_MAX_TRACKED) {
    const oldest = requests.keys().next().value;
    if (oldest !== undefined) requests.delete(oldest);
  }

  requests.set(key, { lastAttempt: now, attempts: (state?.attempts ?? 0) + 1 });
  return true;
}

// ─── Types ───────────────────────────────────────────────────────────

/** Minimal interface for an RTP receiver that supports the legacy Insertable Streams API. */
export interface InsertableStreamsReceiver {
  createEncodedStreams?: () => { readable: ReadableStream; writable: WritableStream };
}

/** Callbacks the VoiceService provides for self-healing key recovery. */
export interface DecryptRecoveryCallbacks {
  getActiveChannelId: () => string | null;
  addDecryptKeyForUser: (channelId: string, userId: string) => Promise<boolean>;
  invalidateChannelKey: (channelId: string) => void;
  requestKeyframe: (senderUserId: string) => void;
  /**
   * #1895: provision the exact key for a typed FrameKeyMiss on demand (mirrors
   * the Worker path's requestFrameKeyOnce). Optional + fail-safe: if a caller
   * omits it the pipeline still drops the frame (fail-closed) — it just can't
   * self-provision the missing CSK version. VoiceService always supplies it.
   */
  requestFrameKey?: (senderUserId: string, keyVersion: number, keyId: number) => void;
}

// ─── Constants ───────────────────────────────────────────────────────

const RECOVERY_COOLDOWN_MS = 5000;
const RECOVERY_THRESHOLD = 50;
const PERSISTENT_FAILURE_THRESHOLD = 500;

// ─── Decrypt Error Logging ───────────────────────────────────────────

/** Log a decrypt failure without exposing IVs or encrypted trailer bytes. */
function logDecryptFailure(
  senderUserId: string,
  dropCount: number,
  frame: { data: ArrayBuffer },
  decryptErr: unknown,
  encryption: MediaEncryption,
  verbose: boolean
): void {
  if (dropCount !== 1 && dropCount % 100 !== 0) return;

  const errMsg =
    decryptErr instanceof Error ? `${decryptErr.name}: ${decryptErr.message}` : String(decryptErr);

  if (verbose) {
    const fd = new Uint8Array(frame.data);
    const hasMagic = fd.length >= 2 && fd.at(-2) === 0xde && fd.at(-1) === 0xad;
    console.warn('E2EE: dropping frame for sender (dropped count):', senderUserId, dropCount, {
      error: errMsg,
      frameSize: fd.length,
      localEpoch: encryption.getCurrentKeyId(),
      hasMagic,
    });
  } else {
    console.warn(
      'E2EE: dropping undecryptable frame for sender (dropped count):',
      senderUserId,
      dropCount,
      {
        error: errMsg,
        frameSize: frame.data.byteLength,
      }
    );
  }
}

// ─── Self-Healing Recovery ───────────────────────────────────────────

/** Attempt key re-derivation when decryption fails persistently. */
function attemptSelfHealingRecovery(
  senderUserId: string,
  callbacks: DecryptRecoveryCallbacks,
  state: { recoveryInProgress: boolean; lastRecoveryAttempt: number }
): void {
  if (state.recoveryInProgress) return;
  if (Date.now() - state.lastRecoveryAttempt <= RECOVERY_COOLDOWN_MS) return;

  state.recoveryInProgress = true;
  state.lastRecoveryAttempt = Date.now();

  const channelId = callbacks.getActiveChannelId();
  if (!channelId) {
    state.recoveryInProgress = false;
    return;
  }

  console.debug(`E2EE: attempting self-healing key recovery for ${senderUserId}`);
  callbacks.invalidateChannelKey(channelId);
  callbacks
    .addDecryptKeyForUser(channelId, senderUserId)
    .then((ok) => {
      if (ok) {
        console.debug(`E2EE: recovery key re-derived for ${senderUserId}`);
      }
    })
    .catch(() => {})
    .finally(() => {
      state.recoveryInProgress = false;
    });
}

// ─── Public API ──────────────────────────────────────────────────────

/**
 * Handle a typed key miss on the legacy decrypt path: make it observable and
 * request the exact key within the budget. Returns the updated miss count.
 *
 * Extracted from the transform closure rather than inlined because the closure
 * already carries the success, first-frame, recovery and generic-drop paths;
 * adding this branch to it pushed its cognitive complexity past the S3776
 * threshold. Behaviour is unchanged — the caller still returns immediately, so
 * the frame stays fail-closed and never reaches the decoder.
 *
 * #1895: the typed miss is NOT counted toward the generic drop/self-heal
 * counter — that recovery is version-blind (the #1878/#1885 residual this
 * fixes); only the wrong-base OperationError case needs it.
 *
 * Parity with the Worker ([internal]rules/e2ee.md — "Both transform APIs are
 * load-bearing"): this branch was silent on both paths, so a receiver missing
 * every key logged nothing at all on either.
 */
function handleTypedKeyMiss(
  senderUserId: string,
  miss: FrameKeyMissError,
  missCount: number,
  missRequests: Map<string, { lastAttempt: number; attempts: number }>,
  callbacks: DecryptRecoveryCallbacks
): number {
  const next = missCount + 1;
  if (next === 1 || next % 100 === 0) {
    console.warn(`E2EE: no key for ${senderUserId} frame — dropped (misses: ${next})`, {
      wantKeyVersion: miss.keyVersion,
      wantKeyId: miss.keyId,
    });
  }
  if (shouldRequestFrameKey(missRequests, miss)) {
    callbacks.requestFrameKey?.(miss.senderUserId, miss.keyVersion, miss.keyId);
  }
  return next;
}

/**
 * Create and pipe a legacy decrypt TransformStream on a consumer's
 * createEncodedStreams API.
 *
 * This is the main-thread path for Chromium 86-130. It decrypts each
 * frame, drops undecryptable frames with progressive logging, and
 * triggers self-healing key recovery after RECOVERY_THRESHOLD drops.
 */
export function applyLegacyDecryptPipeline(
  receiver: InsertableStreamsReceiver,
  senderUserId: string,
  encryption: MediaEncryption,
  callbacks: DecryptRecoveryCallbacks,
  verbose: boolean,
  codecFamily?: CodecFamily // #1895: SENDER's codec — drives per-codec decrypt dispatch
): void {
  if (typeof receiver.createEncodedStreams !== 'function') {
    const message = 'E2EE: no Insertable Streams API available — frames will not be decrypted';
    console.warn(message);
    throw new Error(message);
  }

  try {
    const { readable, writable } = receiver.createEncodedStreams();
    let dropCount = 0;
    let missCount = 0;
    let firstDecryptLogged = false;
    const recoveryState = { recoveryInProgress: false, lastRecoveryAttempt: 0 };
    // Per-pipeline request budget for typed key misses, mirroring the Worker's
    // frameKeyRequests policy (e2eeWorker.ts). Kept local rather than shared:
    // one pipeline is already scoped to one sender, so the closure IS the right
    // scope and no module-global cap is needed. Without it a persistent miss
    // calls getChannelKeyByVersion once per frame (~50/s for audio).
    const missRequests = new Map<string, { lastAttempt: number; attempts: number }>();

    const transform = new TransformStream({
      transform: async (frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame, controller) => {
        try {
          await encryption.decryptFrame(frame, senderUserId, codecFamily);
          controller.enqueue(frame);

          if (!firstDecryptLogged) {
            firstDecryptLogged = true;
            console.debug(`E2EE: first frame decrypted for ${senderUserId}`);
          }
          if (dropCount > 0 || missCount > 0) {
            console.debug(
              `E2EE: decrypt recovered for ${senderUserId} (${dropCount} drops, ${missCount} key misses)`
            );
            missCount = 0;
            // Deliberately NOT missRequests.clear(). One decryptable frame does
            // not mean every tracked key became available: if decodable frames
            // interleave with misses for a still-unresolved key, a wholesale
            // clear makes each miss a fresh first request and restores
            // frame-rate fetching — defeating the burst cap this map exists to
            // enforce. Entries age out per key via the idle reset, or are
            // evicted by the size cap. The Worker does the same: it clears one
            // key on arrival (clearFrameKeyRequest) and never the whole map.
            if ('type' in frame) {
              callbacks.requestKeyframe(senderUserId);
            }
            dropCount = 0;
          }
        } catch (decryptErr) {
          if (isFrameKeyMiss(decryptErr)) {
            missCount = handleTypedKeyMiss(
              senderUserId,
              decryptErr,
              missCount,
              missRequests,
              callbacks
            );
            return;
          }
          dropCount++;
          logDecryptFailure(senderUserId, dropCount, frame, decryptErr, encryption, verbose);

          if (dropCount === RECOVERY_THRESHOLD) {
            attemptSelfHealingRecovery(senderUserId, callbacks, recoveryState);
          }

          if (dropCount === PERSISTENT_FAILURE_THRESHOLD) {
            console.error(
              `E2EE: persistent decrypt failure for ${senderUserId} — ${PERSISTENT_FAILURE_THRESHOLD} frames dropped. Rejoin may be required.`
            );
          }
        }
      },
    });

    readable
      .pipeThrough(transform)
      .pipeTo(writable)
      .catch((err: unknown) => {
        console.warn('E2EE decrypt pipe error:', errorMessage(err));
      });

    console.debug(`E2EE: decrypt transform applied for ${senderUserId} (createEncodedStreams)`);
  } catch (err) {
    console.error('E2EE: createEncodedStreams failed on receiver:', errorMessage(err));
    throw new Error(`E2EE: createEncodedStreams failed on receiver: ${errorMessage(err)}`);
  }
}
