/**
 * Receive-transform bypass probe — decision logic.
 *
 * 2026-08-21 incident: on both Chrome 149 and Electron 43, receivers with an
 * attached RTCRtpScriptTransform decoded arriving RTP as ciphertext while the
 * transform processed ZERO frames — loud garbled audio (decoded ciphertext at
 * ~full scale, PLC-extended across DTX gaps) and black video. Every JS-visible
 * knob was correct: transform attached before media (server-paused consumer),
 * clean PC config, worker alive and encrypting. The divergence is only
 * observable by pairing decoder-side getStats() with a worker-side
 * entered-frame count — which is what this probe does.
 *
 * Pure function so the policy is testable without a VoiceService instance.
 */

import type { CodecFamily, E2EETransformOptions } from '../workers/e2eeProtocol';

/**
 * Build the ConsumerOptions.onRtpReceiver callback that attaches the decrypt
 * transform AT RECEIVER CREATION — after setRemoteDescription, before
 * createAnswer, before any media routing exists. Chromium ≥149 (encoded
 * transform V2 line) does not route frames through a transform attached after
 * the receiver is live: the pipe stays empty and ciphertext reaches the
 * decoder (2026-08-21 field capture, PR #2865). Creation-time attachment
 * mirrors the sender-side onRtpSender hook, which works on the same engines.
 *
 * Reads the RTCRtpScriptTransform constructor from globalThis at CALL time so
 * the caller's path decision (script-transform vs legacy) stays the single
 * gate and tests can stub the global.
 */
export function buildDecryptCreationAttach(
  worker: Worker,
  senderUserId: string,
  codecFamily: CodecFamily | undefined,
  probeId: string
): (receiver: RTCRtpReceiver) => void {
  return (receiver) => {
    const options: E2EETransformOptions = {
      role: 'decrypt',
      senderUserId,
      codecFamily,
      probeId,
    };
    receiver.transform = new RTCRtpScriptTransform(worker, options);
    console.debug(
      `E2EE: decrypt transform applied for ${senderUserId} (RTCRtpScriptTransform, at receiver creation)`
    );
  };
}

/** Delay after attach (and between retries) before pairing the two counters. */
export const BYPASS_PROBE_DELAY_MS = 5_000;
/** Fewer received packets than this is inconclusive (paused/silent producer). */
export const BYPASS_PROBE_MIN_PACKETS = 10;
/** Inconclusive probes retry this many times at the fast cadence… */
export const BYPASS_PROBE_MAX_ATTEMPTS = 3;
/** …then drop to this slow poll for the consumer's lifetime. A producer that
 * joins muted / camera-off can start sending minutes later; giving up
 * permanently would forfeit bypass detection for that whole session
 * (Gitar finding, PR #2865). The chain dies with the consumer. */
export const BYPASS_PROBE_SLOW_DELAY_MS = 30_000;

export type BypassProbePhase = 'first' | 'reattached';

export type BypassProbeAction =
  | 'verified' // frames are entering the transform — healthy
  | 'retry' // not enough packets to judge yet — probe again soon
  | 'slow-retry' // still no packets — keep polling slowly while the consumer lives
  | 'reattach' // bypass confirmed once — try re-attaching the transform
  | 'close'; // bypass confirmed after re-attach — fail closed

export function decideBypassProbeAction(
  packetsReceived: number,
  entered: number,
  phase: BypassProbePhase,
  attempt: number
): BypassProbeAction {
  if (entered > 0) return 'verified';
  if (packetsReceived < BYPASS_PROBE_MIN_PACKETS) {
    // A paused/silent producer (join muted, camera off) can start sending at
    // any point — never stop watching, just slow down.
    return attempt < BYPASS_PROBE_MAX_ATTEMPTS ? 'retry' : 'slow-retry';
  }
  return phase === 'first' ? 'reattach' : 'close';
}
