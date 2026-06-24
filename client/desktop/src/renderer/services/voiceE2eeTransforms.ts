/**
 * Legacy E2EE decrypt pipeline extracted from VoiceService to reduce
 * cognitive complexity. Used on Chromium 86-130 (createEncodedStreams path).
 *
 * The modern RTCRtpScriptTransform path stays inline in VoiceService
 * because it's a 6-line branch with no stateful logic.
 */

import type { MediaEncryption } from './mediaEncryption';
import { errorMessage } from '../utils/redactError';

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
}

// ─── Constants ───────────────────────────────────────────────────────

const RECOVERY_COOLDOWN_MS = 5000;
const RECOVERY_THRESHOLD = 50;
const PERSISTENT_FAILURE_THRESHOLD = 500;

// ─── Decrypt Error Logging ───────────────────────────────────────────

/** Log a decrypt failure — verbose mode includes frame trailer hex dump. */
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
    const trailerHex =
      fd.length >= 16
        ? Array.from(fd.slice(-16))
            .map((b) => b.toString(16).padStart(2, '0'))
            .join(' ')
        : 'too-small';
    const hasMagic = fd.length >= 2 && fd.at(-2) === 0xde && fd.at(-1) === 0xad;
    console.warn('E2EE: dropping frame for sender (dropped count):', senderUserId, dropCount, {
      error: errMsg,
      frameSize: fd.length,
      localEpoch: encryption.getCurrentKeyId(),
      hasMagic,
      keyId: hasMagic && fd.length >= 15 ? fd.at(-15) : -1,
      trailerHex,
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
  verbose: boolean
): void {
  if (typeof receiver.createEncodedStreams !== 'function') {
    const message = 'E2EE: no Insertable Streams API available — frames will not be decrypted';
    console.warn(message);
    throw new Error(message);
  }

  try {
    const { readable, writable } = receiver.createEncodedStreams();
    let dropCount = 0;
    let firstDecryptLogged = false;
    const recoveryState = { recoveryInProgress: false, lastRecoveryAttempt: 0 };

    const transform = new TransformStream({
      transform: async (frame: RTCEncodedAudioFrame | RTCEncodedVideoFrame, controller) => {
        try {
          await encryption.decryptFrame(frame, senderUserId);
          controller.enqueue(frame);

          if (!firstDecryptLogged) {
            firstDecryptLogged = true;
            console.debug(`E2EE: first frame decrypted for ${senderUserId}`);
          }
          if (dropCount > 0) {
            console.debug(
              `E2EE: decrypt recovered after ${dropCount} dropped frames for ${senderUserId}`
            );
            if ('type' in frame) {
              callbacks.requestKeyframe(senderUserId);
            }
            dropCount = 0;
          }
        } catch (decryptErr) {
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
