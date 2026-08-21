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
